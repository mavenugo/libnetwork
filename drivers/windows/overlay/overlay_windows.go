package overlay

//go:generate protoc -I.:../../Godeps/_workspace/src/github.com/gogo/protobuf  --gogo_out=import_path=github.com/docker/libnetwork/drivers/overlay,Mgogoproto/gogo.proto=github.com/gogo/protobuf/gogoproto:. overlay.proto

import (
	"fmt"
	"net"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/discoverapi"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/types"
	"github.com/hashicorp/serf/serf"
)

const (
	networkType  = "overlay"
	vethPrefix   = "veth"
	vethLen      = 7
	vxlanIDStart = 256
	vxlanIDEnd   = (1 << 24) - 1
	vxlanPort    = 4789
	vxlanVethMTU = 1450
)

var initVxlanIdm = make(chan (bool), 1)

type driver struct {
	eventCh          chan serf.Event
	notifyCh         chan ovNotify
	exitCh           chan chan struct{}
	bindAddress      string
	advertiseAddress string
	neighIP          string
	config           map[string]interface{}
	peerDb           peerNetworkMap
	serfInstance     *serf.Serf
	networks         networkTable
	store            datastore.DataStore
	localStore       datastore.DataStore
	once             sync.Once
	joinOnce         sync.Once
	sync.Mutex
}

// Init registers a new instance of overlay driver
func Init(dc driverapi.DriverCallback, config map[string]interface{}) error {
	logrus.Info("WINOVERLAY: Enter Init")

	c := driverapi.Capability{
		DataScope: datastore.GlobalScope,
	}

	d := &driver{
		networks: networkTable{},
		peerDb: peerNetworkMap{
			mp: map[string]*peerMap{},
		},
		config: config,
	}

	if data, ok := config[netlabel.GlobalKVClient]; ok {
		logrus.Info("WINOVERLAY: Inside GlobalKVCClient")
		var err error
		dsc, ok := data.(discoverapi.DatastoreConfigData)
		if !ok {
			return types.InternalErrorf("incorrect data in datastore configuration: %v", data)
		}
		d.store, err = datastore.NewDataStoreFromConfig(dsc)
		if err != nil {
			return types.InternalErrorf("failed to initialize data store: %v", err)
		}
	}

	if data, ok := config[netlabel.LocalKVClient]; ok {
		logrus.Info("WINOVERLAY: Inside LocalKVCClient")
		var err error
		dsc, ok := data.(discoverapi.DatastoreConfigData)
		if !ok {
			return types.InternalErrorf("incorrect data in datastore configuration: %v", data)
		}
		d.localStore, err = datastore.NewDataStoreFromConfig(dsc)
		if err != nil {
			return types.InternalErrorf("failed to initialize local data store: %v", err)
		}
	}

	d.restoreEndpoints()

	logrus.Info("WINOVERLAY: Exit Init")

	return dc.RegisterDriver(networkType, d, c)
}

// Endpoints are stored in the local store. Restore them and reconstruct the overlay sandbox
func (d *driver) restoreEndpoints() error {
	logrus.Info("WINOVERLAY: Enter Restore Endpoints")

	if d.localStore == nil {
		logrus.Warnf("Cannot restore overlay endpoints because local datastore is missing")
		return nil
	}
	kvol, err := d.localStore.List(datastore.Key(overlayEndpointPrefix), &endpoint{})
	if err != nil && err != datastore.ErrKeyNotFound {
		return fmt.Errorf("failed to read overlay endpoint from store: %v", err)
	}

	if err == datastore.ErrKeyNotFound {
		logrus.Info("WINOVERLAY: Restore Endpoints: None to restore.")

		return nil
	}

	for _, kvo := range kvol {
		ep := kvo.(*endpoint)
		logrus.Infof("WINOVERLAY: Restore Endpoints: Restoring %s", ep.id)
		n := d.network(ep.nid)
		if n == nil {
			logrus.Debugf("Network (%s) not found for restored endpoint (%s)", ep.nid[0:7], ep.id[0:7])
			logrus.Debugf("Deleting stale overlay endpoint (%s) from store", ep.id[0:7])
			if err := d.deleteEndpointFromStore(ep); err != nil {
				logrus.Debugf("Failed to delete stale overlay endpoint (%s) from store", ep.id[0:7])
			}
			continue
		}
		logrus.Info("WINOVERLAY: Restore Endpoints: Network found")
		n.addEndpoint(ep)
		d.peerDbAdd(ep.nid, ep.id, ep.addr.IP, ep.addr.Mask, ep.mac, net.ParseIP(n.providerAddress), true)
	}

	logrus.Info("WINOVERLAY: Exit Restore Endpoints")

	return nil
}

