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
	kosctlrutils "k8s.io/examples/staging/kos/pkg/controllers/utils"
	netfabric "k8s.io/examples/staging/kos/pkg/networkfabric"
)

const (
	// Name of the indexer which computes the MAC address
	// for a network attachment. Used for handling of
	// pre-existing interfaces at start-up.
	attMACIndexName = "attachmentMAC"

	// NetworkAttachments in network.example.com/v1alpha1
	// fields names. Used to build field selectors.
	attNodeFieldName   = "spec.node"
	attIPFieldName     = "status.ipv4"
	attHostIPFieldName = "status.hostIP"
	attVNIFieldName    = "spec.vni"

	// fields selector comparison operators.
	// Used to build fields selectors.
	equal    = "="
	notEqual = "!="

	// resync period for Informers caches. Set
	// to 0 because we don't want resyncs.
	resyncPeriod = 0

	// netFabricRetryPeriod is the time we wait before retrying when an
	// network fabric operation fails while handling pre-existing interfaces.
	netFabricRetryPeriod = time.Second
)

// vnState stores all the state needed for a Virtual Network for
// which there is at least one NetworkAttachment local to this node.
type vnState struct {
	// remoteAttsInformer is an informer on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not on
	// this node. It is stopped when the last local NetworkAttachment in the
	// Virtual Network associated with the vnState instance is deleted. To
	// stop it, remoteAttsInformerStopCh must be closed.
	remoteAttsInformer       k8scache.SharedIndexInformer
	remoteAttsInformerStopCh chan struct{}

	// remoteAttsLister is a lister on the NetworkAttachments that are
	// both: (1) in the Virtual Network the vnState represents, (2) not
	// on this node. Since a Virtual Network cannot span multiple k8s API
	// namespaces, it's a NamespaceLister.
	remoteAttsLister koslisterv1a1.NetworkAttachmentNamespaceLister

	// namespace is the namespace of the Virtual Network
	// associated with this vnState.
	namespace string

	// localAtts and remoteAtts store the names of the local and remote
	// NetworkAttachments in the Virtual Network the vnState represents,
	// respectively. localAtts is used to detect when the last local attachment
	// in the virtual network is deleted, so that remoteAttsInformer can be
	// stopped. remoteAtts is used to enqueue references to the remote
	// attachments in the Virtual Network when such Virtual Network becomes
	// irrelevant (deletion of last local attachment), so that the interfaces of
	// the remote attachments can be deleted.
	localAtts  map[string]struct{}
	remoteAtts map[string]struct{}
}

// attachmentState represents the state associated with a NetworkAttachment
// relevant to the ConnectionAgent.
type attachmentState struct {
	// vnStateVNI represents the last seen vni of the attachment, that is, the
	// vni associated with the vnState the attachment name is stored in. It is
	// used when processing an attachment because the attachment stored in the
	// Informer cache only carries the most recent vni. But if there's been an
	// update to the vni field, the attachment name must be removed by the
	// vnState associated with the old vni, hence vnStateVNI is used to store
	// the old vni value.
	vnStateVNI uint32

	// if ifcIsValid is true ifc stores the netfabric NetworkInterface for the
	// attachment. Used to delete the interface if the attachment is deleted and
	// to detect cases where the interface should be updated by comparing its
	// fields against those in the most recent version of the attachment.
	ifcIsValid bool
	ifc        netfabric.NetworkInterface
}

// attVNIsIntel represents the "intel" on the attachment current location (in
// terms of virtual network). It stores the VNIs with whom an attachment has been
// seen by the notification handlers on remote attachments.
// It's NOT THREAD-SAFE.
type attVNIsIntel struct {
	onlyVNI uint32
	vnis    map[uint32]struct{}
}

func newAttVNIsIntel() *attVNIsIntel {
	return &attVNIsIntel{
		vnis: make(map[uint32]struct{}, 1),
	}
}

func (avi *attVNIsIntel) addVNI(vni uint32) {
	avi.vnis[vni] = struct{}{}
	if len(avi.vnis) == 1 {
		avi.onlyVNI = vni
	}
}

func (avi *attVNIsIntel) removeVNI(vni uint32) {
	delete(avi.vnis, vni)
	if len(avi.vnis) == 1 {
		avi.onlyVNI = vni
	}
}

func (avi *attVNIsIntel) getIntel() (uint32, int) {
	return avi.onlyVNI, len(avi.vnis)
}

// ConnectionAgent represents a K8S controller which runs on every node of the
// cluster and eagerly maintains up-to-date the mapping between virtual IPs and
// physical IPs for every relevant NetworkAttachment. A NetworkAttachment is
// relevant to a connection agent if: (1) it runs on the same node as the
// connection agent, or (2) it's part of a Virtual Network where at least one
// NetworkAttachment for which (1) is true exists. To achieve its goal, a
// connection agent receives notifications about relevant NetworkAttachments
// from the K8s API server through Informers, and when necessary
// creates/updates/deletes Network Interfaces through a low-level network
// interface fabric. When a new Virtual Network becomes relevant for the
// connection agent because of the creation of the first attachment of that
// Virtual Network on the same node as the connection agent, a new informer on
// remote NetworkAttachments in that Virtual Network is created. Upon being
// notified of the creation of a local NetworkAttachment, the connection agent
// also updates the status of such attachment with its host IP and the name of
// the interface which was created.
type ConnectionAgent struct {
	localNodeName string
	hostIP        gonet.IP
	kcs           *kosclientset.Clientset
	netv1a1Ifc    netvifc1a1.NetworkV1alpha1Interface
	queue         k8sworkqueue.RateLimitingInterface
	workers       int
	netFabric     netfabric.Interface
	stopCh        <-chan struct{}

	// Informer and lister on NetworkAttachments on the same node as the
	// connection agent
	localAttsInformer k8scache.SharedIndexInformer
	localAttsLister   koslisterv1a1.NetworkAttachmentLister

	// Map from vni to vnState associated with that vni. Accessed only while
	// holding vniToVnStateMutex
	vniToVnStateMutex sync.RWMutex
	vniToVnState      map[uint32]*vnState

	// nsnToAttState maps attachments (both local and remote) to namespaced
	// names and state associated with that attachment. Accessed only while
	// holding nsnToAttStateMutex
	nsnToAttStateMutex sync.RWMutex
	nsnToAttState      map[k8stypes.NamespacedName]*attachmentState

	// nsnToVNIs maps attachments (both local and remote) to namespaced names
	// and set of vnis where the attachment has been seen. It is accessed by the
	// notification handlers for remote attachments, which add/remove the vni
	// with which they see the attachment upon creation/deletion of the attachment
	// respectively. When a worker processes an attachment reference, it reads
	// from nsnToVNIs the vnis with which the attachment has been seen. If there's
	// more than one vni, the current state of the attachment is ambiguous and
	// the worker stops the processing and returns an error. This causes the
	// reference to be requeued with a delay, and hopefully by the time it is
	// dequeued again the ambiguity as for the current attachment state has been
	// resolved. Accessed only while holding nsnToVNIsMutex.
	nsnToVNIsIntelMutex sync.RWMutex
	nsnToVNIsIntel      map[k8stypes.NamespacedName]*attVNIsIntel
}

