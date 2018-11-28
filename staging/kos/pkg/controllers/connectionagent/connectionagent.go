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
	gonet "net"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8swait "k8s.io/apimachinery/pkg/util/wait"
	k8scache "k8s.io/client-go/tools/cache"
	k8sworkqueue "k8s.io/client-go/util/workqueue"

	netv1a1 "k8s.io/examples/staging/kos/pkg/apis/network/v1alpha1"
	kosclientset "k8s.io/examples/staging/kos/pkg/client/clientset/versioned"
	netvifc1a1 "k8s.io/examples/staging/kos/pkg/client/clientset/versioned/typed/network/v1alpha1"
	kosinformers "k8s.io/examples/staging/kos/pkg/client/informers/externalversions"
	kosinternalifcs "k8s.io/examples/staging/kos/pkg/client/informers/externalversions/internalinterfaces"
	kosinformersv1a1 "k8s.io/examples/staging/kos/pkg/client/informers/externalversions/network/v1alpha1"
	koslisterv1a1 "k8s.io/examples/staging/kos/pkg/client/listers/network/v1alpha1"
	netfabric "k8s.io/examples/staging/kos/pkg/networkfabric"
)

const (
	// K8s API server reosurce name for NetworkAttachments.
	attResourceName = "networkattachments"

	// NetworkAttachments in network.example.com/v1alpha1
	// fields names. Used to build field selectors.
	attNodeFieldName   = "spec.node"
	attVNIFieldName    = "spec.vni"
	attHostIPFieldName = "status.hostIP"
	attIPFieldName     = "status.ipv4"

	// fields selector comparison operators.
	// Used to build fields selectors.
	equal    = "="
	notEqual = "!="

	// resync period for Informers caches. Set
	// to 0 because we don't want resyncs.
	resyncPeriod = 0
)

// vnState stores all the state needed for a Virtual Network for
// which there is at least one NetworkAttachment local to this node.
type vnState struct {
	// mutex is used to access the map localAtts
	// and remoteAttsInformer when starting it.
	mutex sync.RWMutex

	// remoteAttsInformer is an informer on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not on
	// this node. It is stopped when the last local NetworkAttachment in the
	// Virtual Network associated with the vnState instance is deleted. To
	// stop it, remoteAttsInformerStopCh must be closed.
	remoteAttsInformer       k8scache.SharedInformer
	remoteAttsInformerStopCh chan struct{}

	// remoteAttsLister is a lister on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not
	// on this node. Since a Virtual Network cannot span multiple k8s API
	// namespaces, it's a NamespaceLister.
	remoteAttsLister koslisterv1a1.NetworkAttachmentNamespaceLister

	// localAtts stores the namespaced names the local NetworkAttachments in the Virtual
	// Network the vnState represents. A local attachment is added to it when
	// it is seen for the first time, and removed from it when it is deleted
	// from the local attachments informer cache. Upon being removed, a check
	// on whether the attachment is the last one in localAtts is made, and if that's
	// the case remoteAttsInformer is stopped by closing remoteAttsInformerStopCh,
	// and vnState is removed from the map storing all the vnStates localAtts is used
	// as a set where the elements are the keys, the map values can be ignored.
	// TODO we should implement and use a set rather than a map for this
	localAtts map[k8stypes.NamespacedName]interface{}
}

// vniAndNsn is used as the type for the keys in the maps
// storing Network Interfaces of the NetworkAttachments.
// Using a Namespaced name only as the key is not enough
// because when you have updates to the vni field two different
// references are enqueued (one for the attachment version with the
// old vni and one for the attachment version with the new vni).
// In such cases there is the risk of race conditions if the key
// is represented by a namespaced name only.
type vniAndNsn struct {
	vni uint32
	nsn k8stypes.NamespacedName
}

