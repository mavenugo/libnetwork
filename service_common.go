// +build linux windows

package libnetwork

import (
	"net"

	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/common"
)

func newService(name string, id string, ingressPorts []*PortConfig, serviceAliases []string) *service {
	return &service{
		name:           name,
		id:             id,
		ingressPorts:   ingressPorts,
		loadBalancers:  make(map[string]*loadBalancer),
		serviceAliases: serviceAliases,
		ipToEndpoint:   common.NewSetMatrix(),
	}
}

func (c *controller) getLBIndex(sid, nid string, ingressPorts []*PortConfig) int {
	skey := serviceKey{
		id:    sid,
		ports: portConfigs(ingressPorts).String(),
	}
	c.Lock()
	s, ok := c.serviceBindings[skey]
	c.Unlock()

	if !ok {
		return 0
	}

	s.Lock()
	lb := s.loadBalancers[nid]
	s.Unlock()

	return int(lb.fwMark)
}

func (c *controller) cleanupServiceBindings(cleanupNID string) {
	var cleanupFuncs []func()

	c.Lock()
	services := make([]*service, 0, len(c.serviceBindings))
	for _, s := range c.serviceBindings {
		services = append(services, s)
	}
	c.Unlock()

	for _, s := range services {
		s.Lock()
		// Skip the serviceBindings that got deleted
		if s.deleted {
			s.Unlock()
			continue
		}
		for nid, lb := range s.loadBalancers {
			if cleanupNID != "" && nid != cleanupNID {
				continue
			}

			for eid, be := range lb.backEnds {
				service := s
				loadBalancer := lb
				networkID := nid
				epID := eid
				epIP := be.ip

				cleanupFuncs = append(cleanupFuncs, func() {
					if err := c.rmServiceBinding(service.name, service.id, networkID, epID, be.containerName, loadBalancer.vip,
						service.ingressPorts, service.serviceAliases, be.taskAliases, epIP, "cleanupServiceBindings"); err != nil {
						logrus.Errorf("Failed to remove service bindings for service %s network %s endpoint %s while cleanup: %v",
							service.id, networkID, epID, err)
					}
				})
			}
		}
		s.Unlock()
	}

	for _, f := range cleanupFuncs {
		f()
	}

}

func (c *controller) addEndpointNameResolution(svcName, svcID, nID, eID, containerName string, vip net.IP, serviceAliases, taskAliases []string, ip net.IP, addService bool, method string) error {
	n, err := c.NetworkByID(nID)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()

	// // Add resolution for endpoint name
	n.(*network).addSvcRecords(eID, containerName, ip, nil, true, method)

	// Add endpoint IP to special "tasks.svc_name" so that the applications have access to DNS RR.
	n.(*network).addSvcRecords(eID, "tasks."+svcName, ip, nil, false, method)
	for _, alias := range serviceAliases {
		n.(*network).addSvcRecords(eID, "tasks."+alias, ip, nil, false, method)
	}

	// Add resolution for taskaliases
	for _, alias := range taskAliases {
		n.(*network).addSvcRecords(eID, alias, ip, nil, true, method)
	}

	// Add service name to vip in DNS, if vip is valid. Otherwise resort to DNS RR
	if len(vip) == 0 {
		n.(*network).addSvcRecords(eID, svcName, ip, nil, false, method)
		for _, alias := range serviceAliases {
			n.(*network).addSvcRecords(eID, alias, ip, nil, false, method)
		}
	}

	if addService && len(vip) != 0 {
		n.(*network).addSvcRecords(eID, svcName, vip, nil, false, method)
		for _, alias := range serviceAliases {
			n.(*network).addSvcRecords(eID, alias, vip, nil, false, method)
		}
	}

	return nil
}

func (c *controller) deleteEndpointNameResolution(svcName, svcID, nID, eID, containerName string, vip net.IP, serviceAliases, taskAliases []string, ip net.IP, rmService, multipleEntries bool, method string) error {
	n, err := c.NetworkByID(nID)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()

	// // Delete resolution for endpoint name
	n.(*network).deleteSvcRecords(eID, containerName, ip, nil, true, method)

	// Delete the special "tasks.svc_name" backend record.
	if !multipleEntries {
		n.(*network).deleteSvcRecords(eID, "tasks."+svcName, ip, nil, false, method)
		for _, alias := range serviceAliases {
			n.(*network).deleteSvcRecords(eID, "tasks."+alias, ip, nil, false, method)
		}
	}

	// Delete resolution for taskaliases
	for _, alias := range taskAliases {
		n.(*network).deleteSvcRecords(eID, alias, ip, nil, true, method)
	}

	// If we are doing DNS RR delete the endpoint IP from DNS record right away.
	if !multipleEntries && len(vip) == 0 {
		n.(*network).deleteSvcRecords(eID, svcName, ip, nil, false, method)
		for _, alias := range serviceAliases {
			n.(*network).deleteSvcRecords(eID, alias, ip, nil, false, method)
		}
	}

	// Remove the DNS record for VIP only if we are removing the service
	if rmService && len(vip) != 0 && !multipleEntries {
		n.(*network).deleteSvcRecords(eID, svcName, vip, nil, false, method)
		for _, alias := range serviceAliases {
			n.(*network).deleteSvcRecords(eID, alias, vip, nil, false, method)
		}
	}

	return nil
}