// NewConnectionAgent returns a deactivated instance of a ConnectionAgent (neither
// the workers goroutines nor any Informer have been started). Invoke Run to activate.
func NewConnectionAgent(localNodeName string,
	hostIP gonet.IP,
	kcs *kosclientset.Clientset,
	queue k8sworkqueue.RateLimitingInterface,
	workers int,
	netFabric netfabric.Interface) *ConnectionAgent {

	return &ConnectionAgent{
		localNodeName:  localNodeName,
		hostIP:         hostIP,
		kcs:            kcs,
		netv1a1Ifc:     kcs.NetworkV1alpha1(),
		queue:          queue,
		workers:        workers,
		netFabric:      netFabric,
		vniToVnState:   make(map[uint32]*vnState),
		nsnToAttState:  make(map[k8stypes.NamespacedName]*attachmentState),
		nsnToVNIsIntel: make(map[k8stypes.NamespacedName]*attVNIsIntel),
	}
}

// Run activates the ConnectionAgent: the local attachments informer is started,
// pre-existing network interfaces on the node are handled, and the worker
// goroutines are started. Close stopCh to stop the ConnectionAgent.
func (ca *ConnectionAgent) Run(stopCh <-chan struct{}) error {
	defer k8sutilruntime.HandleCrash()
	defer ca.queue.ShutDown()

	ca.stopCh = stopCh
	ca.initLocalAttsInformerAndLister()
	go ca.localAttsInformer.Run(stopCh)
	glog.V(2).Infoln("Local NetworkAttachments Informer started")

	err := ca.waitForLocalAttsCacheSync(stopCh)
	if err != nil {
		return err
	}
	glog.V(2).Infoln("Local NetworkAttachments cache synced")

	err = ca.handlePreExistingIfcs()
	if err != nil {
		return err
	}
	glog.V(2).Infoln("Pre-existing interfaces synced")

	for i := 0; i < ca.workers; i++ {
		go k8swait.Until(ca.processQueue, time.Second, stopCh)
	}
	glog.V(2).Infof("Launched %d workers", ca.workers)

	<-stopCh
	return nil
}

func (ca *ConnectionAgent) initLocalAttsInformerAndLister() {
	localAttWithAnIPSelector := ca.localAttWithAnIPSelector()
	glog.V(6).Info("Created Local NetworkAttachments fields selector: " +
		localAttWithAnIPSelector)

	ca.localAttsInformer, ca.localAttsLister = v1a1AttsCustomInformerAndLister(ca.kcs,
		resyncPeriod,
		fromFieldsSelectorToTweakListOptionsFunc(localAttWithAnIPSelector))

	ca.localAttsInformer.AddIndexers(map[string]k8scache.IndexFunc{attMACIndexName: attachmentMACAddr})

	ca.localAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    ca.onLocalAttAdded,
		UpdateFunc: ca.onLocalAttUpdated,
		DeleteFunc: ca.onLocalAttRemoved,
	})
}

func (ca *ConnectionAgent) onLocalAttAdded(obj interface{}) {
	att := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Local NetworkAttachments cache: notified of addition of %#+v", att)
	ca.queue.Add(kosctlrutils.AttNSN(att))
}

func (ca *ConnectionAgent) onLocalAttUpdated(oldObj, newObj interface{}) {
	oldAtt := oldObj.(*netv1a1.NetworkAttachment)
	newAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Local NetworkAttachments cache: notified of update from %#+v to %#+v",
		oldAtt,
		newAtt)
	ca.queue.Add(kosctlrutils.AttNSN(newAtt))
}

func (ca *ConnectionAgent) onLocalAttRemoved(obj interface{}) {
	peeledObj := kosctlrutils.Peel(obj)
	att := peeledObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Local NetworkAttachments cache: notified of removal of %#+v", att)
	ca.queue.Add(kosctlrutils.AttNSN(att))
}

func (ca *ConnectionAgent) waitForLocalAttsCacheSync(stopCh <-chan struct{}) (err error) {
	if !k8scache.WaitForCacheSync(stopCh, ca.localAttsInformer.HasSynced) {
		err = fmt.Errorf("Caches failed to sync")
	}
	return
}

func (ca *ConnectionAgent) handlePreExistingIfcs() (err error) {
	err = ca.handlePreExistingLocalIfcs()
	if err != nil {
		return
	}

	err = ca.handlePreExistingRemoteIfcs()
	return
}

