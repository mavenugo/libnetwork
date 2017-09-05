package cniapi

import (
	"fmt"
	"net"
	"net/http"
	"encoding/json"
	"bytes"
	"io/ioutil"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/types"
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

func (l *LibNetCniClient) SetupPod(args *skel.CmdArgs) (*current.Result,error) {
	var data current.Result

	podNetInfo, err := validatePodNetworkInfo(args)
	buf, err := json.Marshal(podNetInfo)
	if err != nil {
		return nil, err
	}

	body := bytes.NewBuffer(buf)
	url := l.url + AddPodUrl
	r, err := l.httpClient.Post(url, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

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

	return &data, nil
}

func (l *LibNetCniClient) TearDownPod(args *skel.CmdArgs)error {
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
	rt.NetNS = args.Netns
	if args.IfName == "" {
		rt.IfName = "eth1"
	} else {
		rt.IfName = args.IfName
	}
	var netConf struct {
                types.NetConf
        }
        if err := json.Unmarshal(args.StdinData, &netConf);err != nil{
                return nil,fmt.Errorf("failed to unmarshal network configuration :%v",err)
        }
	rt.NetConf = netConf.NetConf
	return rt,nil
}
