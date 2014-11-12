// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

// Test for KVFeed, source nozzle in XDCR
package main

import (
	"crypto/tls"
	"crypto/x509"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"encoding/json"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/metadata"
	rm "github.com/couchbase/goxdcr/replication_manager"
	s "github.com/couchbase/goxdcr/service_impl"
	ms "github.com/couchbase/goxdcr/mock_services"
	utils "github.com/couchbase/goxdcr/utils"
	"github.com/couchbase/goxdcr/tests/common"
	"net/http"
	"os"
	"time"
	"reflect"
)

var options struct {
	sourceKVHost string //source kv host name
	sourceKVPort      int //source kv admin port
	gometaPort        int // gometa request port
	username     string //username
	password     string //password
	
	// parameters of remote cluster
	remoteUuid string // remote cluster uuid
	remoteName string // remote cluster name
	remoteHostName string // remote cluster host name
	remoteUserName     string //remote cluster userName
	remotePassword     string //remote cluster password
	remoteDemandEncryption  bool  // whether encryption is needed
	remoteCertificate   string  // certificate for encryption
}

func argParse() {
	flag.StringVar(&options.sourceKVHost, "sourceKVHost", "127.0.0.1",
		"source KV host name")
	flag.IntVar(&options.sourceKVPort, "sourceKVPort", 9000,
		"admin port number for source kv")
	flag.IntVar(&options.gometaPort, "gometaPort", 5003,
		"port number for gometa requests")
	flag.StringVar(&options.username, "username", "Administrator", "userName to cluster admin console")
	flag.StringVar(&options.password, "password", "welcome", "password to Cluster admin console")
	
	flag.StringVar(&options.remoteUuid, "remoteUuid", "1234567",
		"remote cluster uuid")
	flag.StringVar(&options.remoteName, "remoteName", "remote",
		"remote cluster name")
	flag.StringVar(&options.remoteHostName, "remoteHostName", "127.0.0.1:9000", //"ec2-204-236-180-81.us-west-1.compute.amazonaws.com:8091",
		"remote cluster host name")
	flag.StringVar(&options.remoteUserName, "remoteUserName", "Administrator", "remote cluster userName")
	flag.StringVar(&options.remotePassword, "remotePassword", "welcome", "remote cluster password")
	flag.BoolVar(&options.remoteDemandEncryption, "remoteDemandEncryption", false, "whether encryption is needed")
	flag.StringVar(&options.remoteCertificate, "remoteCertificate", "", "certificate for encryption")

	flag.Parse()
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage : %s [OPTIONS] \n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	fmt.Println("Start Testing adminport...")
	argParse()
	startAdminport()
}

func startAdminport() {
	ms.SetTestOptions(utils.GetHostAddr(options.sourceKVHost, options.sourceKVPort), options.sourceKVHost, options.username, options.password)

	cmd, err := s.StartGometaService()
	if err != nil {
		fmt.Println("Test failed. err: ", err)
		return
	}

	defer s.KillGometaService(cmd)

	metadata_svc, err := s.DefaultMetadataSvc()
	if err != nil {
		fmt.Println("Test failed. err: ", err)
		return
	}
	
	rm.StartReplicationManager(options.sourceKVHost, options.sourceKVPort,
							   s.NewReplicationSpecService(metadata_svc, nil),
							   s.NewRemoteClusterService(metadata_svc, nil),	
							   new(ms.MockClusterInfoSvc), 
							   new(ms.MockXDCRTopologySvc), 
							   new(ms.MockReplicationSettingsSvc))
	
	//wait for server to finish starting
	time.Sleep(time.Second * 3)
	
	//testAuth()
	//testSSLAuth()
		
	if err := testRemoteClusters(false/*remoteClusterExpected*/); err != nil {
		fmt.Println(err.Error())
		return
	}

	if err := testCreateRemoteCluster(); err != nil {
		fmt.Println(err.Error())
		return
	}
	
	if err := testRemoteClusters(true/*remoteClusterExpected*/); err != nil {
		fmt.Println(err.Error())
		return
	}
	
	if err := testDeleteRemoteCluster(); err != nil {
		fmt.Println(err.Error())
		return
	}
	
	if err := testRemoteClusters(false/*remoteClusterExpected*/); err != nil {
		fmt.Println(err.Error())
		return
	}

	fmt.Println("All tests passed.")

}