func (ca *ConnectionAgent) handlePreExistingLocalIfcs() error {
	allPreExistingLocalIfcs, err := ca.netFabric.ListLocalIfcs()
	if err != nil {
		return fmt.Errorf("Failed initial local network interfaces list: %s", err.Error())
	}

	for _, aPreExistingLocalIfc := range allPreExistingLocalIfcs {
		ifcMAC := aPreExistingLocalIfc.GuestMAC.String()
		ifcOwnerAtts, err := ca.localAttsInformer.GetIndexer().ByIndex(attMACIndexName, ifcMAC)
		if err != nil {
			return fmt.Errorf("Indexing local network interface with MAC %s failed: %s",
				ifcMAC,
				err.Error())
		}

		if len(ifcOwnerAtts) == 1 {
			// If we're here there's a local attachment which should own the interface
			// because their MAC addresses match. Hence we add the interface to
			// the attachment state.
			ifcOwnerAtt := ifcOwnerAtts[0].(*netv1a1.NetworkAttachment)
			attVNI, attNSN := ifcOwnerAtt.Spec.VNI, kosctlrutils.AttNSN(ifcOwnerAtt)
			attState := ca.getAttState(attNSN)
			if attState == nil {
				attState := &attachmentState{
					vnStateVNI: attVNI,
				}
				ca.setAttState(attNSN, attState)
			}
			ca.updateVNState(attState, attVNI, attNSN, ifcOwnerAtt.Spec.Node)
			oldIfc, oldIfcExists := attState.ifc, attState.ifcIsValid
			attState.vnStateVNI, attState.ifc, attState.ifcIsValid = attVNI, aPreExistingLocalIfc, true
			glog.V(3).Infof("Matched interface %#+v with local attachment %#+v", aPreExistingLocalIfc, ifcOwnerAtt)
			if oldIfcExists {
				aPreExistingLocalIfc = oldIfc
			} else {
				continue
			}
		}

		// If we're here the interface must be deleted, e.g. because it could not be matched to an attachment
		for i, err := 1, ca.netFabric.DeleteLocalIfc(aPreExistingLocalIfc); err != nil; i++ {
			glog.V(3).Infof("Deletion of orphan local interface %#+v failed: %s. Attempt nbr. %d",
				aPreExistingLocalIfc,
				err.Error(),
				i)
			time.Sleep(netFabricRetryPeriod)
		}
		glog.V(3).Infof("Deleted orphan local interface %#+v", aPreExistingLocalIfc)
	}

	return nil
}

func (ca *ConnectionAgent) handlePreExistingRemoteIfcs() error {
	// Start all remote attachments caches because we need to look up remote
	// attachments to decide which interfaces to keep and which to delete
	allLocalAtts, err := ca.localAttsLister.List(k8slabels.Everything())
	if err != nil {
		return fmt.Errorf("Failed initial local attachments list: %s", err.Error())
	}
	for _, aLocalAtt := range allLocalAtts {
		aLocalAttNSN, aLocalAttVNI := kosctlrutils.AttNSN(aLocalAtt), aLocalAtt.Spec.VNI
		aLocalAttState := ca.getAttState(aLocalAttNSN)
		if aLocalAttState == nil {
			aLocalAttState = &attachmentState{
				vnStateVNI: aLocalAttVNI,
			}
			ca.setAttState(aLocalAttNSN, aLocalAttState)
		}
		ca.updateVNState(aLocalAttState,
			aLocalAttVNI,
			aLocalAttNSN,
			ca.localNodeName)
		aLocalAttState.vnStateVNI = aLocalAttVNI
	}

	// Read all remote ifcs, for each interface find the attachment with the same
	// MAC in the cache for the remote attachments with the same VNI as the interface.
	// If either the attachment or the cache are not found, delete the interface,
	// bind it to the attachment otherwise.
	allPreExistingRemoteIfcs, err := ca.netFabric.ListRemoteIfcs()
	if err != nil {
		return fmt.Errorf("Failed initial remote network interfaces list: %s", err.Error())
	}
	for _, aPreExistingRemoteIfc := range allPreExistingRemoteIfcs {
		var ifcOwnerAtts []interface{}
		ifcMAC, ifcVNI := aPreExistingRemoteIfc.GuestMAC.String(), aPreExistingRemoteIfc.VNI
		remoteAttsInformer, remoteAttsInformerStopCh := ca.getRemoteAttsInformerForVNI(ifcVNI)
		if remoteAttsInformer != nil {
			if !remoteAttsInformer.HasSynced() &&
				!k8scache.WaitForCacheSync(remoteAttsInformerStopCh, remoteAttsInformer.HasSynced) {
				return fmt.Errorf("Failed to sync cache of remote attachments for VNI %d", ifcVNI)
			}
			ifcOwnerAtts, err = remoteAttsInformer.GetIndexer().ByIndex(attMACIndexName, ifcMAC)
		}

		if len(ifcOwnerAtts) == 1 {
			// If we're here a remote attachment owning the interface has been found
			ifcOwnerAtt := ifcOwnerAtts[0].(*netv1a1.NetworkAttachment)
			remAttNSN := kosctlrutils.AttNSN(ifcOwnerAtt)
			remAttState := ca.getAttState(remAttNSN)
			if remAttState == nil {
				remAttState = &attachmentState{
					vnStateVNI: ifcVNI,
				}
				ca.setAttState(remAttNSN, remAttState)
			}
			ca.updateVNState(remAttState, ifcVNI, remAttNSN, ifcOwnerAtt.Spec.Node)
			oldIfc, oldIfcExists := remAttState.ifc, remAttState.ifcIsValid
			remAttState.vnStateVNI, remAttState.ifc, remAttState.ifcIsValid = ifcVNI, aPreExistingRemoteIfc, true
			glog.V(3).Infof("Init: matched interface %#+v with remote attachment %#+v",
				aPreExistingRemoteIfc,
				ifcOwnerAtt)
			if oldIfcExists {
				aPreExistingRemoteIfc = oldIfc
			} else {
				continue
			}
		}

		// If we're here either there's no remote attachments cache associated with
		// the interface vni (because there are no local attachments with that vni),
		// or no remote attachment owning the interface was found, or the attachment
		// owning the interface already has one. For all such cases we need to delete
		// the interface.
		for i, err := 1, ca.netFabric.DeleteRemoteIfc(aPreExistingRemoteIfc); err != nil; i++ {
			glog.V(3).Infof("Init: deletion of orphan remote interface %#+v failed: %s. Attempt nbr. %d",
				aPreExistingRemoteIfc,
				err.Error(),
				i)
			time.Sleep(netFabricRetryPeriod)
		}
		glog.V(3).Infof("Init: deleted orphan local interface %#+v", aPreExistingRemoteIfc)
	}

	return nil
}