// attQueueRef is a queue reference to a Network Attachment. Upon
// dequeuing a reference, a worker goroutine can lookup for the
// attachment in different caches: the local Attachments cache, or
// one of the caches for remote attachments (one for each Virtual
// Network relevant to the Connection Agent). That's why attQueueRef
// stores a locality flag (true if lookup for the attachment must be
// done in the local attachments cache) and a vni, which identifies
// the remote attachments cache where to lookup if the locality flag
// is false.
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
	netv1a1Ifc    netvifc1a1.NetworkV1alpha1Interface
	queue         k8sworkqueue.RateLimitingInterface
	workers       int
	netFabric     netfabric.Interface
	stopCh        <-chan struct{}

	localAttsInformer k8scache.SharedIndexInformer
	localAttsLister   koslisterv1a1.NetworkAttachmentLister

	vniToVnStateMutex sync.RWMutex
	vniToVnState      map[uint32]*vnState

	localIfcsMutex sync.RWMutex
	localIfcs      map[vniAndNsn]netfabric.NetworkInterface

	remoteIfcsMutex sync.RWMutex
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
		netv1a1Ifc:    kcs.NetworkV1alpha1(),
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

	ca.stopCh = stopCh
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
		go k8swait.Until(ca.processQueue, time.Second, stopCh)
	}
	glog.V(2).Infof("Launched %d workers\n", ca.workers)

	<-stopCh
	return nil
}

func (ca *ConnectionAgent) initLocalAttsInformerAndLister() {
	localAttWithAnIPSelector := ca.localAttWithAnIPSelector()
	glog.V(6).Info("Created Local NetworkAttachments fields selector: " + localAttWithAnIPSelector)

	ca.localAttsInformer, ca.localAttsLister = v1a1AttsCustomInformerAndLister(ca.kcs,
		resyncPeriod,
		fromFieldsSelectorToTweakListOptionsFunc(localAttWithAnIPSelector))

	ca.localAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    ca.onLocalAttAdded,
		UpdateFunc: ca.onLocalAttUpdated,
		DeleteFunc: ca.onLocalAttRemoved})
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

func (ca *ConnectionAgent) processQueueItem(attRef attQueueRef) {
	defer ca.queue.Done(attRef)
	var err error
	var localStr string
	if attRef.local {
		localStr = "local"
		err = ca.processLocalAtt(attRef)
	} else {
		localStr = "remote"
		err = ca.processRemoteAtt(attRef)
	}
	requeues := ca.queue.NumRequeues(attRef.nsn)
	if err != nil {
		glog.Warningf("Failed processing %s NetworkAttachment %s in VN with ID %d, requeuing (%d earlier requeues): %s\n",
			localStr,
			attRef.nsn,
			attRef.vni,
			requeues,
			err.Error())
		ca.queue.AddRateLimited(attRef)
		return
	}
	glog.V(4).Infof("Finished %s NetworkAttachment %s in VN with ID %d with %d requeues\n", localStr, attRef.nsn, attRef.vni, requeues)
	ca.queue.Forget(attRef)
}

func (ca *ConnectionAgent) onLocalAttAdded(obj interface{}) {
	localAtt := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v addition to local NetworkAttachments cache\n", localAtt)
	localAttRef := ca.fromAttToEnquableRef(localAtt)
	ca.queue.Add(localAttRef)
}

// TODO make the enqueuing conditional on a check on whether the update is relevant (e.g. an update which adds a metadata
// label is completely irrelevant and should not trigger an enqueuing)
func (ca *ConnectionAgent) onLocalAttUpdated(oldObj, newObj interface{}) {
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
		// both so that not only the new data structures (e.g. Network Interface) are allocated, but the old ones are deallocated (by two
		// separate worker threads).
		oldLocalAttRef := ca.fromAttToEnquableRef(oldLocalAtt)
		ca.queue.Add(oldLocalAttRef)
	}
}

func (ca *ConnectionAgent) onLocalAttRemoved(obj interface{}) {
	peeledObj := peel(obj)
	localAtt := peeledObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v removal from local NetworkAttachments cache\n", localAtt)
	localAttRef := ca.fromAttToEnquableRef(localAtt)
	ca.queue.Add(localAttRef)
}

