package overlay

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/Microsoft/hcsshim"
	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/types"
)

var (
	hostMode  bool
	networkMu sync.Mutex
)

type networkTable map[string]*network

type subnet struct {
	vni      uint32
	initErr  error
	subnetIP *net.IPNet
	gwIP     *net.IPNet
}

type subnetJSON struct {
	SubnetIP string
	GwIP     string
	Vni      uint32
}

type network struct {
	id              string
	name            string
	hnsId           string
	dbIndex         uint64
	dbExists        bool
	providerAddress string
	interfaceName   string
	endpoints       endpointTable
	driver          *driver
	initEpoch       int
	initErr         error
	subnets         []*subnet
	secure          bool
	sync.Mutex
}

func (d *driver) NetworkAllocate(id string, option map[string]string, ipV4Data, ipV6Data []driverapi.IPAMData) (map[string]string, error) {
	logrus.Info("WINOVERLAY: Enter NetworkAllocate")
	return nil, types.NotImplementedErrorf("not implemented")
}

func (d *driver) NetworkFree(id string) error {
	logrus.Info("WINOVERLAY: Enter NetworkFree")
	return types.NotImplementedErrorf("not implemented")
}

func (d *driver) CreateNetwork(id string, option map[string]interface{}, nInfo driverapi.NetworkInfo, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	var (
		networkName   string
		interfaceName string
	)

	logrus.Info("WINOVERLAY: Enter CreateNetwork")

	if id == "" {
		return fmt.Errorf("invalid network id")
	}

	if len(ipV4Data) == 0 || ipV4Data[0].Pool.String() == "0.0.0.0/0" {
		return types.BadRequestErrorf("ipv4 pool is empty")
	}

	vnis := make([]uint32, 0, len(ipV4Data))

	// Since we perform lazy configuration make sure we try
	// configuring the driver when we enter CreateNetwork
	if err := d.configure(); err != nil {
		return err
	}

	n := &network{
		id:        id,
		driver:    d,
		endpoints: endpointTable{},
		subnets:   []*subnet{},
	}

	genData, ok := option[netlabel.GenericData].(map[string]string)

	if !ok {
		return fmt.Errorf("Unknown generic data option")
	}

	for label, value := range genData {
		switch label {
		case "com.docker.network.windowsshim.networkname":
			networkName = value
		case "com.docker.network.windowsshim.interface":
			interfaceName = value
		case "com.docker.network.windowsshim.hnsid":
			n.hnsId = value
		case netlabel.OverlayVxlanIDList:
			vniStrings := strings.Split(value, ",")
			for _, vniStr := range vniStrings {
				vni, err := strconv.Atoi(vniStr)
				if err != nil {
					return fmt.Errorf("invalid vxlan id value %q passed", vniStr)
				}

				vnis = append(vnis, uint32(vni))
			}
		}
	}

	// If we are getting vnis from libnetwork, either we get for
	// all subnets or none.
	if len(vnis) < len(ipV4Data) {
		return fmt.Errorf("insufficient vnis(%d) passed to overlay", len(vnis))
	}

	for i, ipd := range ipV4Data {
		s := &subnet{
			subnetIP: ipd.Pool,
			gwIP:     ipd.Gateway,
		}

		if len(vnis) != 0 {
			s.vni = vnis[i]
		}

		n.subnets = append(n.subnets, s)
	}

	n.name = networkName
	if n.name == "" {
		n.name = id
	}

	n.interfaceName = interfaceName

	if err := n.writeToStore(); err != nil {
		return fmt.Errorf("failed to update data store for network %v: %v", n.id, err)
	}

	if nInfo != nil {
		if err := nInfo.TableEventRegister(ovPeerTable); err != nil {
			return err
		}
	}

	d.addNetwork(n)

	// A non blank hnsid indicates that the network was discovered
	// from HNS. No need to call HNS if this network was discovered
	// from HNS
	if n.hnsId == "" {

		logrus.Infof("WINOVERLAY: CreateNetwork will notify HNS of network id: %s", id)

		subnets := []hcsshim.Subnet{}

		for _, s := range n.subnets {
			subnet := hcsshim.Subnet{
				AddressPrefix: s.subnetIP.String(),
			}

			if s.gwIP != nil {
				subnet.GatewayAddress = s.gwIP.IP.String()
			}

			vsidPolicy, err := json.Marshal(hcsshim.VsidPolicy{
				Type: "VSID",
				VSID: uint(s.vni),
			})

			if err != nil {
				return err
			}

			subnet.Policies = append(subnet.Policies, vsidPolicy)
			subnets = append(subnets, subnet)
		}

		network := &hcsshim.HNSNetwork{
			Name:               n.name,
			Type:               d.Type(),
			Subnets:            subnets,
			NetworkAdapterName: interfaceName,
		}

		configurationb, err := json.Marshal(network)
		if err != nil {
			return err
		}

		configuration := string(configurationb)
		logrus.Infof("HNSNetwork Request =%v", configuration)

		hnsresponse, err := hcsshim.HNSNetworkRequest("POST", "", configuration)
		if err != nil {
			return err
		}
		n.hnsId = hnsresponse.Id
		n.providerAddress = hnsresponse.ManagementIP
		genData["com.docker.network.windowsshim.hnsid"] = n.hnsId

		// Write network to store to persist the providerAddress
		logrus.Infof("WINOVERLAY: CreateNetwork: Writing network to store again with PA %s", n.providerAddress)

		if err := n.writeToStore(); err != nil {
			return fmt.Errorf("failed to update data store for network %v: %v", n.id, err)
		}
	}

	logrus.Infof("WINOVERLAY: CreateNetwork: All done.")
	return nil
}