func (ca *ConnectionAgent) processQueue() {
	for {
		item, stop := ca.queue.Get()
		if stop {
			return
		}
		attNSN := item.(k8stypes.NamespacedName)
		ca.processQueueItem(attNSN)
	}
}

func (ca *ConnectionAgent) processQueueItem(attNSN k8stypes.NamespacedName) {
	defer ca.queue.Done(attNSN)
	err := ca.processNetworkAttachment(attNSN)
	requeues := ca.queue.NumRequeues(attNSN)
	if err != nil {
		// If we're here there's been an error: either the attachment current state was
		// ambiguous (e.g. more than one vni), or there's been a problem while processing
		// it (e.g. Interface creation failed). We requeue the attachment reference so that
		// it can be processed again and hopefully next time there will be no errors.
		glog.Warningf("Failed processing NetworkAttachment %s, requeuing (%d earlier requeues): %s",
			attNSN,
			requeues,
			err.Error())
		ca.queue.AddRateLimited(attNSN)
		return
	}
	glog.V(4).Infof("Finished NetworkAttachment %s with %d requeues", attNSN, requeues)
	ca.queue.Forget(attNSN)
}

func (ca *ConnectionAgent) processNetworkAttachment(attNSN k8stypes.NamespacedName) error {
	att, deleted, err := ca.getAttachment(attNSN)
	if att != nil {
		// If we are here the attachment exists and it's current state is univocal
		err = ca.processExistingAtt(att)
	} else if deleted {
		// If we are here the attachment has been deleted
		err = ca.processDeletedAtt(attNSN)
	}
	return err
}

// getAttachment attempts to determine the univocal version of the NetworkAttachment
// with namespaced name attNSN. If it succeeds it returns the attachment if it is
// found in an Informer cache or a boolean flag set to true if it could not be found
// in any cache (e.g. because it has been deleted). If the current attachment
// version cannot be determined without ambiguity, an error is returned. An
// attachment is considered amibguous if it either has been seen with more than
// one vni in a remote attachments cache, or if it is found both in the local
// attachments cache and a remote attachments cache.
func (ca *ConnectionAgent) getAttachment(attNSN k8stypes.NamespacedName) (*netv1a1.NetworkAttachment, bool, error) {
	// Retrieve the number of VN(I)s where the attachment could be as a remote
	// attachment, or, if it could be only in one VN(I), return that VNI.
	vni, nbrOfVNIs := ca.getAttVNIs(attNSN)
	if nbrOfVNIs > 1 {
		// If the attachment could be a remote one in more than one VNI, we
		// return immediately. When a deletion notification handler removes the
		// VNI with which it's seeing the attachment the attachment state will be
		// "less ambiguous" (one less potential VNI) and a reference will be enqueued
		// again triggering reconsideration of the attachment.
		glog.V(4).Infof("Attachment %s has inconsistent state, found in %d VN(I)s",
			attNSN,
			nbrOfVNIs)
		return nil, false, nil
	}

	// If the attachment has been seen in exactly one VNI lookup it up in
	// the remote attachments cache for the VNI with which it's been seen
	var (
		attAsRemote          *netv1a1.NetworkAttachment
		remAttCacheLookupErr error
	)
	if nbrOfVNIs == 1 {
		remoteAttsLister := ca.getRemoteAttListerForVNI(vni)
		if remoteAttsLister != nil {
			attAsRemote, remAttCacheLookupErr = remoteAttsLister.Get(attNSN.Name)
		}
	}

	// Lookup the attachment in the local attachments cache
	attAsLocal, localAttCacheLookupErr := ca.localAttsLister.NetworkAttachments(attNSN.Namespace).Get(attNSN.Name)

	switch {
	case (remAttCacheLookupErr != nil && !k8serrors.IsNotFound(remAttCacheLookupErr)) ||
		(localAttCacheLookupErr != nil && !k8serrors.IsNotFound(localAttCacheLookupErr)):
		// If we're here at least one of the two lookups failed. This should
		// never happen. No point in retrying.
		glog.V(1).Infof("attempt to retrieve attachment %s with lister failed: %s. This should never happen, hence a reference to %s will not be requeued",
			attNSN,
			aggregateErrors("\n\t", remAttCacheLookupErr, localAttCacheLookupErr).Error(),
			attNSN)
	case attAsLocal != nil && attAsRemote != nil:
		// If we're here the attachment has been found in both caches, hence it's
		// state is ambiguous. It will be deleted by one of the caches soon, and
		// this will cause a reference to be enqueued, so it will be processed
		// again when the ambiguity has been resolved (assuming it has not been
		// seen with other VNIs meanwhile).
		glog.V(4).Infof("att %s has inconsistent state: found both in local atts cache and remote atts cache for VNI %d",
			attNSN,
			vni)
	case attAsLocal != nil && attAsRemote == nil:
		// If we're here the attachment was found only in the local cache:
		// that's the univocal version of the attachment
		return attAsLocal, false, nil
	case attAsLocal == nil && attAsRemote != nil:
		// If we're here the attachment was found only in the remote attachments
		// cache for its vni: that's the univocal version of the attachment
		return attAsRemote, false, nil
	default:
		// If we're here neither lookup could find the attachment: we assume the
		// attachment has been deleted by both caches and is therefore no longer
		// relevant to the connection agent
		return nil, true, nil
	}
	return nil, false, nil
}