func (c *controller) addServiceBinding(svcName, svcID, nID, eID, containerName string, vip net.IP, ingressPorts []*PortConfig, serviceAliases, taskAliases []string, ip net.IP, method string) error {
	var addService bool

	n, err := c.NetworkByID(nID)
	if err != nil {
		return err
	}

	skey := serviceKey{
		id:    svcID,
		ports: portConfigs(ingressPorts).String(),
	}

	var s *service
	for {
		c.Lock()
		var ok bool
		s, ok = c.serviceBindings[skey]
		if !ok {
			// Create a new service if we are seeing this service
			// for the first time.
			s = newService(svcName, svcID, ingressPorts, serviceAliases)
			c.serviceBindings[skey] = s
			logrus.Errorf("addServiceBinding created new serviceBindings %s %p", eID, s)
		}
		c.Unlock()
		s.Lock()
		if !s.deleted {
			// ok the object is good to be used
			break
		}
		s.Unlock()
	}
	logrus.Errorf("addServiceBinding from %s START for %s %s", method, svcName, eID)

	defer s.Unlock()

	lb, ok := s.loadBalancers[nID]
	if !ok {
		// Create a new load balancer if we are seeing this
		// network attachment on the service for the first
		// time.
		lb = &loadBalancer{
			vip:      vip,
			fwMark:   fwMarkCtr,
			backEnds: make(map[string]loadBalancerBackend),
			service:  s,
		}

		fwMarkCtrMu.Lock()
		fwMarkCtr++
		fwMarkCtrMu.Unlock()

		s.loadBalancers[nID] = lb
		addService = true
		logrus.Errorf("addServiceBinding created new lb %s s:%p lb:%p", eID, s, lb)
	}

	lb.backEnds[eID] = loadBalancerBackend{ip: ip,
		containerName: containerName,
		taskAliases:   taskAliases}
	logrus.Errorf("addServiceBinding lb.backEnds[eid] added %s ip %s", eID, ip.String())

	ok, entries := s.assignIPToEndpoint(ip.String(), eID)
	if !ok || entries > 1 {
		logrus.Errorf("addServiceBinding %s WEIRD ok:%t entries:%d", eID, ok, entries)
		s.printIPToEndpoint(ip.String(), "addServiceBinding "+eID)
	}

	// Add loadbalancer service and backend in all sandboxes in
	// the network only if vip is valid.
	if len(vip) != 0 {
		n.(*network).addLBBackend(ip, vip, lb.fwMark, ingressPorts)
	}

	// Add the appropriate name resolutions
	c.addEndpointNameResolution(svcName, svcID, nID, eID, containerName, vip, serviceAliases, taskAliases, ip, addService, "addServiceBinding")

	logrus.Errorf("addServiceBinding from %s END for %s %s", method, svcName, eID)

	return nil
}

func (c *controller) rmServiceBinding(svcName, svcID, nID, eID, containerName string, vip net.IP, ingressPorts []*PortConfig, serviceAliases []string, taskAliases []string, ip net.IP, method string) error {

	var rmService bool

	n, err := c.NetworkByID(nID)
	if err != nil {
		return err
	}

	skey := serviceKey{
		id:    svcID,
		ports: portConfigs(ingressPorts).String(),
	}

	c.Lock()
	s, ok := c.serviceBindings[skey]
	c.Unlock()
	logrus.Errorf("rmServiceBinding from %s START for %s %s", method, svcName, eID)
	if !ok {
		logrus.Errorf("rmServiceBinding %s %s %s out c.serviceBindings[skey]", method, svcName, eID)
		return nil
	}

	s.Lock()
	defer s.Unlock()
	lb, ok := s.loadBalancers[nID]
	if !ok {
		logrus.Errorf("rmServiceBinding %s %s %s out s.loadBalancers[nid] %p", method, svcName, eID, s)
		return nil
	}

	_, ok = lb.backEnds[eID]
	if !ok {
		logrus.Errorf("rmServiceBinding %s %s %s out lb.backEnds[eid] s:%p lb:%p", method, svcName, eID, s, lb)
		return nil
	}

	delete(lb.backEnds, eID)
	logrus.Errorf("rmServiceBinding delete (lb.backEnds[eid]) %s ip %s", eID, ip.String())
	if len(lb.backEnds) == 0 {
		// All the backends for this service have been
		// removed. Time to remove the load balancer and also
		// remove the service entry in IPVS.
		rmService = true

		logrus.Errorf("rmServiceBinding delete the lb %s s:%p lb:%p", eID, s, lb)
		delete(s.loadBalancers, nID)
	}

	if len(s.loadBalancers) == 0 {
		// All loadbalancers for the service removed. Time to
		// remove the service itself.
		c.Lock()
		logrus.Errorf("rmServiceBinding delete the s %s s:%p", nID, s)

		// Mark the object as deleted so that the add won't use it wrongly
		s.deleted = true
		delete(c.serviceBindings, skey)
		c.Unlock()
	}

	ok, entries := s.removeIPToEndpoint(ip.String(), eID)
	s.printIPToEndpoint(ip.String(), "rmServiceBinding "+eID)
	if !ok || entries > 0 {
		logrus.Errorf("rmServiceBinding %s WEIRD ok:%t entries:%d SUPPRESSING this event it maybe stale", eID, ok, entries)
		return nil
	}

	// Remove loadbalancer service(if needed) and backend in all
	// sandboxes in the network only if the vip is valid.
	if len(vip) != 0 {
		n.(*network).rmLBBackend(ip, vip, lb.fwMark, ingressPorts, rmService)
	}

	// Delete the name resolutions
	c.deleteEndpointNameResolution(svcName, svcID, nID, eID, containerName, vip, serviceAliases, taskAliases, ip, rmService, entries > 0, "rmServiceBinding")

	logrus.Errorf("rmServiceBinding from %s END for %s %s", method, svcName, eID)
	return nil
}
