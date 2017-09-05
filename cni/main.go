package main 

import (
	"fmt"
	
	log "github.com/sirupsen/logrus"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"	
	"github.com/docker/libnetwork/pkg/cniapi"
)

func cmdAdd(args *skel.CmdArgs) error {
	fmt.Printf("Received CNI ADD %v",args)
	libClient:= cniapi.NewLibNetCniClient() 
	libClient.SetupPod(args)
	return nil
	//return types.PrintResult(result, cniVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	 fmt.Printf("Received CNI DEL %v",args)
	libClient:= cniapi.NewLibNetCniClient()
	return  libClient.TearDownPod(args)
}

func main() {
	log.Infof("Starting Libnetwork CNI plugin")
	skel.PluginMain(cmdAdd, cmdDel, version.PluginSupports("", "0.1.0", "0.2.0", version.Current()))
}	