func (ca *ConnectionAgent) processExistingAtt(att *netv1a1.NetworkAttachment) error {
	attNSN, attVNI, attNode := kosctlrutils.AttNSN(att), att.Spec.VNI, att.Spec.Node

	// Retrieve the last seen attachment state, which stores the attachment network
	// interface if it was created, and the vni with which it was processed last time
	// (this field is used to update the old vnState in case the current version of the
	// attachment has a different vni).
	attState := ca.getAttState(attNSN)
	if attState == nil {
		attState = &attachmentState{
			vnStateVNI: attVNI,
		}
		ca.setAttState(attNSN, attState)
	}

	// Update the vnState associated with the attachment. This typically involves
	// adding the attachment to the vnState associated to its vni (and initializing
	// that vnState if the attachment is the first local one with its vni), but
	// could also entail removing the attachment from the vnState associated with
	// its old vni if the vni has changed.
	var err error
	vnState := ca.updateVNState(attState, attVNI, attNSN, attNode)
	if vnState != nil {
		// If we're here att is currently remote but was previously the last local
		// attachment in its vni. Thus, we act as if the last local attachment
		// in the vn was deleted
		ca.clearVNResources(vnState, attVNI)
		return nil
	}

	// Update attState with the most recent vni.
	attState.vnStateVNI = attVNI

	// Create or update the interface associated with the attachment.
	attGuestIP := gonet.ParseIP(att.Status.IPv4)
	var attHostIP gonet.IP
	if attNode == ca.localNodeName {
		attHostIP = ca.hostIP
	} else {
		attHostIP = gonet.ParseIP(att.Status.HostIP)
	}
	attIfc, attHasIfc, err := ca.createOrUpdateIfc(attState,
		attGuestIP,
		attHostIP,
		attVNI,
		attNSN)

	// Update attState with the most recent interface (if it's been created)
	attState.ifc, attState.ifcIsValid = attIfc, attHasIfc
	if err != nil {
		return err
	}

	// If the attachment is local, update its status with the local host IP and
	// the name of the interface which was created (if it has changed).
	localHostIPStr := ca.hostIP.String()
	ifcName := attIfc.Name
	if attNode == ca.localNodeName &&
		(att.Status.HostIP != localHostIPStr || (attIfc.Name != att.Status.IfcName)) {

		updatedAtt, err := ca.setAttStatus(att, ifcName)
		if err != nil {
			return err
		}
		if updatedAtt != nil {
			glog.V(3).Infof("Updated att %s status with hostIP: %s, ifcName: %s",
				attNSN,
				updatedAtt.Status.HostIP,
				updatedAtt.Status.IfcName)
		}
	}

	return nil
}

func (ca *ConnectionAgent) processDeletedAtt(attNSN k8stypes.NamespacedName) error {
	attState := ca.getAttState(attNSN)
	if attState == nil {
		return nil
	}

	vni := attState.vnStateVNI
	lastLocalAttInVN := ca.updateVNStateAfterAttDeparture(attNSN.Name, vni)
	if lastLocalAttInVN {
		glog.V(2).Infof("NetworkAttachment %s was the last local with vni %d: remote attachments informer was stopped", attNSN, vni)
	}

	err := ca.deleteIfc(attState.ifc, attState.ifcIsValid)
	if err != nil {
		return err
	}

	ca.removeAttState(attNSN)
	return nil
}

func (ca *ConnectionAgent) getAttState(attNSN k8stypes.NamespacedName) *attachmentState {
	ca.nsnToAttStateMutex.RLock()
	defer ca.nsnToAttStateMutex.RUnlock()
	return ca.nsnToAttState[attNSN]
}

func (ca *ConnectionAgent) setAttState(attNSN k8stypes.NamespacedName, attState *attachmentState) {
	ca.nsnToAttStateMutex.Lock()
	defer ca.nsnToAttStateMutex.Unlock()
	ca.nsnToAttState[attNSN] = attState
}

func (ca *ConnectionAgent) removeAttState(attNSN k8stypes.NamespacedName) {
	ca.nsnToAttStateMutex.Lock()
	defer ca.nsnToAttStateMutex.Unlock()
	delete(ca.nsnToAttState, attNSN)
}

func (ca *ConnectionAgent) updateVNState(attState *attachmentState,
	attVNI uint32,
	attNSN k8stypes.NamespacedName,
	attNode string) *vnState {

	if attState != nil {
		oldAttVNI := attState.vnStateVNI
		if oldAttVNI != attVNI {
			// if we're here the attachment vni changed since the last time it
			// was processed, hence we update the vnState associated with the
			// old value of the vni to reflect the attachment departure.
			ca.updateVNStateAfterAttDeparture(attNSN.Name, oldAttVNI)
		}
	}
	return ca.updateVNStateForExistingAtt(attNSN, attNode == ca.localNodeName, attVNI)
}