func (d *driver) DeleteNetwork(nid string) error {
	logrus.Info("WINOVERLAY: Enter DeleteNetwork")

	if nid == "" {
		return fmt.Errorf("invalid network id")
	}

	// Make sure driver resources are initialized before proceeding
	if err := d.configure(); err != nil {
		return err
	}

	n := d.network(nid)
	if n == nil {
		return fmt.Errorf("could not find network with id %s", nid)
	}

	logrus.Infof("WINOVERLAY: DeleteNetwork calling HNS with ID %v", n.hnsId)

	_, err := hcsshim.HNSNetworkRequest("DELETE", n.hnsId, "")
	if err != nil {
		return err
	}

	d.deleteNetwork(nid)

	return nil
}

func (d *driver) ProgramExternalConnectivity(nid, eid string, options map[string]interface{}) error {
	return nil
}

func (d *driver) RevokeExternalConnectivity(nid, eid string) error {
	return nil
}

func (d *driver) addNetwork(n *network) {

	logrus.Info("WINOVERLAY: Enter addNetwork")

	d.Lock()
	d.networks[n.id] = n
	d.Unlock()
}

func (d *driver) deleteNetwork(nid string) {

	logrus.Info("WINOVERLAY: Enter deleteNetwork")

	d.Lock()
	delete(d.networks, nid)
	d.Unlock()
}

func (d *driver) network(nid string) *network {

	logrus.Info("WINOVERLAY: Enter network")

	d.Lock()
	networks := d.networks
	d.Unlock()

	n, ok := networks[nid]
	if !ok {
		n = d.getNetworkFromStore(nid)
		if n != nil {
			n.driver = d
			n.endpoints = endpointTable{}
			networks[nid] = n
		}
	}

	return n
}

func (d *driver) getNetworkFromStore(nid string) *network {

	logrus.Info("WINOVERLAY: Enter getNetworkFromStore")

	if d.store == nil {
		return nil
	}

	n := &network{id: nid}
	if err := d.store.GetObject(datastore.Key(n.Key()...), n); err != nil {
		return nil
	}

	// As the network is being discovered from the global store, HNS may not be aware of it yet
	// Todo - this is code duplication from CreateNetwork, move to helper function

	logrus.Infof("WINOVERLAY: Notify HNS of existing network with name: %s", n.name)

	subnets := []hcsshim.Subnet{}

	for _, s := range n.subnets {
		subnet := hcsshim.Subnet{
			AddressPrefix: s.subnetIP.String(),
		}

		if s.gwIP != nil {
			subnet.GatewayAddress = s.gwIP.String()
		}

		vsidPolicy, err := json.Marshal(hcsshim.VsidPolicy{
			Type: "VSID",
			VSID: uint(s.vni),
		})

		if err != nil {
			// todo should log error
			return nil
		}

		subnet.Policies = append(subnet.Policies, vsidPolicy)
		subnets = append(subnets, subnet)
	}

	network := &hcsshim.HNSNetwork{
		Id:                 n.hnsId,
		Name:               n.name,
		Type:               d.Type(),
		Subnets:            subnets,
		NetworkAdapterName: n.interfaceName,
	}

	configurationb, err := json.Marshal(network)
	if err != nil {
		// todo should log error
		return nil
	}

	configuration := string(configurationb)
	logrus.Infof("HNSNetwork Request =%v", configuration)

	_, err = hcsshim.HNSNetworkRequest("POST", "", configuration)
	if err != nil {
		// todo should log error
		return nil
	}

	return n
}

func (n *network) Key() []string {
	return []string{"overlay", "network", n.id}
}

