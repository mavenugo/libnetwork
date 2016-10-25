package overlay

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Microsoft/hcsshim"
	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/datastore"
)

const overlayNetworkPrefix = "overlay/network"

type localNetwork struct {
	id              string
	hnsID           string
	providerAddress string
	dbIndex         uint64
	dbExists        bool
	sync.Mutex
}

func (d *driver) findHnsNetwork(n *network) error {
	ln, err := d.getLocalNetworkFromStore(n.id)

	logrus.Infof("WINOVERLAY: Finding localnetwork  network id: %s", n.id)
	if err != nil {
		return err
	}

	if ln == nil {
		logrus.Infof("WINOVERLAY: localnetwork  not found id: %s", n.id)
		// A non blank hnsid indicates that the network was discovered
		// from HNS. No need to call HNS if this network was discovered
		// from HNS

		logrus.Infof("WINOVERLAY: CreateNetwork will notify HNS of network id: %s", n.id)

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
			NetworkAdapterName: n.interfaceName,
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

		// Write network to store to persist the providerAddress
		logrus.Infof("WINOVERLAY: CreateNetwork: Writing network to store again with PA %s", n.providerAddress)

		if err := d.writeLocalNetworkToStore(n); err != nil {
			return fmt.Errorf("failed to update data store for network %v: %v", n.id, err)
		}
	} else {
		logrus.Infof("WINOVERLAY: localnetwork  found id: %s", n.id)
		n.hnsId = ln.hnsID
		n.providerAddress = ln.providerAddress
	}

	return nil
}

func (d *driver) getLocalNetworkFromStore(nid string) (*localNetwork, error) {

	if d.localStore == nil {
		return nil, fmt.Errorf("overlay local store not initialized, network not found")
	}

	n := &localNetwork{id: nid}
	if err := d.localStore.GetObject(datastore.Key(n.Key()...), n); err != nil {
		return nil, nil
	}

	return n, nil
}

func (d *driver) deleteLocalNetworkFromStore(n *network) error {
	if d.localStore == nil {
		return fmt.Errorf("overlay local store not initialized, network not deleted")
	}

	ln, err := d.getLocalNetworkFromStore(n.id)

	if err != nil {
		return err
	}
	logrus.Infof("WINOVERLAY: Deleting localnetwork  id: %s", n.id)
	if err = d.localStore.DeleteObjectAtomic(ln); err != nil {
		return err
	}

	return nil
}

func (d *driver) writeLocalNetworkToStore(n *network) error {
	if d.localStore == nil {
		return fmt.Errorf("overlay local store not initialized, network not added")
	}

	ln := &localNetwork{
		id:              n.id,
		hnsID:           n.hnsId,
		providerAddress: n.providerAddress,
	}

	logrus.Infof("WINOVERLAY: Storing localnetwork  id: %s", n.id)
	if err := d.localStore.PutObjectAtomic(ln); err != nil {
		return err
	}
	return nil
}

func (n *localNetwork) DataScope() string {
	return datastore.LocalScope
}

func (n *localNetwork) New() datastore.KVObject {
	return &localNetwork{}
}

func (n *localNetwork) CopyTo(o datastore.KVObject) error {
	dstep := o.(*localNetwork)
	*dstep = *n
	return nil
}

func (n *localNetwork) Key() []string {
	return []string{overlayNetworkPrefix, n.id}
}

func (n *localNetwork) KeyPrefix() []string {
	return []string{overlayNetworkPrefix}
}

func (n *localNetwork) Index() uint64 {
	return n.dbIndex
}

func (n *localNetwork) SetIndex(index uint64) {
	n.dbIndex = index
	n.dbExists = true
}

func (n *localNetwork) Exists() bool {
	return n.dbExists
}

func (n *localNetwork) Skip() bool {
	return false
}

func (n *localNetwork) Value() []byte {
	b, err := json.Marshal(n)
	if err != nil {
		return nil
	}
	return b
}

func (n *localNetwork) SetValue(value []byte) error {
	return json.Unmarshal(value, n)
}

func (n *localNetwork) MarshalJSON() ([]byte, error) {
	networkMap := make(map[string]interface{})

	networkMap["id"] = n.id
	networkMap["hnsID"] = n.hnsID
	networkMap["providerAddress"] = n.providerAddress
	return json.Marshal(networkMap)
}

func (n *localNetwork) UnmarshalJSON(value []byte) error {
	var networkMap map[string]interface{}

	json.Unmarshal(value, &networkMap)

	n.id = networkMap["id"].(string)
	n.hnsID = networkMap["hnsID"].(string)
	n.providerAddress = networkMap["providerAddress"].(string)
	return nil
}
