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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8swait "k8s.io/apimachinery/pkg/util/wait"
	k8scache "k8s.io/client-go/tools/cache"
	k8sworkqueue "k8s.io/client-go/util/workqueue"

	netv1a1 "k8s.io/examples/staging/kos/pkg/apis/network/v1alpha1"
	kosclientset "k8s.io/examples/staging/kos/pkg/client/clientset/versioned"
	kosinformers "k8s.io/examples/staging/kos/pkg/client/informers/externalversions"
	kosinternalifcs "k8s.io/examples/staging/kos/pkg/client/informers/externalversions/internalinterfaces"
	koslisterv1a1 "k8s.io/examples/staging/kos/pkg/client/listers/network/v1alpha1"
	netfabric "k8s.io/examples/staging/kos/pkg/networkfabric"
)

const (
	// K8s API server reosurce name for NetworkAttachments.
	attResourceName = "networkattachments"

	// NetworkAttachments in network.example.com/v1alpha1 fields
	// names. Used to build instances of fields.Selector.
	attNodeFieldName   = "spec.node"
	attVNIFieldName    = "spec.vni"
	attHostIPFieldName = "status.hostIP"
	attIPFieldName     = "status.ipv4"
	attIfcFieldName    = "status.ifcName"

	// fields.Selector comparison operators. Used to build
	// instances of fields.Selector.
	equal    = "="
	notEqual = "!="

	// resync period for Informers caches
	resyncPeriod = 0
)