// Fini cleans up the driver resources
func Fini(drv driverapi.Driver) {
	logrus.Info("WINOVERLAY: Enter Fini")

	d := drv.(*driver)

	if d.exitCh != nil {
		waitCh := make(chan struct{})

		d.exitCh <- waitCh

		<-waitCh
	}
}

func (d *driver) configure() error {
	logrus.Info("WINOVERLAY: Enter configure")

	if d.store == nil {
		return nil
	}

	return nil
}

func (d *driver) Type() string {
	return networkType
}

func validateSelf(node string) error {
	logrus.Info("WINOVERLAY: Enter validateSelf")

	advIP := net.ParseIP(node)
	if advIP == nil {
		return fmt.Errorf("invalid self address (%s)", node)
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("Unable to get interface addresses %v", err)
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.Equal(advIP) {
			return nil
		}
	}
	return fmt.Errorf("Multi-Host overlay networking requires cluster-advertise(%s) to be configured with a local ip-address that is reachable within the cluster", advIP.String())
}

func (d *driver) nodeJoin(advertiseAddress, bindAddress string, self bool) {

	logrus.Info("WINOVERLAY: Enter nodeJoin")

	if self && !d.isSerfAlive() {
		if err := validateSelf(advertiseAddress); err != nil {
			logrus.Errorf("%s", err.Error())
		}

		logrus.Infof("WINOVERLAY: nodeJoin local driver advertiseAddress/bindAddress set to %s / %s", advertiseAddress, bindAddress)

		d.Lock()
		d.advertiseAddress = advertiseAddress
		d.bindAddress = bindAddress
		d.Unlock()

		// If there is no cluster store there is no need to start serf.
		if d.store != nil {

			logrus.Info("WINOVERLAY: nodeJoin There is a cluster, doing serfInit")

			err := d.serfInit()
			if err != nil {
				logrus.Errorf("initializing serf instance failed: %v", err)
				return
			}
		}
	}

	logrus.Infof("WINOVERLAY: nodeJoin done with self setup")

	d.Lock()
	if !self {
		d.neighIP = advertiseAddress
	}
	neighIP := d.neighIP
	d.Unlock()

	logrus.Infof("WINOVERLAY: nodeJoin neighIP is %s", neighIP)

	if d.serfInstance != nil && neighIP != "" {

		logrus.Infof("WINOVERLAY: nodeJoin wow doing serfJoin for neighIP")

		var err error
		d.joinOnce.Do(func() {
			err = d.serfJoin(neighIP)
			if err == nil {
				d.pushLocalDb()
			}
		})
		if err != nil {
			logrus.Errorf("joining serf neighbor %s failed: %v", advertiseAddress, err)
			d.Lock()
			d.joinOnce = sync.Once{}
			d.Unlock()
			return
		}
	}
}

func (d *driver) pushLocalEndpointEvent(action, nid, eid string) {

	logrus.Info("WINOVERLAY: Enter pushLocalEndpointEvent")

	n := d.network(nid)
	if n == nil {
		logrus.Debugf("Error pushing local endpoint event for network %s", nid)
		return
	}
	ep := n.endpoint(eid)
	if ep == nil {
		logrus.Debugf("Error pushing local endpoint event for ep %s / %s", nid, eid)
		return
	}

	if !d.isSerfAlive() {
		return
	}
	d.notifyCh <- ovNotify{
		action: "join",
		nw:     n,
		ep:     ep,
	}
}

// DiscoverNew is a notification for a new discovery event, such as a new node joining a cluster
func (d *driver) DiscoverNew(dType discoverapi.DiscoveryType, data interface{}) error {

	logrus.Info("WINOVERLAY: Enter DiscoverNew")

	var err error
	switch dType {
	case discoverapi.NodeDiscovery:
		nodeData, ok := data.(discoverapi.NodeDiscoveryData)
		if !ok || nodeData.Address == "" {
			return fmt.Errorf("invalid discovery data")
		}
		d.nodeJoin(nodeData.Address, nodeData.BindAddress, nodeData.Self)
	case discoverapi.DatastoreConfig:
		if d.store != nil {
			return types.ForbiddenErrorf("cannot accept datastore configuration: Overlay driver has a datastore configured already")
		}
		dsc, ok := data.(discoverapi.DatastoreConfigData)
		if !ok {
			return types.InternalErrorf("incorrect data in datastore configuration: %v", data)
		}
		d.store, err = datastore.NewDataStoreFromConfig(dsc)
		if err != nil {
			return types.InternalErrorf("failed to initialize data store: %v", err)
		}
	default:
	}
	return nil
}

// DiscoverDelete is a notification for a discovery delete event, such as a node leaving a cluster
func (d *driver) DiscoverDelete(dType discoverapi.DiscoveryType, data interface{}) error {

	logrus.Info("WINOVERLAY: Enter DiscoverDelete")

	return nil
}