func (n *network) KeyPrefix() []string {
	return []string{"overlay", "network"}
}

func (n *network) Value() []byte {
	m := map[string]interface{}{}

	netJSON := []*subnetJSON{}

	for _, s := range n.subnets {
		sj := &subnetJSON{
			SubnetIP: s.subnetIP.String(),
			GwIP:     s.gwIP.String(),
			Vni:      s.vni,
		}
		netJSON = append(netJSON, sj)
	}

	b, err := json.Marshal(netJSON)
	if err != nil {
		return []byte{}
	}

	m["secure"] = n.secure
	m["subnets"] = netJSON
	m["providerAddress"] = n.providerAddress
	m["interfaceName"] = n.interfaceName
	m["hnsId"] = n.hnsId
	m["name"] = n.name
	b, err = json.Marshal(m)
	if err != nil {
		return []byte{}
	}

	return b
}

func (n *network) Index() uint64 {
	return n.dbIndex
}

func (n *network) SetIndex(index uint64) {
	n.dbIndex = index
	n.dbExists = true
}

func (n *network) Exists() bool {
	return n.dbExists
}

func (n *network) Skip() bool {
	return false
}

func (n *network) SetValue(value []byte) error {
	var (
		m       map[string]interface{}
		newNet  bool
		isMap   = true
		netJSON = []*subnetJSON{}
	)

	if err := json.Unmarshal(value, &m); err != nil {
		err := json.Unmarshal(value, &netJSON)
		if err != nil {
			return err
		}
		isMap = false
	}

	if len(n.subnets) == 0 {
		newNet = true
	}

	if isMap {
		if val, ok := m["secure"]; ok {
			n.secure = val.(bool)
		}
		if val, ok := m["providerAddress"]; ok {
			n.providerAddress = val.(string)
		}
		if val, ok := m["interfaceName"]; ok {
			n.interfaceName = val.(string)
		}
		if val, ok := m["hnsId"]; ok {
			n.hnsId = val.(string)
		}
		if val, ok := m["name"]; ok {
			n.name = val.(string)
		}
		bytes, err := json.Marshal(m["subnets"])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(bytes, &netJSON); err != nil {
			return err
		}
	}

	for _, sj := range netJSON {
		subnetIPstr := sj.SubnetIP
		gwIPstr := sj.GwIP
		vni := sj.Vni

		subnetIP, _ := types.ParseCIDR(subnetIPstr)
		gwIP, _ := types.ParseCIDR(gwIPstr)

		if newNet {
			s := &subnet{
				subnetIP: subnetIP,
				gwIP:     gwIP,
				vni:      vni,
			}
			n.subnets = append(n.subnets, s)
		} else {
			sNet := n.getMatchingSubnet(subnetIP)
			if sNet != nil {
				sNet.vni = vni
			}
		}
	}
	return nil
}

func (n *network) DataScope() string {
	return datastore.GlobalScope
}

func (n *network) writeToStore() error {

	logrus.Info("WINOVERLAY: Enter writeToStore")

	if n.driver.store == nil {
		logrus.Info("WINOVERLAY: writeToStore returning nil due to no driver.store")
		return nil
	}

	logrus.Info("WINOVERLAY: writeToStore putting atomic object")

	return n.driver.store.PutObjectAtomic(n)
}

// contains return true if the passed ip belongs to one the network's
// subnets
func (n *network) contains(ip net.IP) bool {
	for _, s := range n.subnets {
		if s.subnetIP.Contains(ip) {
			return true
		}
	}

	return false
}

// getSubnetforIP returns the subnet to which the given IP belongs
func (n *network) getSubnetforIP(ip *net.IPNet) *subnet {

	logrus.Info("WINOVERLAY: Enter getSubnetForIP")

	for _, s := range n.subnets {
		// first check if the mask lengths are the same
		i, _ := s.subnetIP.Mask.Size()
		j, _ := ip.Mask.Size()
		if i != j {
			continue
		}
		if s.subnetIP.Contains(ip.IP) {
			return s
		}
	}
	return nil
}

// getMatchingSubnet return the network's subnet that matches the input
func (n *network) getMatchingSubnet(ip *net.IPNet) *subnet {

	logrus.Info("WINOVERLAY: Enter getMatchingSubnet")

	if ip == nil {
		return nil
	}
	for _, s := range n.subnets {
		// first check if the mask lengths are the same
		i, _ := s.subnetIP.Mask.Size()
		j, _ := ip.Mask.Size()
		if i != j {
			continue
		}
		if s.subnetIP.IP.Equal(ip.IP) {
			return s
		}
	}
	return nil
}
