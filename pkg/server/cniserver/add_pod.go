package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/docker/libnetwork/client"
	"github.com/docker/libnetwork/pkg/cniapi"
	log "github.com/sirupsen/logrus"
)

func addPod(w http.ResponseWriter, r *http.Request, vars map[string]string) (_ interface{}, retErr error) {
	cniInfo := cniapi.CniInfo{}

	content, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Failed to read request: %v", err)
		return nil, err
	}

	if err := json.Unmarshal(content, &cniInfo); err != nil {
		return nil, err
	}
	log.Infof("Received add pod request %+v", cniInfo)
	sbID, err := createSandbox(cniInfo.ContainerID, cniInfo.NetNS)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox for %q: %v", cniInfo.ContainerID, err)
	}
	defer func() {
		if retErr != nil {
			deleteSandbox(sbID)
		}
	}()
	epID, err := createEndpoint(cniInfo.ContainerID, cniInfo.NetConf)
	if err != nil {
		return nil, fmt.Errorf("failed to create endpoint for %q: %v", cniInfo.ContainerID, err)
	}
	defer func() {
		if retErr != nil {
			deleteEndpoint(epID, cniInfo.NetConf)
		}
	}()
	if err = endpointJoin(sbID, epID, cniInfo.NetNS); err != nil {
		return nil, fmt.Errorf("failed to attach endpoint to sandbox for container:%q,sandbox:%q,endpoint:%q, error:%v", cniInfo.ContainerID, sbID, epID, err)
	}
	defer func() {
		if retErr != nil {
			err = endpointLeave(sbID, epID)
			log.Warnf("failed to detach endpoint %q from sandbox %q , err:%v", epID, sbID, err)
		}
	}()
	return nil, err

}

func createSandbox(ContainerID, netns string) (_ string, retErr error) {
	sc := client.SandboxCreate{ContainerID: ContainerID, UseExternalKey: true}
	obj, _, err := readBody(httpCall("POST", "/sandboxes", sc, nil))
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			_, err = deleteSandbox(ContainerID)
			log.Warnf("error delete sandbox for container id %q on create sandbox failure, error:%v", ContainerID, err)
		}
	}()

	var replyID string
	err = json.Unmarshal(obj, &replyID)
	if err != nil {
		return "", err
	}
	return replyID, nil
}

func createEndpoint(ContainerID string, netConfig types.NetConf) (_ string, retErr error) {
	var replyID string
	sc := client.ServiceCreate{Name: ContainerID, Network: netConfig.Name}
	obj, _, err := readBody(httpCall("POST", "/services", sc, nil))
	if err != nil {
		return "", err
	}
	err = json.Unmarshal(obj, &replyID)
	return replyID, err
}

func endpointJoin(sandboxID, endpointID, netns string) (retErr error) {
	nc := client.ServiceAttach{SandboxID: sandboxID, SandboxKey: netns}

	_, _, err := readBody(httpCall("POST", "/services/"+endpointID+"/backend", nc, nil))

	return err
}