// vnState stores all the state needed for a Virtual Network for
// which there is at least one NetworkAttachment local to this node.
type vnState struct {
	// remoteAttsInformer is an informer on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not on
	// this node.
	remoteAttsInformer k8scache.SharedInformer

	// remoteAttsLister is a lister on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not
	// on this node. Since a Virtual Network cannot span multiple k8s API
	// namespaces, it's a NamespaceLister.
	remoteAttsLister koslisterv1a1.NetworkAttachmentNamespaceLister

	nbrOfLocalAttsMutex sync.Mutex

	// nbrOfLocalAtts stores the number of NetworkAttachments that are both:
	// (1) in the Virtual Network the vnState represents, (2) on this node.
	// It can be accessed only after acquiring nbrOfLocalAttsMutex. Notice
	// that the fact that its type is uint, which is represented with a finite
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

type attQueueRef struct {
	vni   uint32
	local bool
	nsn   k8stypes.NamespacedName
}

// TODO make the following comments more precise.
// ConnectionAgent represents K8S controller which runs on every node of the cluster and
// eagerly maintains up-to-date the mapping between virtual IPs and physical IPs for
// every relevant NetworkAttachment. A NetworkAttachment is relevant for a connection agent
// if: (1) it runs on the same node as the connection agent, or (2) it's part of a
// Virtual Network where at least one NetworkAttachment for which (1) is true exists.
// To achieve its goal, a Connection Agent receives notifications about relevant
// NetworkAttachments from the K8s API server through Informers, and when necessary
// creates/updates/deletes Network Interfaces through a low-level network interface fabric.
type ConnectionAgent struct {
	localNodeName string
	hostIP        string
	kcs           *kosclientset.Clientset
	queue         k8sworkqueue.RateLimitingInterface
	workers       int
	netFabric     netfabric.Interface

	localAttsInformer k8scache.SharedIndexInformer
	localAttsLister   koslisterv1a1.NetworkAttachmentLister

	vniToVnStateMutex sync.Mutex
	vniToVnState      map[uint32]*vnState

	localIfcsMutex sync.Mutex
	localIfcs      map[vniAndNsn]netfabric.NetworkInterface

	remoteIfcsMutex sync.Mutex
	remoteIfcs      map[vniAndNsn]netfabric.NetworkInterface
}

func NewConnectionAgent(localNodeName string,
	hostIP string,
	kcs *kosclientset.Clientset,
	queue k8sworkqueue.RateLimitingInterface,
	workers int,
	netFabric netfabric.Interface) *ConnectionAgent {

	return &ConnectionAgent{
		localNodeName: localNodeName,
		hostIP:        hostIP,
		kcs:           kcs,
		queue:         queue,
		workers:       workers,
		netFabric:     netFabric,
		vniToVnState:  make(map[uint32]*vnState),
		localIfcs:     make(map[vniAndNsn]netfabric.NetworkInterface),
		remoteIfcs:    make(map[vniAndNsn]netfabric.NetworkInterface),
	}
}

func (ca *ConnectionAgent) Run(stopCh <-chan struct{}) error {
	defer k8sutilruntime.HandleCrash()
	defer ca.queue.ShutDown()

	ca.initLocalAttsInformerAndLister()
	ca.startLocalAttsInformer(stopCh)
	glog.V(2).Infoln("Local NetworkAttachments Informer started")

	err := ca.waitForLocalAttsCacheSync(stopCh)
	if err != nil {
		return err
	}
	glog.V(2).Infoln("Local NetworkAttachments cache synced")

	// TODO retrieve already existing interfaces and init data structures accordingly

	for i := 0; i < ca.workers; i++ {
		// ? Why can't we just run ca.processQueue non-stop? AKA why are we executing it periodically every second?
		// We wouldn't be doing busy waiting because invoking Get() from an empty queue is blocking. One possible
		// reason is that we expect Notifications for the same object to arrive at a high frequence: processing all
		// of them is inefficient and useless, because you react to a notification and as soon as you're done you
		// need to undo what you just did because of a new notification, so it's better to wait for more notifications
		// to accumulate.
		go k8swait.Until(ca.processQueue, time.Second, stopCh)
	}
	glog.V(2).Infof("Launched %d workers\n", ca.workers)

	<-stopCh
	return nil
}

func (ca *ConnectionAgent) OnLocalAttAdded(obj interface{}) {
	localAtt := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v addition to local NetworkAttachments cache\n", localAtt)
	localAttRef := ca.fromAttToEnquableRef(localAtt)
	ca.queue.Add(localAttRef)
}

func (ca *ConnectionAgent) OnLocalAttUpdated(oldObj, newObj interface{}) {
	oldLocalAtt := oldObj.(*netv1a1.NetworkAttachment)
	newLocalAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment update from %#+v to %#+v in local NetworkAttachments cache\n", oldLocalAtt, newLocalAtt)

	newLocalAttRef := ca.fromAttToEnquableRef(newLocalAtt)
	ca.queue.Add(newLocalAttRef)
	if newLocalAtt.Spec.VNI != oldLocalAtt.Spec.VNI {
		// A reference is made up by a namespaced name, a vni, and a locality flag. Changes to namespace and name are not possible:
		// that's equivalent to creating a new, independent network attachment. Changes to the node where the network attachment runs
		// won't result in an update but in a deletion because the informer this handler is registered to has a field selector on the
		// value of this node. The only changes to a NetworkAttachment that cause an update and a change in the fields of its enquable
		// reference are those to its VNI value. In this case, the old object and the updated object have different references. We enqueue
		// both so that not only the new data structures are allocated, but the old ones are deallocated (by two separate worker threads).
		oldLocalAttRef := ca.fromAttToEnquableRef(oldLocalAtt)
		ca.queue.Add(oldLocalAttRef)
	}
}

func (ca *ConnectionAgent) OnLocalAttRemoved(obj interface{}) {
	localAtt := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v removal from local NetworkAttachments cache\n", localAtt)
	localAttRef := ca.fromAttToEnquableRef(localAtt)
	ca.queue.Add(localAttRef)
}

func (ca *ConnectionAgent) initLocalAttsInformerAndLister() {
	localAttWithAnIPSelector := ca.localAttWithAnIPSelector()
	glog.V(6).Info("Created Local NetworkAttachments fields selector: " + localAttWithAnIPSelector)

	ca.localAttsInformer, ca.localAttsLister = v1a1AttsCustomInformerAndLister(ca.kcs,
		resyncPeriod,
		fromFieldsSelectorToTweakListOptionsFunc(localAttWithAnIPSelector))

	ca.localAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		ca.OnLocalAttAdded,
		ca.OnLocalAttUpdated,
		ca.OnLocalAttRemoved})
}

func (ca *ConnectionAgent) startLocalAttsInformer(stopCh <-chan struct{}) {
	go ca.localAttsInformer.Run(stopCh)
}

func (ca *ConnectionAgent) waitForLocalAttsCacheSync(stopCh <-chan struct{}) (err error) {
	if !k8scache.WaitForCacheSync(stopCh, ca.localAttsInformer.HasSynced) {
		err = fmt.Errorf("Caches failed to sync")
	}
	return
}

func (ca *ConnectionAgent) processQueue() {
	for {
		item, stop := ca.queue.Get()
		if stop {
			return
		}
		attRef := item.(attQueueRef)
		ca.processQueueItem(attRef)
	}
}

func (ctlr *ConnectionAgent) processQueueItem(attRef attQueueRef) {
	defer ctlr.queue.Done(attRef)
	// TODO the following lines are only for temporary debugging. Remove when you're done.
	var localOrRemote string
	if attRef.local {
		localOrRemote = "local"
	} else {
		localOrRemote = "remote"
	}
	glog.Infof("Processing %s NetworkAttachment %s in VNI %d\n", localOrRemote, attRef.nsn.String(), attRef.vni)
}

func (ca *ConnectionAgent) fromAttToEnquableRef(att *netv1a1.NetworkAttachment) (enquableRef attQueueRef) {
	enquableRef.nsn = attNSN(att)
	enquableRef.vni = att.Spec.VNI
	if att.Spec.Node == ca.localNodeName {
		enquableRef.local = true
	} else {
		enquableRef.local = false
	}
	return
}

