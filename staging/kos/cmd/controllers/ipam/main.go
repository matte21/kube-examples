/*
Copyright 2016 The Kubernetes Authors.

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
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/golang/glog"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	kosclientset "k8s.io/examples/staging/kos/pkg/client/clientset/versioned"
	kosinformers "k8s.io/examples/staging/kos/pkg/client/informers/externalversions"
	ipamctlr "k8s.io/examples/staging/kos/pkg/controllers/ipam"
	_ "k8s.io/examples/staging/kos/pkg/controllers/workqueue_prometheus"
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

	glog.Infof("IPAM controller start, kubeconfig=%q, workers=%d, QPS=%d, burst=%d\n", kubeconfigFilename, workers, clientQPS, clientBurst)

	if len(os.Getenv("GOMAXPROCS")) == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}
	rand.Seed(time.Now().UnixNano())
	rand.Uint64()
	rand.Uint64()
	rand.Uint64()

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
	stopCh := StopOnSignals()
	sif := kosinformers.NewSharedInformerFactory(kcs, 0)
	net1 := sif.Network().V1alpha1()
	subnetInformer := net1.Subnets()
	netattInformer := net1.NetworkAttachments()
	lockInformer := net1.IPLocks()
	sif.Start(stopCh)
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(200*time.Millisecond, 8*time.Hour), "kos_ipam_controller_queue")
	ctlr, err := ipamctlr.NewIPAMController(kcs.NetworkV1alpha1(), subnetInformer.Informer(), subnetInformer.Lister(), netattInformer.Informer(), netattInformer.Lister(), lockInformer.Informer(), lockInformer.Lister(), queue, workers)
	if err != nil {
		glog.Errorf("Failed to initialize IPAM controller: %s\n", err.Error())
		os.Exit(9)
	}
	glog.V(2).Infoln("Created IPAMController")
	ctlr.Run(stopCh)
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
