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

	defaultNumWorkers  = 2
	defaultClientQPS   = 100
	defaultClientBurst = 200
)

func main() {
	var (
		nodeName               string
		hostIP                 string
		netFabricType          string
		kubeconfigFilename     string
		workers                int
		clientQPS, clientBurst int
	)
	flag.StringVar(&nodeName, "nodename", "", "node name")
	flag.StringVar(&hostIP, "hostip", "", "host IP")
	flag.StringVar(&netFabricType, "netfabric", "", "network fabric type")
	flag.StringVar(&kubeconfigFilename, "kubeconfig", "", "kubeconfig filename")
	flag.IntVar(&workers, "workers", defaultNumWorkers, "number of worker threads")
	flag.IntVar(&clientQPS, "qps", defaultClientQPS, "limit on rate of calls to api-server")
	flag.IntVar(&clientBurst, "burst", defaultClientBurst, "allowance for transient burst of calls to api-server")
	flag.Set("logtostderr", "true")
	flag.Parse()

	if len(os.Getenv("GOMAXPROCS")) == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	nodeName, err := getNodeName(nodeName)
	if nodeName == "" {
		glog.Errorf("Could not retrieve node name: neither command line flag \"nodename\" nor env var \"NODE_NAME\" were set. Lookup of OS hostname failed: %s\n",
			err.Error())
		os.Exit(2)
	}

	hostIP = getHostIP(hostIP)
	if hostIP == "" {
		glog.Errorf("Could not retrieve host IP: neither command line flag \"hostip\" nor env var \"HOST_IP\" were provided\n")
		os.Exit(3)
	}

	netFabricType = getNetFabricType(netFabricType)
	// No check on whether netFabricType is set because the
	// factory falls back to a default implementation if not.
	netFabric := netfabricfactory.NewNetFabricForType(netFabricType)

	clientCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigFilename)
	if err != nil {
		glog.Errorf("Failed to build client config for kubeconfig=%q: %s\n", kubeconfigFilename, err.Error())
		os.Exit(4)
	}
	clientCfg.QPS = float32(clientQPS)
	clientCfg.Burst = clientBurst

	kcs, err := kosclientset.NewForConfig(clientCfg)
	if err != nil {
		glog.Errorf("Failed to build KOS clientset for kubeconfig=%q: %s\n", kubeconfigFilename, err.Error())
		os.Exit(5)
	}

	// TODO think whether the rate limiter parameters make sense
	queue := workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(200*time.Millisecond, 8*time.Hour))

	ca := cactlr.NewConnectionAgent(nodeName, hostIP, kcs, queue, workers, netFabric)

	glog.Infof("Connection Agent start, nodeName=%s, hostIP=%s, netFabric=%s, kubeconfig=%q, workers=%d, QPS=%d, burst=%d\n",
		nodeName,
		hostIP,
		netFabric.Type(),
		kubeconfigFilename,
		workers,
		clientQPS,
		clientBurst)

	stopCh := StopOnSignals()
	err = ca.Run(stopCh)
	if err != nil {
		glog.Info(err)
	}
}

func getNodeName(nodeName string) (string, error) {
	if nodeName != "" {
		return nodeName, nil
	}
	nodeName, _ = os.LookupEnv(nodeNameEnv)
	if nodeName != "" {
		return nodeName, nil
	}
	// fall back to host name, the default node name
	return os.Hostname()
}

func getHostIP(hostIP string) string {
	if hostIP != "" {
		return hostIP
	}
	hostIP, _ = os.LookupEnv(hostIPEnv)
	if hostIP != "" {
		return hostIP
	}
	return ""
}

func getNetFabricType(netFabricType string) string {
	if netFabricType != "" {
		return netFabricType
	}
	netFabricType, _ = os.LookupEnv(netFabricTypeEnv)
	if netFabricType != "" {
		return netFabricType
	}
	return ""
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