// updateVNStateForExistingAtt adds the attachment to the vnState associated with
// its vni. If the attachment is local and is the first one for its vni, the
// associated vnState is initialized (this entails starting the remote attachments
// informer). If the attachment was the last local attachment in its vnState and
// has become remote, the vnState for its vni is cleared (it's removed from the
// map storing the vnStates) and returned, so that the caller can perform a clean
// up of the resources associated with the vnState (remote attachments informer
// is stopped and references to the remote attachments are enqueued).
func (ca *ConnectionAgent) updateVNStateForExistingAtt(attNSN k8stypes.NamespacedName,
	attIsLocal bool,
	vni uint32) (vnStateRet *vnState) {

	attName := attNSN.Name
	firstLocalAttInVN := false

	ca.vniToVnStateMutex.Lock()
	defer func() {
		ca.vniToVnStateMutex.Unlock()
		if firstLocalAttInVN {
			glog.V(2).Infof("VN with ID %d became relevant: an Informer has been started", vni)
		}
	}()

	vnState := ca.vniToVnState[vni]
	if attIsLocal {
		// If we're here the attachment is local. If the vnState for the
		// attachment vni is missing it means that the attachment is the first
		// local one for its vni, hence we initialize the vnState (this entails
		// starting the remote attachments informer). We also add the attachment
		// name to the local attachments in the virtual network and remove the
		// attachment name from the remote attachments: this is needed in case
		// we're here because of an update which did not change the vni but made
		// the attachment transition from remote to local.
		if vnState == nil {
			vnState = ca.initVNState(vni, attNSN.Namespace)
			ca.vniToVnState[vni] = vnState
			firstLocalAttInVN = true
		}
		delete(vnState.remoteAtts, attName)
		vnState.localAtts[attName] = struct{}{}
	} else {
		// If we're here the attachment is remote. If the vnState for the
		// attachment vni is not missing (because the last local attachment with
		// the same vni has been deleted), we remove the attachment name from
		// the local attachments: this is needed in case we're here because of
		// an update which did not change the vni but made the attachment
		// transition from local to remote. After doing this, we check whether
		// the local attachment we've removed was the last one for its vni. If
		// that's the case (len(state.localAtts) == 0), the vni is no longer
		// relevant to the connection agent, hence we return the vnState after
		// removing it from the map storing all the vnStates so that the caller
		// can perform the necessary clean up. If that's not the case, we add the
		// attachment name to the remote attachments in the vnState.
		if vnState != nil {
			vnState.remoteAtts[attName] = struct{}{}
			delete(vnState.localAtts, attName)
			if len(vnState.localAtts) == 0 {
				delete(ca.vniToVnState, vni)
				vnStateRet = vnState
				return
			}
		}
	}

	return
}

func (ca *ConnectionAgent) updateVNStateAfterAttDeparture(attName string, vni uint32) bool {
	vnState := ca.removeAttFromVNState(attName, vni)
	if vnState == nil {
		return false
	}
	// If we're here attName was the last local attachment in the virtual network
	// with id vni. Hence we stop the remote attachments informer and enqueue
	// references to remote attachments in that virtual network, so that their
	// interfaces can be deleted.
	ca.clearVNResources(vnState, vni)
	return true
}

func (ca *ConnectionAgent) createOrUpdateIfc(attState *attachmentState,
	attGuestIP, attHostIP gonet.IP,
	attVNI uint32,
	attNSN k8stypes.NamespacedName) (existingIfc netfabric.NetworkInterface, attHasIfc bool, err error) {

	if attState != nil {
		existingIfc, attHasIfc = attState.ifc, attState.ifcIsValid
	}

	newIfcNeedsToBeCreated := !attHasIfc ||
		ifcNeedsUpdate(existingIfc, attGuestIP, attHostIP, attVNI)

	err = ca.deleteIfc(existingIfc, attHasIfc && newIfcNeedsToBeCreated)
	if err != nil {
		err = fmt.Errorf("Update of network interface of attachment %s failed, old interface %#+v could not be deleted: %s",
			attNSN,
			existingIfc,
			err.Error())
		return
	}

	if newIfcNeedsToBeCreated {
		attHasIfc = false
		guestMAC := generateMACAddr(attVNI, attGuestIP)
		newIfc := netfabric.NetworkInterface{
			Name:     generateIfcName(guestMAC),
			VNI:      attVNI,
			GuestMAC: guestMAC,
			GuestIP:  attGuestIP,
			HostIP:   attHostIP,
		}
		if attHostIP.Equal(ca.hostIP) {
			err = ca.netFabric.CreateLocalIfc(newIfc)
		} else {
			err = ca.netFabric.CreateRemoteIfc(newIfc)
		}
		if err != nil {
			err = fmt.Errorf("Creation of network interface of attachment %s failed, interface %#+v could not be created: %s",
				attNSN,
				newIfc,
				err.Error())
			return
		}
		existingIfc = newIfc
		attHasIfc = true
	}

	return
}

func (ca *ConnectionAgent) deleteIfc(ifc netfabric.NetworkInterface, ifcNeedsDeletion bool) error {
	if ifcNeedsDeletion {
		if ifc.HostIP.Equal(ca.hostIP) {
			// If we're here the interface is local
			return ca.netFabric.DeleteLocalIfc(ifc)
		}
		// If we're here the interface is remote
		return ca.netFabric.DeleteRemoteIfc(ifc)
	}
	return nil
}

func (ca *ConnectionAgent) setAttStatus(att *netv1a1.NetworkAttachment,
	ifcName string) (*netv1a1.NetworkAttachment, error) {

	att2 := att.DeepCopy()
	att2.Status.HostIP = ca.hostIP.String()
	att2.Status.IfcName = ifcName
	updatedAtt, err := ca.netv1a1Ifc.NetworkAttachments(att2.Namespace).Update(att2)
	return updatedAtt, err
}

// removeAttFromVNState removes attName from the vnState associated with vni, both
// for local and remote attachments. If attName is the last local attachment in
// the vnState, vnState is returned, so that the caller can perform additional
// clean up (e.g. stopping the remote attachments informer).
func (ca *ConnectionAgent) removeAttFromVNState(attName string, vni uint32) *vnState {
	ca.vniToVnStateMutex.Lock()
	defer ca.vniToVnStateMutex.Unlock()
	vnState := ca.vniToVnState[vni]
	if vnState != nil {
		delete(vnState.localAtts, attName)
		if len(vnState.localAtts) == 0 {
			delete(ca.vniToVnState, vni)
			return vnState
		}
		delete(vnState.remoteAtts, attName)
	}
	return nil
}

