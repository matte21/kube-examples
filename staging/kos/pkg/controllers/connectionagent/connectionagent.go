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

package connectionagent

import (
	"sync"

	k8stypes "k8s.io/apimachinery/pkg/types"
	k8scache "k8s.io/client-go/tools/cache"
	k8sworkqueue "k8s.io/client-go/util/workqueue"

	kosclientv1a1 "k8s.io/examples/staging/kos/pkg/client/clientset/versioned/typed/network/v1alpha1"
	netv1a1lister "k8s.io/examples/staging/kos/pkg/client/listers/network/v1alpha1"
	// TODO find a better name for this pkg or a better alias to import it
	"k8s.io/examples/staging/kos/pkg/localifc"
)

const (
	// NetworkAttachments v1alpha1 fields names. Used to
	// build isntances of fields.Selector and/or to update
	// such fields in the corresponding NetworkAttachments
	// objects.
	attHostIPFieldName = "status.hostIP"
	attNodeFieldName   = "spec.node"
	attVNIFieldName    = "spec.vni"
	attIPFieldName     = "status.ipv4"
	attIfcFieldName    = "status.ifcName"
)

// vnState stores all the state needed for a Virtual Network for
// which there is at least one NetworkAttachment local to this node.
type vnState struct {
	// remoteAttsInformer is an informer on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not on
	// this node.
	remoteAttsInformer *k8scache.SharedInformer

	// remoteAttsLister is a lister on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not
	// on this node. Since a Virtual Network cannot span multiple k8s API
	// namespaces, it's a NamespaceLister
	remoteAttsLister *netv1a1lister.NetworkAttachmentNamespaceLister

	nbrOfLocalAttsMutex sync.RWMutex

	// nbrOfLocalAtts stores the number of NetworkAttachments that are both:
	// (1) in the Virtual Network the vnState represents, (2) on this node.
	// It can be accessed only after acquiring nbrOfLocalAttsMutex. Notice
	// that the fact that we're using uint, which is represented with a finite
	// number of bits (32 or 64 on 32-bit and 64-bit architectures respectively),
	// means that there's a limit to the number of local NetworkAttachments in
	// the Virtual Network (2^32 -1 and 2^64 - 1 on 32-bit and 64-bit architectures
	// respectively), and exceeding that leads to an overflow and incorrect behavior.
	// Such limits though are way higher than the number of NetworkAttachments we
	// expect.
	// TODO investigate whether using package math/big leads to a significant
	// performance penalty. Use that if not.
	nbrOfLocalAtts uint
}

type vniAndNsn struct {
	vni uint32
	nsn k8stypes.NamespacedName
}

type netAttQueueRef struct {
	vni   uint32
	local bool
	nsn   k8stypes.NamespacedName
}

// TODO make the following comments more precise.
// ConnectionAgent represents K8S controller which runs on every node of the cluster and
// eagerly maintains the mapping between virtual IPs and physical IPs for every relevant
// NetworkAttachment up-to-date. A NetworkAttachment is relevant for a connection agent
// if: (1) it runs on the same node as the connection agent, or (2) it's part of a
// Virtual Network where at least one NetworkAttachment for which (1) is true exists. To
// achieve its goal, a Connection Agent receives notifications about relevant
// NetworkAttachments from the K8s API server through Informers, and when necessary
// creates/updates/deletes Network Interfaces through a low-level networking interface.
type ConnectionAgent struct {
	hostIP        string
	localNodeName string
	netIfc        kosclientv1a1.NetworkV1alpha1Interface
	queue         k8sworkqueue.RateLimitingInterface
	workers       int

	localAttsInformer *k8scache.SharedInformer

	vniToVnStateMutex sync.Mutex
	vniToVnState      map[uint32]*vnState

	localIfcsMutex sync.Mutex
	localIfcs      map[vniAndNsn]localifc.NetworkInterface

	remoteIfcsMutex sync.Mutex
	remoteIfcs      map[vniAndNsn]localifc.NetworkInterface
}

func NewConnectionAgent(hostIP string,
	localNodeName string,
	netIfc kosclientv1a1.NetworkV1alpha1Interface,
	queue k8sworkqueue.RateLimitingInterface,
	workers int) *ConnectionAgent {

	return &ConnectionAgent{
		hostIP:        hostIP,
		localNodeName: localNodeName,
		netIfc:        netIfc,
		queue:         queue,
		workers:       workers,
		vniToVnState:  make(map[uint32]*vnState),
		localIfcs:     make(map[vniAndNsn]localifc.NetworkInterface),
		remoteIfcs:    make(map[vniAndNsn]localifc.NetworkInterface),
	}
}

func (ctlr *ConnectionAgent) Run(stopCh <-chan struct{}) error {
	// TODO implement
	return nil
}