func (ca *ConnectionAgent) processLocalAtt(attRef attQueueRef) error {
	localAtt, err := ca.localAttsLister.NetworkAttachments(attRef.nsn.Namespace).Get(attRef.nsn.Name)
	if err != nil && !k8serrors.IsNotFound(err) {
		// This should never happen.  No point in retrying.
		glog.Errorf("NetworkAttachments Lister failed to lookup %s/%s: %s\n",
			attRef.nsn.Namespace, attRef.nsn.Name, err.Error())
		return nil
	}
	if err != nil && k8serrors.IsNotFound(err) {
		// If we're here the attachment has been deleted
		err = ca.releaseLocalAttResources(attRef)
		return err
	}
	if err == nil && attRef.vni != localAtt.Spec.VNI {
		// If we are here there's been an update on the vni field, and we must clear
		// the resources associated with localAtt when it had the old vni field value.
		err = ca.releaseLocalAttResources(attRef)
	}
	err = ca.processExistingLocalAtt(localAtt)
	return err
}

func (ca *ConnectionAgent) processRemoteAtt(attRef attQueueRef) error {
	stateOfAttVN, stateFound := ca.getVNStateForVNI(attRef.vni)
	ifcKey := fromAttRefToVNIAndNsn(attRef)
	if !stateFound {
		// If we're here the last local NetworkAttachment in the Virtual Network of the remote attachment
		// referenced by attRef has been deleted => the attachment by attRef is no longer relevant for
		// the connection agent.
		deletedIfc, ifcFound, err := ca.deleteRemoteIfc(ifcKey)
		if ifcFound {
			glog.V(3).Infof("Deleted remote Network Interface %#+v\n", deletedIfc)
		}
		return err
	}
	remoteAtt, err := stateOfAttVN.remoteAttsLister.Get(attRef.nsn.Name)
	if err != nil && !k8serrors.IsNotFound(err) {
		// This should never happen. No point in retrying.
		return nil
	}
	if err != nil && k8serrors.IsNotFound(err) {
		// If we're here the remote attachment has been deleted.
		deletedIfc, ifcFound, err := ca.deleteRemoteIfc(ifcKey)
		if ifcFound {
			glog.V(3).Infof("Deleted remote Network Interface %#+v\n", deletedIfc)
		}
		return err
	}
	return ca.createOrUpdateRemoteAttIfc(remoteAtt)
}

func (ca *ConnectionAgent) releaseLocalAttResources(attRef attQueueRef) error {
	ca.updateVNStateForDeletedLocalAtt(attRef)
	deletedIfc, ifcFound, err := ca.deleteLocalIfc(fromAttRefToVNIAndNsn(attRef))
	if err == nil && ifcFound {
		glog.V(3).Infof("Deleted local Network Interface %#+v\n", deletedIfc)
	}
	return err
}

// TODO break this method body into smaller methods, possibly shared with createOrUpdateRemoteAttIfc. Refactor it, make it more readable.
func (ca *ConnectionAgent) processExistingLocalAtt(localAtt *netv1a1.NetworkAttachment) error {
	ca.updateVNStateForExistingLocalAtt(localAtt)
	ifcKey := fromAttToVNIAndNsn(localAtt)
	oldIfc, ifcFound := ca.getLocalIfc(ifcKey)
	var ifcNeedsUpdate bool
	if ifcFound {
		ifcNeedsUpdate = ca.ifcUpdateIsNeeded(oldIfc, localAtt)
	}
	var err1 error
	var ifcWasUpdated bool
	if ifcNeedsUpdate {
		// Delete the old interface
		// TODO think about adding an "UpdateIfc" method to the network fabric
		err1 = ca.netFabric.DeleteLocalIfc(oldIfc)
		if err1 != nil {
			return err1
		}
		ca.unsetLocalIfc(ifcKey)
		ifcWasUpdated = true
	}
	var newIfc netfabric.NetworkInterface
	if !ifcFound || ifcNeedsUpdate {
		// Create a new interface
		// TODO could not think of a better way to generate a name. Think this through.
		newIfc.Name = fmt.Sprintf("%d-%s/%s", localAtt.Spec.VNI, localAtt.Namespace, localAtt.Name)
		newIfc.VNI = localAtt.Spec.VNI
		newIfc.HostIP = gonet.ParseIP(ca.hostIP)
		guestIP := gonet.ParseIP(localAtt.Status.IPv4)
		newIfc.GuestIP = guestIP
		newIfc.GuestMAC = generateMACAddr(localAtt.Spec.VNI, guestIP)
		err1 = ca.netFabric.CreateLocalIfc(newIfc)
		if err1 == nil {
			ca.setLocalIfc(newIfc, ifcKey)
			ifcWasUpdated = true
		}
	}
	// Do a conditional update on the status of the NetworkAttachment
	var err2 error
	if localAtt.Status.HostIP != ca.hostIP || (ifcWasUpdated && (newIfc.Name != oldIfc.Name)) {
		var ifcNameForStatus string
		if ifcWasUpdated {
			ifcNameForStatus = newIfc.Name
		} else {
			ifcNameForStatus = oldIfc.Name
		}
		var updatedAtt *netv1a1.NetworkAttachment
		updatedAtt, err2 = ca.setHostIPAndIfcNameInAttStatus(localAtt, ifcNameForStatus)
		if err2 == nil {
			glog.V(4).Infof("Set host IP %s and interface name %s in local NetworkAttachment %s/%s status",
				updatedAtt.Status.HostIP,
				updatedAtt.Status.IfcName,
				updatedAtt.Namespace,
				updatedAtt.Name)
		}
	}
	_, errToReturn := aggregateErrors("\n\t", err1, err2)
	return errToReturn
}