// clearVNResources stops the informer on remote attachments on the virtual
// network and enqueues references to such attachments so that their interfaces
// can be deleted.
func (ca *ConnectionAgent) clearVNResources(vnState *vnState, vni uint32) {
	close(vnState.remoteAttsInformerStopCh)
	for aRemoteAttName := range vnState.remoteAtts {
		aRemoteAttNSN := k8stypes.NamespacedName{
			Namespace: vnState.namespace,
			Name:      aRemoteAttName,
		}
		ca.removeVNI(aRemoteAttNSN, vni)
		ca.queue.Add(aRemoteAttNSN)
	}
}

func (ca *ConnectionAgent) initVNState(vni uint32, namespace string) *vnState {
	remoteAttsInformer, remoteAttsLister := v1a1AttsCustomNamespaceInformerAndLister(ca.kcs,
		resyncPeriod,
		namespace,
		fromFieldsSelectorToTweakListOptionsFunc(ca.remoteAttInVNWithVirtualIPHostIPSelector(vni)))

	remoteAttsInformer.AddIndexers(map[string]k8scache.IndexFunc{attMACIndexName: attachmentMACAddr})

	remoteAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    ca.onRemoteAttAdded,
		UpdateFunc: ca.onRemoteAttUpdated,
		DeleteFunc: ca.onRemoteAttRemoved,
	})

	remoteAttsInformerStopCh := make(chan struct{})
	go remoteAttsInformer.Run(aggregateTwoStopChannels(ca.stopCh, remoteAttsInformerStopCh))

	return &vnState{
		remoteAttsInformer:       remoteAttsInformer,
		remoteAttsInformerStopCh: remoteAttsInformerStopCh,
		remoteAttsLister:         remoteAttsLister,
		namespace:                namespace,
		localAtts:                make(map[string]struct{}),
		remoteAtts:               make(map[string]struct{}),
	}
}

func (ca *ConnectionAgent) onRemoteAttAdded(obj interface{}) {
	att := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Remote NetworkAttachments cache for VNI %d: notified of addition of %#+v",
		att.Spec.VNI,
		att)
	attNSN := kosctlrutils.AttNSN(att)
	ca.addVNI(attNSN, att.Spec.VNI)
	ca.queue.Add(attNSN)
}

func (ca *ConnectionAgent) onRemoteAttUpdated(oldObj, newObj interface{}) {
	oldAtt := oldObj.(*netv1a1.NetworkAttachment)
	newAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Remote NetworkAttachments cache for VNI %d: notified of update from %#+v to %#+v",
		newAtt.Spec.VNI,
		oldAtt,
		newAtt)
	ca.queue.Add(kosctlrutils.AttNSN(newAtt))
}

func (ca *ConnectionAgent) onRemoteAttRemoved(obj interface{}) {
	peeledObj := kosctlrutils.Peel(obj)
	att := peeledObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Remote NetworkAttachments cache for VNI %d: notified of deletion of %#+v",
		att.Spec.VNI,
		att)
	attNSN := kosctlrutils.AttNSN(att)
	ca.removeVNI(attNSN, att.Spec.VNI)
	ca.queue.Add(attNSN)
}

func (ca *ConnectionAgent) addVNI(nsn k8stypes.NamespacedName, vni uint32) {
	ca.nsnToVNIsIntelMutex.Lock()
	defer ca.nsnToVNIsIntelMutex.Unlock()
	attVNIsIntel := ca.nsnToVNIsIntel[nsn]
	if attVNIsIntel == nil {
		attVNIsIntel = newAttVNIsIntel()
		ca.nsnToVNIsIntel[nsn] = attVNIsIntel
	}
	attVNIsIntel.addVNI(vni)
}

func (ca *ConnectionAgent) removeVNI(nsn k8stypes.NamespacedName, vni uint32) {
	ca.nsnToVNIsIntelMutex.Lock()
	defer ca.nsnToVNIsIntelMutex.Unlock()
	attVNIsIntel := ca.nsnToVNIsIntel[nsn]
	if attVNIsIntel == nil {
		return
	}
	attVNIsIntel.removeVNI(vni)
	if _, nbrOfVNIs := attVNIsIntel.getIntel(); nbrOfVNIs == 0 {
		delete(ca.nsnToVNIsIntel, nsn)
	}
}

func (ca *ConnectionAgent) getAttVNIs(nsn k8stypes.NamespacedName) (onlyVNI uint32, nbrOfVNIs int) {
	ca.nsnToVNIsIntelMutex.RLock()
	defer ca.nsnToVNIsIntelMutex.RUnlock()
	attVNIsIntel := ca.nsnToVNIsIntel[nsn]
	if attVNIsIntel == nil {
		return
	}
	onlyVNI, nbrOfVNIs = attVNIsIntel.getIntel()
	return
}

func (ca *ConnectionAgent) getRemoteAttListerForVNI(vni uint32) koslisterv1a1.NetworkAttachmentNamespaceLister {
	ca.vniToVnStateMutex.RLock()
	defer ca.vniToVnStateMutex.RUnlock()
	vnState := ca.vniToVnState[vni]
	if vnState == nil {
		return nil
	}
	return vnState.remoteAttsLister
}

// getRemoteAttsIndexerForVNI accesses the map with all the vnStates but it's not
// thread-safe because it is meant to be used only at start-up, when there's only
// one thread running.
func (ca *ConnectionAgent) getRemoteAttsInformerForVNI(vni uint32) (k8scache.SharedIndexInformer, chan struct{}) {
	vnState := ca.vniToVnState[vni]
	if vnState == nil {
		return nil, nil
	}
	return vnState.remoteAttsInformer, vnState.remoteAttsInformerStopCh
}

