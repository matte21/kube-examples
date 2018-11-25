/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/golang/glog"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	kosclientset "k8s.io/examples/staging/kos/pkg/client/clientset/versioned"
	cactlr "k8s.io/examples/staging/kos/pkg/controllers/connectionagent"
	netfabricfactory "k8s.io/examples/staging/kos/pkg/networkfabric/factory"
)

const (
	nodeNameEnv      = "NODE_NAME"
	hostIPEnv        = "HOST_IP"
	netFabricTypeEnv = "NET_FABRIC_TYPE"
)

func main() {
	var kubeconfigFilename string
	var workers int
	var clientQPS, clientBurst int
	flag.StringVar(&kubeconfigFilename, "kubeconfig", "", "kubeconfig filename")
	flag.IntVar(&workers, "workers", 2, "number of worker threads")
	flag.IntVar(&clientQPS, "qps", 100, "limit on rate of calls to api-server")
	flag.IntVar(&clientBurst, "burst", 200, "allowance for transient burst of calls to api-server")
	flag.Set("logtostderr", "true")
	flag.Parse()

	glog.Infof("ConnectionAgent controller start, kubeconfig=%q, workers=%d, QPS=%d, burst=%d\n",
		kubeconfigFilename,
		workers,
		clientQPS,
		clientBurst)

	if len(os.Getenv("GOMAXPROCS")) == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	clientCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigFilename)
	if err != nil {
		glog.Errorf("Failed to build client config for kubeconfig=%q: %s\n", kubeconfigFilename, err.Error())
		os.Exit(2)
	}
	clientCfg.QPS = float32(clientQPS)
	clientCfg.Burst = clientBurst

	kcs, err := kosclientset.NewForConfig(clientCfg)
	if err != nil {
		glog.Errorf("Failed to build KOS clientset for kubeconfig=%q: %s\n", kubeconfigFilename, err.Error())
		os.Exit(3)
	}

	localNodeName, localNodeEnvSet := os.LookupEnv(nodeNameEnv)
	if !localNodeEnvSet || len(localNodeName) == 0 {
		glog.Error("Env variable storing the node name is not set")
		os.Exit(4)
	}
	glog.V(2).Infof("Node name: %s\n", localNodeName)

	hostIP, hostIPEnvSet := os.LookupEnv(hostIPEnv)
	if !hostIPEnvSet || len(hostIP) == 0 {
		glog.Error("Env variable storing the host IP is not set")
		os.Exit(5)
	}
	glog.V(2).Infof("Using %s as the node IP address\n", hostIP)

	// TODO Add possibility to set this field through command line option.
	// No check on whether the environment var is set because the
	// factory will fall back to a default implementation if not
	netFabricType, _ := os.LookupEnv(netFabricTypeEnv)
	netFabric := netfabricfactory.NewNetFabricForType(netFabricType)
	glog.V(2).Infof("Using %s as the type for the network fabric\n", netFabric.Type())

	// TODO think whether the rate limiter parameters make sense
	queue := workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(200*time.Millisecond, 8*time.Hour))

	ctlr := cactlr.NewConnectionAgent(localNodeName, hostIP, kcs.NetworkV1alpha1(), queue, workers, netFabric)
	glog.V(2).Infoln("Created ConnectionAgent")

	stopCh := StopOnSignals()
	err = ctlr.Run(stopCh)
	if err != nil {
		glog.Info(err)
	}
}

// StopOnSignals makes a "stop channel" that is closed upon receipt of certain
// OS signals commonly used to request termination of a process.  On the second
// such signal, Exit(1) immediately.
func StopOnSignals() <-chan struct{} {
	stopCh := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		close(stopCh)
		<-c
		os.Exit(1)
	}()
	return stopCh
}