// TODO the algorithm in this method is terribly complicated. Try to simplify it and review it to make sure it's correct
func (ca *ConnectionAgent) updateVNStateForExistingLocalAtt(localAtt *netv1a1.NetworkAttachment) {
	nsn := attNSN(localAtt)
	vni := localAtt.Spec.VNI

	state, stateExists := ca.getVNStateForVNI(vni)
	var attIsInState bool
	if stateExists {
		state.mutex.RLock()
		_, attIsInState = state.localAtts[nsn]
		state.mutex.RUnlock()
	}
	if attIsInState {
		// If we are here localAtt is already marked as present in the state of its Virtual Network => The informer on remote
		// attachments is running and the Virtual Network state will not be cleared by the deletion of another attachment, because
		// the worker processing the deleted attachment will see that there's still localAtt.
		return
	}

	if !stateExists || !attIsInState {
		ca.vniToVnStateMutex.Lock()
		// Do another lookup because since the previous one another worker might have initialized
		// the Virtual Network state if it was missing or deleted it if it was there.
		state, stateExists = ca.vniToVnState[vni]
		if !stateExists {
			// The state for the Virtual Network localAtt is part of does not exist, so we create a new one
			// and set it here without risk of race conditions because we hold the lock on ca.vniToVnStateMutex.
			newVNState := &vnState{
				localAtts: make(map[k8stypes.NamespacedName]interface{}, 1),
			}
			ca.vniToVnState[vni] = newVNState
			newVNState.mutex.Lock()
			ca.vniToVnStateMutex.Unlock()
			// Add localAtts amongst the attachments in the Virtual Network state.
			newVNState.localAtts[nsn] = nil
			ca.initRemoteAttsInformerAndLister(newVNState, nsn, vni)
			ca.startRemoteAttsInformer(newVNState)
			newVNState.mutex.Unlock()
			// TODO make these prints a single one
			glog.V(5).Infof("Started informer on Remote attachments in Virtual Network with ID %d\n", vni)
			glog.V(5).Infof("Created Virtual Network with ID %d state and added local NetworkAttachment %s to it", vni, nsn)
		} else {
			// If we are here the state for the Virtual Network localAtt is part of exists, but localAtts is not in it yet.
			// The following Lock() invocation is really bad for performance because it might be blocking: we're blocked
			// while holding the lock on ca.vniToVnStateMutex which is acquired by every worker for every notification.
			// It's a bottleneck, at some point we'll need to address this.
			state.mutex.Lock()
			state.localAtts[nsn] = nil
			state.mutex.Unlock()
			ca.vniToVnStateMutex.Unlock()
			glog.V(5).Infof("Added local NetworkAttachment %s to Virtual Network with ID %d state", nsn, vni)
		}
	}
}