// Return a string representing a field selector that matches NetworkAttachments
// that run on the local node and have a virtual IP.
func (ca *ConnectionAgent) localAttWithAnIPSelector() string {
	// localAttSelector expresses the constraint that the NetworkAttachment runs
	// on this node.
	localAttSelector := attNodeFieldName + equal + ca.localNodeName

	// Express the constraint that the NetworkAttachment has a virtual IP by
	// saying that the field containig the virtual IP must not be equal to the
	// empty string.
	attWithAnIPSelector := attIPFieldName + notEqual

	// Build a selector which is a logical AND between
	// attWithAnIPSelectorString and localAttSelectorString.
	allSelectors := []string{localAttSelector, attWithAnIPSelector}
	return strings.Join(allSelectors, ",")
}

// Return a string representing a field selector that matches NetworkAttachments
// that run on a remote node on the Virtual Network identified by the given VNI
// and have a virtual IP and the host IP field set.
func (ca *ConnectionAgent) remoteAttInVNWithVirtualIPHostIPSelector(vni uint32) string {
	// remoteAttSelector expresses the constraint that the NetworkAttachment
	// runs on a remote node.
	remoteAttSelector := attNodeFieldName + notEqual + ca.localNodeName

	// hostIPIsNotLocalSelector expresses the constraint that the NetworkAttachment
	// status.hostIP is not equal to that of the current node. Without this selector,
	// an update to the spec.Node field of a NetworkAttachment could lead to a
	// creation notification for the attachment in a remote attachments cache,
	// even if the attachment still has the host IP of the current node
	// (status.hostIP is set with an update by the connection agent on the
	// node of the attachment). This could result in the creation of a remote
	// interface with the host IP of the local node.
	hostIPIsNotLocalSelector := attHostIPFieldName + notEqual + ca.hostIP.String()

	// attWithAnIPSelector and attWithHostIPSelector express the constraints that
	// the NetworkAttachment has the fields storing virtual IP and host IP set,
	// by saying that such fields must not be equal to the empty string.
	attWithAnIPSelector := attIPFieldName + notEqual
	attWithHostIPSelector := attHostIPFieldName + notEqual

	// attInSpecificVNSelector expresses the constraint that the NetworkAttachment
	// is in the Virtual Network identified by vni.
	attInSpecificVNSelector := attVNIFieldName + equal + fmt.Sprint(vni)

	// Build and return a selector which is a logical AND between all the selectors
	// defined above.
	allSelectors := []string{remoteAttSelector,
		hostIPIsNotLocalSelector,
		attWithAnIPSelector,
		attWithHostIPSelector,
		attInSpecificVNSelector}
	return strings.Join(allSelectors, ",")
}

func fromFieldsSelectorToTweakListOptionsFunc(customFieldSelector string) kosinternalifcs.TweakListOptionsFunc {
	return func(options *k8smetav1.ListOptions) {
		optionsFieldSelector := options.FieldSelector
		allSelectors := make([]string, 0, 2)
		if strings.Trim(optionsFieldSelector, " ") != "" {
			allSelectors = append(allSelectors, optionsFieldSelector)
		}
		allSelectors = append(allSelectors, customFieldSelector)
		options.FieldSelector = strings.Join(allSelectors, ",")
	}
}

func v1a1AttsCustomInformerAndLister(kcs *kosclientset.Clientset,
	resyncPeriod time.Duration,
	tweakListOptionsFunc kosinternalifcs.TweakListOptionsFunc) (k8scache.SharedIndexInformer, koslisterv1a1.NetworkAttachmentLister) {

	attv1a1Informer := createAttsv1a1Informer(kcs,
		resyncPeriod,
		k8smetav1.NamespaceAll,
		tweakListOptionsFunc)
	return attv1a1Informer.Informer(), attv1a1Informer.Lister()
}

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

// attachmentMACAddr is an Index function that computes the MAC address of a
// NetworkAttachment. Used to map pre-existing interfaces with attachments at
// start up.
func attachmentMACAddr(obj interface{}) ([]string, error) {
	att := obj.(*netv1a1.NetworkAttachment)
	return []string{generateMACAddr(att.Spec.VNI, gonet.ParseIP(att.Status.IPv4)).String()}, nil
}

func generateIfcName(macAddr gonet.HardwareAddr) string {
	return "kos" + strings.Replace(macAddr.String(), ":", "", -1)
}

func generateMACAddr(vni uint32, guestIPv4 gonet.IP) gonet.HardwareAddr {
	guestIPBytes := guestIPv4.To4()
	mac := make([]byte, 6, 6)
	mac[5] = byte(vni)
	mac[4] = byte(vni >> 8)
	mac[3] = guestIPBytes[3]
	mac[2] = guestIPBytes[2]
	mac[1] = guestIPBytes[1]
	mac[0] = (byte(vni>>13) & 0xF8) | ((guestIPBytes[0] & 0x02) << 1) | 2
	return mac
}

// aggregateStopChannels returns a channel which
// is closed when either ch1 or ch2 is closed
func aggregateTwoStopChannels(ch1, ch2 <-chan struct{}) chan struct{} {
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

func aggregateErrors(sep string, errs ...error) error {
	aggregateErrsSlice := make([]string, 0, len(errs))
	for i, err := range errs {
		if err != nil && strings.Trim(err.Error(), " ") != "" {
			aggregateErrsSlice = append(aggregateErrsSlice, fmt.Sprintf("error nbr. %d ", i)+err.Error())
		}
	}
	if len(aggregateErrsSlice) > 0 {
		return fmt.Errorf("%s", strings.Join(aggregateErrsSlice, sep))
	}
	return nil
}

// TODO consider switching to pointers wrt value for the interface
func ifcNeedsUpdate(ifc netfabric.NetworkInterface, newGuestIP, newHostIP gonet.IP, newVNI uint32) bool {
	return !ifc.GuestIP.Equal(newGuestIP) ||
		!ifc.HostIP.Equal(newHostIP) ||
		ifc.VNI != newVNI
}