// Return a string representing a fields.Selector that matches NetworkAttachments
// that run on the local node and have a virtual IP.
func (ca *ConnectionAgent) localAttWithAnIPSelector() string {
	// localAttSelector expresses the constraint that the NetworkAttachment runs on this node.
	localAttSelector := attNodeFieldName + equal + ca.localNodeName

	// Express the constraint that the NetworkAttachment has a virtual IP by saying
	// that the field containig the virtual IP must not be equal to the empty string.
	attWithAnIPSelector := attIPFieldName + notEqual

	// Build a selector which is a logical AND between
	// attWithAnIPSelectorString and localAttSelectorString.
	allSelectors := []string{localAttSelector, attWithAnIPSelector}
	localAttWithAnIPSelector := strings.Join(allSelectors, ",")

	return localAttWithAnIPSelector
}

// TODO add comments
func fromFieldsSelectorToTweakListOptionsFunc(customFieldSelector string) kosinternalifcs.TweakListOptionsFunc {
	return func(options *k8smetav1.ListOptions) {
		// TODO if a selector is both in options.FieldSelector and customFieldsSelector it's repeated in the
		// resulting selector. This is not incorrect but it's redundant (and less efficient I suspect).
		// Fix this: if a selector appears both in options.FieldSelector and customFieldSelector it must
		// appear in the resulting selector only once.
		allSelectors := []string{options.FieldSelector, customFieldSelector}
		options.FieldSelector = strings.Join(allSelectors, ",")
	}
}

// TODO refactor this method to make it share code with v1a1AttsCustomNamespaceInformerAndLister
func v1a1AttsCustomInformerAndLister(kcs *kosclientset.Clientset,
	resyncPeriod time.Duration,
	tweakListOptionsFunc kosinternalifcs.TweakListOptionsFunc) (k8scache.SharedIndexInformer, koslisterv1a1.NetworkAttachmentLister) {

	// We are using a shared informer factory even if the informers we are using are not actually shared:
	// their type is "SharedIndexInformer" but they do not share the cache or the connection to the API server.
	// The only sharing which takes place is between an Informer and the corresponding lister. The reason we
	// are doing this is that it's not intuitive how to make Non Shared Informers leveraging the boilerplate code
	// generated by hack/update-codegen.sh. We should think more thoroughly about this, and choose one of the
	// following alternatives: (1) Leave this code as it is, (2) if it's possible, use the code generated
	// by update-codegen.sh to create and init non-shared informers for network.example.com API group objects,
	// (3) Use non-typed informers/listers manually (without the generated boilerplate).
	attsInformerFactory := kosinformers.NewFilteredSharedInformerFactory(kcs,
		resyncPeriod,
		k8smetav1.NamespaceAll,
		tweakListOptionsFunc)
	netv1a1Ifc := attsInformerFactory.Network().V1alpha1()
	attv1a1Informer := netv1a1Ifc.NetworkAttachments()

	return attv1a1Informer.Informer(), attv1a1Informer.Lister()
}

// TODO refactor this method to make it share code with v1a1AttsCustomInformerAndLister
func v1a1AttsCustomNamespaceInformerAndLister(kcs *kosclientset.Clientset,
	resyncPeriod time.Duration,
	namespace string,
	tweakListOptionsFunc kosinternalifcs.TweakListOptionsFunc) (k8scache.SharedIndexInformer, koslisterv1a1.NetworkAttachmentNamespaceLister) {

	// We are using a shared informer factory even if the informers we are using are not actually shared:
	// their type is "SharedIndexInformer" but they do not share the cache or the connection to the API server.
	// The only sharing which takes place is between an Informer and the corresponding lister. The reason we
	// are doing this is that it's not intuitive how to make Non Shared Informers leveraging the boilerplate code
	// generated by hack/update-codegen.sh. We should think more thoroughly about this, and choose one of the
	// following alternatives: (1) Leave this code as it is, (2) if it's possible, use the code generated
	// by update-codegen.sh to create and init non-shared informers for network.example.com API group objects,
	// (3) Use non-typed informers/listers manually.
	localAttsInformerFactory := kosinformers.NewFilteredSharedInformerFactory(kcs,
		resyncPeriod,
		namespace,
		tweakListOptionsFunc)
	netv1a1Ifc := localAttsInformerFactory.Network().V1alpha1()
	attv1a1Informer := netv1a1Ifc.NetworkAttachments()

	return attv1a1Informer.Informer(), attv1a1Informer.Lister().NetworkAttachments(namespace)
}

func attNSN(netAtt *netv1a1.NetworkAttachment) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{netAtt.Namespace, netAtt.Name}
}