// TODO the algorithm in this method is terribly complicated. Try to simplify it and review it to make sure it's correct
func (ca *ConnectionAgent) updateVNStateForDeletedLocalAtt(deletedAttRef attQueueRef) {
	state, stateExists := ca.getVNStateForVNI(deletedAttRef.vni)
	if !stateExists {
		return
	}
	lastLocalAttInVN := ca.checkIfLastLocalAttInVNAndRemoveAttIfNot(deletedAttRef, state)
	if lastLocalAttInVN {
		ca.vniToVnStateMutex.Lock()
		// state must have been found because deletedAttRef is still in it, so it's impossible that another
		// worker has removed it
		state, _ = ca.vniToVnState[deletedAttRef.vni]
		state.mutex.Lock()
		delete(state.localAtts, deletedAttRef.nsn)
		if len(state.localAtts) > 0 {
			// deletedAttRef is no longer the last one, another attachment has been added to the state
			state.mutex.Unlock()
			ca.vniToVnStateMutex.Unlock()
			return
		}
		// deletedAttRef is the last one in the virtual network state: proceed to remove the virtual netowrk state from the map
		// storing all the states stop informer
		delete(ca.vniToVnState, deletedAttRef.vni)
		ca.vniToVnStateMutex.Unlock()
		ca.stopInformerAndEnqueueRemoteAttRefs(state)
		state.mutex.Unlock()
		// TODO make these prints a single one
		glog.V(5).Infof("Stopped informer on Remote attachments in Virtual Network with ID %d\n", deletedAttRef.vni)
		glog.V(5).Infof("Deleted Virtual Network with ID %d state", deletedAttRef.vni)
	} else {
		glog.V(5).Infof("Removed local NetworkAttachment %s from Virtual Network with ID %d state", deletedAttRef.nsn, deletedAttRef.vni)
	}
}

func (ca *ConnectionAgent) initRemoteAttsInformerAndLister(state *vnState, nsn k8stypes.NamespacedName, vni uint32) {
	state.remoteAttsInformer, state.remoteAttsLister = v1a1AttsCustomNamespaceInformerAndLister(ca.kcs,
		resyncPeriod,
		nsn.Namespace,
		fromFieldsSelectorToTweakListOptionsFunc(ca.remoteAttInVNWithVirtualIPAndHostIPSelector(vni)))
	state.remoteAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    ca.onRemoteAttAdded,
		UpdateFunc: ca.onRemoteAttUpdated,
		DeleteFunc: ca.onRemoteAttRemoved})
}

func (ca *ConnectionAgent) onRemoteAttAdded(obj interface{}) {
	remoteAtt := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v addition to remote NetworkAttachments cache for VNI %d\n", remoteAtt, remoteAtt.Spec.VNI)
	remoteAttRef := ca.fromAttToEnquableRef(remoteAtt)
	ca.queue.Add(remoteAttRef)
}

// TODO make the enqueuing conditional on a check on whether the update is relevant
// (e.g. an update which adds a metadata label is completely irrelevant and should not
// trigger an enqueuing)
func (ca *ConnectionAgent) onRemoteAttUpdated(oldObj, newObj interface{}) {
	oldRemoteAtt := oldObj.(*netv1a1.NetworkAttachment)
	newRemoteAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment update from %#+v to %#+v in remote NetworkAttachments cache\n", oldRemoteAtt, newRemoteAtt)
	newRemoteAttRef := ca.fromAttToEnquableRef(newRemoteAtt)
	ca.queue.Add(newRemoteAttRef)
}

func (ca *ConnectionAgent) onRemoteAttRemoved(obj interface{}) {
	peeledObj := peel(obj)
	remoteAtt := peeledObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v removal from remote NetworkAttachments cache for VNI %d\n", remoteAtt, remoteAtt.Spec.VNI)
	remoteAttRef := ca.fromAttToEnquableRef(remoteAtt)
	ca.queue.Add(remoteAttRef)
}

func (ca *ConnectionAgent) startRemoteAttsInformer(state *vnState) {
	state.remoteAttsInformerStopCh = make(chan struct{})
	go state.remoteAttsInformer.Run(aggregateStopChannels(ca.stopCh, state.remoteAttsInformerStopCh))
}

func (ca *ConnectionAgent) getVNStateForVNI(vni uint32) (state *vnState, vnStateExists bool) {
	ca.vniToVnStateMutex.RLock()
	defer ca.vniToVnStateMutex.RUnlock()
	state, vnStateExists = ca.vniToVnState[vni]
	return
}

