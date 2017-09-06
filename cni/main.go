package main

import (
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/docker/libnetwork/pkg/cniapi"
	log "github.com/sirupsen/logrus"
)

func cmdAdd(args *skel.CmdArgs) error {
	fmt.Printf("Received CNI ADD %v", args)
	libClient := cniapi.NewLibNetCniClient()
	_, err := libClient.SetupPod(args)
	if err != nil {
		fmt.Errorf("Failed to setup Pod , %v", err)
	}
	return nil
	//return types.PrintResult(result, cniVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	fmt.Printf("Received CNI DEL %v", args)
	libClient := cniapi.NewLibNetCniClient()
	if err := libClient.TearDownPod(args); err != nil {
		return fmt.Errorf("Failed to tear down pod, %v", err)
	}
	return nil
}

func main() {
	log.Infof("Starting Libnetwork CNI plugin")
	skel.PluginMain(cmdAdd, cmdDel, version.PluginSupports("", "0.1.0", "0.2.0", version.Current()))
}