func testAuth() error{
	url := fmt.Sprintf("http://%s:%s@%s/pools", options.remoteUserName, options.remotePassword, options.remoteHostName)
	fmt.Printf("url=%v\n", url)
	request, err := http.NewRequest(rm.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set(rm.ContentType, rm.DefaultContentType)

	fmt.Println("request", request)

	response, err := http.DefaultClient.Do(request)
	fmt.Printf("response=%v\n", response)

	// verify contents in response
	defer response.Body.Close()
	bodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	fmt.Printf("body=%v\n", bodyBytes)

	var v map[string]interface{}
	err = json.Unmarshal(bodyBytes, &v)
	fmt.Printf("v=%v, v.type=%v, err=%v\n", v, reflect.TypeOf(v), err)
	
	uuid, ok := v["uuid"]
	fmt.Printf("uuid=%v, ok=%v\n", uuid, ok)
	return nil
}

func testSSLAuth() {
	// Load client cert
	cert, err := tls.LoadX509KeyPair("/Users/yu/server.crt", 
			"/Users/yu/server.key")
	if err != nil {
		fmt.Printf("Could not load client certificate! err=%v\n", err)
		return 
	} 

	CA_Pool := x509.NewCertPool()
	serverCert, err := ioutil.ReadFile("/Users/yu/pem/remoteCert.pem")
	if err != nil {
    	fmt.Printf("Could not load server certificate! err=%v\n", err)
    	return
	}
	CA_Pool.AppendCertsFromPEM(serverCert)
	
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs: CA_Pool,
		InsecureSkipVerify : true,
	}
	tlsConfig.BuildNameToCertificate() 
	
	tr := &http.Transport{
		TLSClientConfig:    tlsConfig,
	}
	client := &http.Client{Transport: tr}
	url := fmt.Sprintf("https://%s:%s@%s/pools", options.remoteUserName, options.remotePassword, options.remoteHostName)
	fmt.Printf("url=%v\n", url)
	response, err := client.Get(url)
	fmt.Printf("response=%v, err=%v\n", response, err)

	if response == nil {
		return 
	}
	// verify contents in response
	defer response.Body.Close()
	bodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return
	}

	fmt.Printf("body=%v\n", bodyBytes)

	var v map[string]interface{}
	err = json.Unmarshal(bodyBytes, &v)
	fmt.Printf("v=%v, v.type=%v, err=%v\n", v, reflect.TypeOf(v), err)
	
	uuid, ok := v["uuid"]
	fmt.Printf("uuid=%v, ok=%v\n", uuid, ok)
	return 
}


func testRemoteClusters(remoteClusterExpected bool) error {
	url := common.GetAdminportUrlPrefix(options.sourceKVHost) + rm.RemoteClustersPath

	request, err := http.NewRequest(rm.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set(rm.ContentType, rm.DefaultContentType)

	fmt.Println("request", request)

	response, err := http.DefaultClient.Do(request)

	err = common.ValidateResponse("RemoteClusters", response, err)
	if err != nil {
		return err
	}
	
	// verify contents in response
	defer response.Body.Close()
	bodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	var remoteClusters []metadata.RemoteClusterReference
	err = json.Unmarshal(bodyBytes, &remoteClusters)
	if err != nil {
		return err
	}
	
	remoteClusterExists := false
	for _, remoteCluster := range remoteClusters {
		if remoteCluster.Name == options.remoteName {
			remoteClusterExists = true
			// verify that fields of remote cluster are as expected
			err = verifyRemoteCluster(&remoteCluster)
			if err != nil {
				return err
			}
			break
		}
	}
	
	if remoteClusterExists && !remoteClusterExpected {
		return errors.New("Did not expect remote cluster to exist but it did.")
	} 
	if !remoteClusterExists && remoteClusterExpected {
		return errors.New("Expected remote cluster to exist but it did not.")
	} 
	return nil
}
	
func testCreateRemoteCluster() error {
	url := common.GetAdminportUrlPrefix(options.sourceKVHost) + rm.RemoteClustersPath

	params := make(map[string]interface{})
	params[rm.RemoteClusterUuid] = options.remoteUuid
	params[rm.RemoteClusterName] = options.remoteName
	params[rm.RemoteClusterHostName] = options.remoteHostName
	params[rm.RemoteClusterUserName] = options.remoteUserName
	params[rm.RemoteClusterPassword] = options.remotePassword
	params[rm.RemoteClusterDemandEncryption] = options.remoteDemandEncryption
	params[rm.RemoteClusterCertificate] = options.remoteCertificate

	paramsBytes, _ := rm.EncodeMapIntoByteArray(params)
	paramsBuf := bytes.NewBuffer(paramsBytes)

	request, err := http.NewRequest(rm.MethodPost, url, paramsBuf)
	if err != nil {
		return err
	}
	request.Header.Set(rm.ContentType, rm.DefaultContentType)

	fmt.Println("request", request)

	response, err := http.DefaultClient.Do(request)

	err = common.ValidateResponse("CreateRemoteCluster", response, err)
	if err != nil {
		return err
	}
	return nil
}

func testDeleteRemoteCluster() error {
	url := common.GetAdminportUrlPrefix(options.sourceKVHost) + rm.RemoteClustersPath + base.UrlDelimiter + options.remoteName

	request, err := http.NewRequest(rm.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set(rm.ContentType, rm.DefaultContentType)

	fmt.Println("request", request)

	response, err := http.DefaultClient.Do(request)

	return common.ValidateResponse("DeleteRemoteCluster", response, err)
}

func verifyRemoteCluster(remoteCluster *metadata.RemoteClusterReference) error {
	if err := common.ValidateFieldValue(rm.RemoteClusterUuid, options.remoteUuid, remoteCluster.Uuid); err == nil {
		return errors.New("uuid is supposed to be updated to real value")
	}
	if err := common.ValidateFieldValue(rm.RemoteClusterHostName, options.remoteHostName, remoteCluster.HostName); err != nil {
		return err
	}
	if err := common.ValidateFieldValue(rm.RemoteClusterUserName, options.remoteUserName, remoteCluster.UserName); err != nil {
		return err
	}
	if err := common.ValidateFieldValue(rm.RemoteClusterPassword, options.remotePassword, remoteCluster.Password); err != nil {
		return err
	}
	if err := common.ValidateFieldValue(rm.RemoteClusterDemandEncryption, options.remoteDemandEncryption, remoteCluster.DemandEncryption); err != nil {
		return err
	}
	if err := common.ValidateFieldValue(rm.RemoteClusterCertificate, options.remoteCertificate, string(remoteCluster.Certificate)); err != nil {
		return err
	}
	return nil
}