func (ca *ConnectionAgent) checkIfLastLocalAttInVNAndRemoveAttIfNot(deletedAttRef attQueueRef, state *vnState) (lastAtt bool) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	_, attFound := state.localAtts[deletedAttRef.nsn]
	if !attFound {
		return
	}
	if len(state.localAtts) > 1 {
		delete(state.localAtts, deletedAttRef.nsn)
		return
	}
	lastAtt = true
	return
}

func (ca *ConnectionAgent) stopInformerAndEnqueueRemoteAttRefs(state *vnState) {
	remoteAtts, err := state.remoteAttsLister.List(k8slabels.Everything())
	close(state.remoteAttsInformerStopCh)
	if err != nil {
		// This should never happen. No point in retrying. If it does happen though, all
		// the remote interfaces that should be deleted for the given Virtual Network on
		// the node are not deleted, which is really bad. Think this through.
		return
	}
	for _, remoteAtt := range remoteAtts {
		ca.queue.Add(ca.fromAttToEnquableRef(remoteAtt))
	}
}

// TODO break this method body into smaller methods, possibly shared with processExistingLocalAtt
func (ca *ConnectionAgent) createOrUpdateRemoteAttIfc(remoteAtt *netv1a1.NetworkAttachment) error {
	ifcKey := fromAttToVNIAndNsn(remoteAtt)
	oldIfc, ifcFound := ca.getRemoteIfc(ifcKey)
	var ifcNeedsUpdate bool
	if ifcFound {
		ifcNeedsUpdate = ca.ifcUpdateIsNeeded(oldIfc, remoteAtt)
	}
	var err error
	if ifcNeedsUpdate {
		// delete the old interface
		err = ca.netFabric.DeleteRemoteIfc(oldIfc)
		if err != nil {
			return err
		}
		ca.unsetRemoteIfc(ifcKey)
	}
	if !ifcFound || ifcNeedsUpdate {
		// create a new interface
		guestIP := gonet.ParseIP(remoteAtt.Status.IPv4)
		newIfc := netfabric.NetworkInterface{
			// TODO Could not think of a better way to generate a name. Think this through.
			Name:     fmt.Sprintf("%d-%s/%s", remoteAtt.Spec.VNI, remoteAtt.Namespace, remoteAtt.Name),
			VNI:      remoteAtt.Spec.VNI,
			HostIP:   gonet.ParseIP(remoteAtt.Status.HostIP),
			GuestIP:  guestIP,
			GuestMAC: generateMACAddr(remoteAtt.Spec.VNI, guestIP)}
		err := ca.netFabric.CreateRemoteIfc(newIfc)
		if err == nil {
			ca.setRemoteIfc(newIfc, ifcKey)
		}
	}
	return err
}

func (ca *ConnectionAgent) setHostIPAndIfcNameInAttStatus(att *netv1a1.NetworkAttachment, ifcName string) (*netv1a1.NetworkAttachment, error) {
	att2 := att.DeepCopy()
	att2.Status.HostIP = ca.hostIP
	att2.Status.IfcName = ifcName
	att3, err := ca.netv1a1Ifc.NetworkAttachments(att2.Namespace).Update(att2)
	return att3, err
}

func (ca *ConnectionAgent) ifcUpdateIsNeeded(ifc netfabric.NetworkInterface, att *netv1a1.NetworkAttachment) bool {
	if ifc.GuestIP.String() != att.Status.IPv4 {
		return true
	}
	if ifc.HostIP.String() != att.Status.HostIP {
		return true
	}
	if ifc.VNI != att.Spec.VNI {
		return true
	}
	return false
}

func (ca *ConnectionAgent) deleteLocalIfc(ifcKey vniAndNsn) (ifc netfabric.NetworkInterface, ifcFound bool, err error) {
	ifc, ifcFound = ca.getLocalIfc(ifcKey)
	if !ifcFound {
		return
	}
	err = ca.netFabric.DeleteLocalIfc(ifc)
	if err == nil {
		ca.unsetLocalIfc(ifcKey)
	}
	return
}

func (ca *ConnectionAgent) deleteRemoteIfc(ifcKey vniAndNsn) (ifc netfabric.NetworkInterface, ifcFound bool, err error) {
	ifc, ifcFound = ca.getRemoteIfc(ifcKey)
	if !ifcFound {
		return
	}
	err = ca.netFabric.DeleteRemoteIfc(ifc)
	if err == nil {
		ca.unsetRemoteIfc(ifcKey)
	}
	return
}

