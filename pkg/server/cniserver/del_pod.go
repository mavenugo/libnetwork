package server

import(
//	"fmt"	
	"net/http"
	"io/ioutil"
	"encoding/json"
	"github.com/docker/libnetwork/pkg/cniapi"
	log "github.com/sirupsen/logrus"
	"github.com/containernetworking/cni/pkg/types"
	//"github.com/docker/libnetwork/client"
)

func deletePod(w http.ResponseWriter, r *http.Request, vars map[string]string) (interface{}, error) {
	cniInfo:= cniapi.CniInfo{}
	
	content, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Failed to read request: %v", err)
		return nil, err
	}

	if err:= json.Unmarshal(content,&cniInfo);err != nil{	
		return nil,err
	}
	
	return content,err

}

func endpointLeave(sandboxID,endpointID string) (retErr error){
	return nil
}

func deleteSandbox(containerID string)(_ string,retErr error){
	return "",nil
}


func deleteEndpoint(containerID string,netConfig types.NetConf)(_ string,retErr error){
	return "",nil
}
