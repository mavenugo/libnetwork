package cniapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ns"
	log "github.com/sirupsen/logrus"
)

const (
	AddPodUrl         = "/AddPod"
	DelPodUrl         = "/DelPod"
	LibnetworkCNISock = "/var/run/cni-libnetwork.sock"
	PluginPath        = "/run/libnetwork"
)

type LibNetCniClient struct {
	url        string
	httpClient *http.Client
}

type CniInfo struct {
	ContainerID string
	NetNS       string
	IfName      string
	NetConf     types.NetConf
}

func unixDial(proto, addr string) (conn net.Conn, err error) {
	sock := LibnetworkCNISock
	return net.Dial("unix", sock)
}

func NewLibNetCniClient() *LibNetCniClient {
	c := new(LibNetCniClient)
	c.url = "http://localhost"
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			Dial: unixDial,
		},
	}
	return c
}

// SetupPod setups up the sandbox and endpoint for the infra container in a pod
func (l *LibNetCniClient) SetupPod(args *skel.CmdArgs) (*current.Result, error) {
	var data current.Result
	log.Infof("Received Setup Pod %+v", args)
	podNetInfo, err := validatePodNetworkInfo(args)
	if err != nil {
		return nil, fmt.Errorf("failed to valid cni arguements, error: %v", err)
	}
	buf, err := json.Marshal(podNetInfo)
	if err != nil {
		return nil, err
	}
	body := bytes.NewBuffer(buf)
	url := l.url + AddPodUrl
	r, err := l.httpClient.Post(url, "application/json", body)
	if err != nil {
		fmt.Printf("%v \n", err)
		return nil, err
	}
	defer r.Body.Close()
	fmt.Printf("Done with http post \n")
	switch {
	case r.StatusCode == int(404):
		return nil, fmt.Errorf("page not found")

	case r.StatusCode == int(403):
		return nil, fmt.Errorf("access denied")

	case r.StatusCode == int(500):
		info, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(info, &data)
		if err != nil {
			return nil, err
		}
		return &data, fmt.Errorf("Internal Server Error")

	case r.StatusCode != int(200):
		log.Errorf("POST Status '%s' status code %d \n", r.Status, r.StatusCode)
		return nil, fmt.Errorf("%s", r.Status)
	}

	response, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(response, &data)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Received AddPod Response: %+v", data)
	return &data, nil
}

// TearDownPod tears the sandbox and endpoint created for the infra
// container in the pod.
func (l *LibNetCniClient) TearDownPod(args *skel.CmdArgs) error {
	var data current.Result
	log.Infof("Received Teardown Pod request %+v", args)
	podNetInfo, err := validatePodNetworkInfo(args)
	if err != nil {
		return fmt.Errorf("failed to valid cni arguements, error: %v", err)
	}
	buf, err := json.Marshal(podNetInfo)
	if err != nil {
		return err
	}
	body := bytes.NewBuffer(buf)
	url := l.url + DelPodUrl
	r, err := l.httpClient.Post(url, "application/json", body)
	if err != nil {
		fmt.Printf("%v \n", err)
		return err
	}
	defer r.Body.Close()
	fmt.Printf("Done with http post \n")
	switch {
	case r.StatusCode == int(404):
		return fmt.Errorf("page not found")

	case r.StatusCode == int(403):
		return fmt.Errorf("access denied")

	case r.StatusCode == int(500):
		info, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return err
		}
		err = json.Unmarshal(info, &data)
		if err != nil {
			return err
		}
		return fmt.Errorf("Internal Server Error")

	case r.StatusCode != int(200):
		log.Errorf("POST Status '%s' status code %d \n", r.Status, r.StatusCode)
		return fmt.Errorf("%s", r.Status)
	}

	response, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}

	err = json.Unmarshal(response, &data)
	if err != nil {
		return err
	}
	fmt.Printf("Received DelPod Response: %+v", data)
	return nil
}

func validatePodNetworkInfo(args *skel.CmdArgs) (*CniInfo, error) {
	rt := new(CniInfo)
	if args.ContainerID == "" {
		return nil, fmt.Errorf("containerID empty")
	}
	rt.ContainerID = args.ContainerID
	if args.Netns == "" {
		return nil, fmt.Errorf("network namespace not present")
	}
	_, err := ns.GetNS(args.Netns)
	if err != nil {
		return nil, err
	}
	rt.NetNS = args.Netns
	if args.IfName == "" {
		rt.IfName = "eth1"
	} else {
		rt.IfName = args.IfName
	}
	var netConf struct {
		types.NetConf
	}
	if err := json.Unmarshal(args.StdinData, &netConf); err != nil {
		return nil, fmt.Errorf("failed to unmarshal network configuration :%v", err)
	}
	rt.NetConf = netConf.NetConf
	return rt, nil
}