func (ca *ConnectionAgent) setLocalIfc(ifc netfabric.NetworkInterface, ifcKey vniAndNsn) {
	ca.localIfcsMutex.Lock()
	defer ca.localIfcsMutex.Unlock()
	ca.localIfcs[ifcKey] = ifc
	return
}

func (ca *ConnectionAgent) setRemoteIfc(ifc netfabric.NetworkInterface, ifcKey vniAndNsn) {
	ca.remoteIfcsMutex.Lock()
	defer ca.remoteIfcsMutex.Unlock()
	ca.remoteIfcs[ifcKey] = ifc
	return
}

func (ca *ConnectionAgent) unsetLocalIfc(ifcKey vniAndNsn) {
	ca.localIfcsMutex.Lock()
	defer ca.localIfcsMutex.Unlock()
	delete(ca.localIfcs, ifcKey)
}

func (ca *ConnectionAgent) unsetRemoteIfc(ifcKey vniAndNsn) {
	ca.remoteIfcsMutex.Lock()
	defer ca.remoteIfcsMutex.Unlock()
	delete(ca.remoteIfcs, ifcKey)
}

func (ca *ConnectionAgent) getLocalIfc(ifcKey vniAndNsn) (ifc netfabric.NetworkInterface, ifcFound bool) {
	ca.localIfcsMutex.RLock()
	defer ca.localIfcsMutex.RUnlock()
	ifc, ifcFound = ca.localIfcs[ifcKey]
	return
}

func (ca *ConnectionAgent) getRemoteIfc(ifcKey vniAndNsn) (ifc netfabric.NetworkInterface, ifcFound bool) {
	ca.remoteIfcsMutex.RLock()
	defer ca.remoteIfcsMutex.RUnlock()
	ifc, ifcFound = ca.remoteIfcs[ifcKey]
	return
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

// Return a string representing a field selector that matches NetworkAttachments
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

// Return a string representing a field selector that matches NetworkAttachments
// that run on a remote node on the Virtual Network identified by the given VNI and
// have a virtual IP and the host IP field set.
func (ca *ConnectionAgent) remoteAttInVNWithVirtualIPAndHostIPSelector(vni uint32) string {
	// remoteAttSelector expresses the constraint that the NetworkAttachment runs on a remote node.
	remoteAttSelector := attNodeFieldName + notEqual + ca.localNodeName

	// attWithAnIPSelector expresses the constraint that the NetworkAttachment has a virtual IP
	// by saying that the field containig the virtual IP must not be equal to the empty string.
	attWithAnIPSelector := attIPFieldName + notEqual

	// attWithHostIPSet expresses the constraint that the NetworkAttachment has the host IP field
	// set by saying that the field containig the host IP must not be equal to the empty string.
	attWithHostIPSet := attHostIPFieldName + notEqual

	// attInSpecificVN expresses the constraint that the NetworkAttachment is in the Virtual
	// Network identified by vni.
	attInSpecificVN := attVNIFieldName + equal + fmt.Sprint(vni)

	// Build a selector which is a logical AND between all the selectors defined above.
	allSelectors := []string{remoteAttSelector, attWithAnIPSelector, attWithHostIPSet, attInSpecificVN}
	localAttWithAnIPSelector := strings.Join(allSelectors, ",")

	return localAttWithAnIPSelector
}

// TODO add comments
func fromFieldsSelectorToTweakListOptionsFunc(customFieldSelector string) kosinternalifcs.TweakListOptionsFunc {
	return func(options *k8smetav1.ListOptions) {
		// TODO if a selector is both in options.FieldSelector and customFieldsSelector it appears twice in the
		// resulting selector. This is not incorrect but it's redundant (and less efficient I suspect).
		// Fix this: if a selector appears both in options.FieldSelector and customFieldSelector it must
		// appear in the resulting selector only once.
		optionsFieldSelector := options.FieldSelector
		allSelectors := make([]string, 0, 2)
		if strings.Trim(optionsFieldSelector, " ") != "" {
			allSelectors = append(allSelectors, optionsFieldSelector)
		}
		allSelectors = append(allSelectors, customFieldSelector)
		options.FieldSelector = strings.Join(allSelectors, ",")
	}
}

// TODO add comments
func v1a1AttsCustomInformerAndLister(kcs *kosclientset.Clientset,
	resyncPeriod time.Duration,
	tweakListOptionsFunc kosinternalifcs.TweakListOptionsFunc) (k8scache.SharedIndexInformer, koslisterv1a1.NetworkAttachmentLister) {

	attv1a1Informer := createAttsv1a1Informer(kcs,
		resyncPeriod,
		k8smetav1.NamespaceAll,
		tweakListOptionsFunc)
	return attv1a1Informer.Informer(), attv1a1Informer.Lister()
}

// TODO add comments
func v1a1AttsCustomNamespaceInformerAndLister(kcs *kosclientset.Clientset,
	resyncPeriod time.Duration,
	namespace string,
	tweakListOptionsFunc kosinternalifcs.TweakListOptionsFunc) (k8scache.SharedIndexInformer, koslisterv1a1.NetworkAttachmentNamespaceLister) {

	attv1a1Informer := createAttsv1a1Informer(kcs,
		resyncPeriod,
		namespace,
		tweakListOptionsFunc)
	return attv1a1Informer.Informer(), attv1a1Informer.Lister().NetworkAttachments(namespace)
}

// TODO add comments
func createAttsv1a1Informer(kcs *kosclientset.Clientset,
	resyncPeriod time.Duration,
	namespace string,
	tweakListOptionsFunc kosinternalifcs.TweakListOptionsFunc) kosinformersv1a1.NetworkAttachmentInformer {

	localAttsInformerFactory := kosinformers.NewFilteredSharedInformerFactory(kcs,
		resyncPeriod,
		namespace,
		tweakListOptionsFunc)
	netv1a1Ifc := localAttsInformerFactory.Network().V1alpha1()
	return netv1a1Ifc.NetworkAttachments()
}

func attNSN(netAtt *netv1a1.NetworkAttachment) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{Namespace: netAtt.Namespace,
		Name: netAtt.Name}
}

func fromAttRefToVNIAndNsn(attRef attQueueRef) vniAndNsn {
	return vniAndNsn{
		attRef.vni,
		attRef.nsn,
	}
}

func fromAttToVNIAndNsn(att *netv1a1.NetworkAttachment) vniAndNsn {
	return vniAndNsn{
		att.Spec.VNI,
		attNSN(att),
	}
}

func generateMACAddr(vni uint32, guestIPv4 gonet.IP) gonet.HardwareAddr {
	guestIPBytes := guestIPv4.To4()
	return []byte{byte(vni >> 24), byte(vni >> 16), byte(vni >> 8), byte(vni), guestIPBytes[2], guestIPBytes[3]}
}

// peel removes the k8scache.DeletedFinalStateUnknown wrapper,
// if any, and returns the result as a k8sruntime.Object.
func peel(obj interface{}) k8sruntime.Object {
	switch o := obj.(type) {
	case *k8scache.DeletedFinalStateUnknown:
		return o.Obj.(k8sruntime.Object)
	case k8sruntime.Object:
		return o
	default:
		panic(obj)
	}
}

// aggregateStopChannels returns a channel which
// is closed when either ch1 or ch2 is closed
func aggregateStopChannels(ch1, ch2 <-chan struct{}) chan struct{} {
	aggregateStopCh := make(chan struct{})
	go func() {
		select {
		case _, ch1Open := <-ch1:
			if !ch1Open {
				close(aggregateStopCh)
				return
			}
		case _, ch2Open := <-ch2:
			if !ch2Open {
				close(aggregateStopCh)
				return
			}
		}
	}()
	return aggregateStopCh
}

func aggregateErrors(sep string, errs ...error) (bool, error) {
	aggregateErrsSlice := make([]string, 0, len(errs))
	for i, err := range errs {
		if err != nil && strings.Trim(err.Error(), " ") != "" {
			aggregateErrsSlice = append(aggregateErrsSlice, fmt.Sprintf("error nbr. %d ", i)+err.Error())
		}
	}
	if len(aggregateErrsSlice) > 0 {
		return true, fmt.Errorf("%s", strings.Join(aggregateErrsSlice, sep))
	}
	return false, nil
}
