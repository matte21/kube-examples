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
	"bytes"
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
	// Name of the indexer which computes the MAC address
	// for a network attachment. Used for handling of
	// pre-existing interfaces at start-up.
	attMACIndexName = "attachmentMAC"

	// NetworkAttachments in network.example.com/v1alpha1
	// fields names. Used to build field selectors.
	attNodeFieldName   = "spec.node"
	attIPFieldName     = "status.ipv4"
	attHostIPFieldName = "status.hostIP"
	attIfcFieldName    = "status.ifcName"
	attVNIFieldName    = "spec.vni"

	// fields selector comparison operators.
	// Used to build fields selectors.
	equal    = "="
	notEqual = "!="

	// Name of status subresource used for patching NetworkAttachments status
	// when processing deleted local Attachments.
	statusSubRes = "status"

	// resync period for Informers caches. Set
	// to 0 because we don't want resyncs.
	resyncPeriod = 0
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

	// localAtts stores the names the local NetworkAttachments in
	// the Virtual Network the vnState represents.
	localAtts  map[string]struct{}
	remoteAtts map[string]struct{}
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

// ConnectionAgent represents a K8S controller which runs on every node of the cluster and
// eagerly maintains up-to-date the mapping between virtual IPs and physical IPs for
// every relevant NetworkAttachment. A NetworkAttachment is relevant to a connection agent
// if: (1) it runs on the same node as the connection agent, or (2) it's part of a
// Virtual Network where at least one NetworkAttachment for which (1) is true exists.
// To achieve its goal, a connection agent receives notifications about relevant
// NetworkAttachments from the K8s API server through Informers, and when necessary
// creates/updates/deletes Network Interfaces through a low-level network interface fabric.
// When a new Virtual Network becomes relevant for the connection agent because of the creation
// of the first attachment of that Virtual Network on the same node as the connection agent,
// a new informer on remote NetworkAttachments in that Virtual Network is created.
type ConnectionAgent struct {
	localNodeName string
	hostIP        gonet.IP
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

	localAttIfcsMutex sync.RWMutex
	localAttIfcs      map[k8stypes.NamespacedName]netfabric.NetworkInterface

	remoteAttIfcsMutex sync.RWMutex
	remoteAttIfcs      map[k8stypes.NamespacedName]netfabric.NetworkInterface
}

func NewConnectionAgent(localNodeName string,
	hostIP gonet.IP,
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
		localAttIfcs:  make(map[k8stypes.NamespacedName]netfabric.NetworkInterface),
		remoteAttIfcs: make(map[k8stypes.NamespacedName]netfabric.NetworkInterface),
	}
}

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

	err = ca.handlePreExistingLocalIfcs()
	if err != nil {
		// ? An error is returned if deleting a single interface fails. This might be a bad idea: we might just want to keep running
		// (after logging the failure) if out of 1k interfaces one could not be deleted. In fact, if the network fabric (OvS) deletion
		// can occasionally experience transient failures and we have a lot of interfaces on the node, the fact that we are just returning
		// an error might make it impossible to make the connection agent successfully start up. If that's the case (OvS can experience
		// flaky failures), we should retry to delete the interface after some time, not just simply log the failure, because that would
		// mean leaving on the node an interface the connection agent is not aware of, and this could cause troubles if later an attachment
		// which needs an interface with the same MAC as the one which could not be deleted is created. If insterad OvS is reliable and fails
		// only when there's something wrong with the system state, then the current solution is fine. Choose what to do after the OvS network
		// fabric is ready.
		return err
	}
	glog.V(2).Infoln("Pre-existing local interfaces synced")

	err = ca.handlePreExistingRemoteIfcs()
	if err != nil {
		// same considerations as for local ifcs
		return err
	}
	glog.V(2).Infoln("Pre-existing remote interfaces synced")

	for i := 0; i < ca.workers; i++ {
		go k8swait.Until(ca.processQueue, time.Second, stopCh)
	}
	glog.V(2).Infof("Launched %d workers", ca.workers)

	<-stopCh
	return nil
}

func (ca *ConnectionAgent) initLocalAttsInformerAndLister() {
	localAttWithAnIPSelector := ca.localAttWithAnIPSelector()
	glog.V(6).Info("Created Local NetworkAttachments fields selector: " + localAttWithAnIPSelector)

	ca.localAttsInformer, ca.localAttsLister = v1a1AttsCustomInformerAndLister(ca.kcs,
		resyncPeriod,
		fromFieldsSelectorToTweakListOptionsFunc(localAttWithAnIPSelector))

	ca.localAttsInformer.AddIndexers(map[string]k8scache.IndexFunc{attMACIndexName: attachmentMACAddr})

	ca.localAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    ca.onLocalAttAdded,
		UpdateFunc: ca.onLocalAttUpdated,
		DeleteFunc: ca.onLocalAttRemoved})
}

func (ca *ConnectionAgent) onLocalAttAdded(obj interface{}) {
	localAtt := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v addition to local NetworkAttachments cache", localAtt)
	localAttRef := ca.fromAttToEnquableRef(localAtt)
	ca.queue.Add(localAttRef)
}

func (ca *ConnectionAgent) onLocalAttUpdated(oldObj, newObj interface{}) {
	oldLocalAtt := oldObj.(*netv1a1.NetworkAttachment)
	newLocalAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment update from %#+v to %#+v in local NetworkAttachments cache", oldLocalAtt, newLocalAtt)
	newLocalAttRef := ca.fromAttToEnquableRef(newLocalAtt)
	ca.queue.Add(newLocalAttRef)
	if newLocalAtt.Spec.VNI != oldLocalAtt.Spec.VNI {
		oldLocalAttRef := ca.fromAttToEnquableRef(oldLocalAtt)
		ca.queue.Add(oldLocalAttRef)
	}
}

func (ca *ConnectionAgent) onLocalAttRemoved(obj interface{}) {
	peeledObj := peel(obj)
	localAtt := peeledObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v removal from local NetworkAttachments cache", localAtt)
	localAttRef := ca.fromAttToEnquableRef(localAtt)
	ca.queue.Add(localAttRef)
}

func (ca *ConnectionAgent) waitForLocalAttsCacheSync(stopCh <-chan struct{}) (err error) {
	if !k8scache.WaitForCacheSync(stopCh, ca.localAttsInformer.HasSynced) {
		err = fmt.Errorf("Caches failed to sync")
	}
	return
}

func (ca *ConnectionAgent) handlePreExistingLocalIfcs() error {
	allPreExistingLocalIfcs, err := ca.netFabric.ListLocalIfcs()
	if err != nil {
		return fmt.Errorf("Failed initial local network interfaces list: %s", err.Error())
	}
	for _, aPreExistingLocalIfc := range allPreExistingLocalIfcs {
		ifcMac := aPreExistingLocalIfc.GuestMAC.String()
		ifcOwnerAtts, err := ca.localAttsInformer.GetIndexer().ByIndex(attMACIndexName, ifcMac)
		if err != nil {
			return fmt.Errorf("Indexing local network interface with MAC %s failed: %s", ifcMac, err.Error())
		}
		if len(ifcOwnerAtts) == 1 {
			// If we're here there's a local attachment which should own the interface because their MAC
			// addresses match. Hence we add the interface under the attachment namespaced name to the
			// map associating interfaces and attachments.
			ifcOwnerAtt := ifcOwnerAtts[0].(*netv1a1.NetworkAttachment)
			firstLocalAttInVN := ca.updateVNStateForExistingLocalAtt(ifcOwnerAtt)
			if firstLocalAttInVN {
				glog.V(2).Infof("VN with ID %d became relevant: an Informer has been started", ifcOwnerAtt.Spec.VNI)
			}
			ca.localAttIfcs[attNSN(ifcOwnerAtt)] = aPreExistingLocalIfc
			glog.V(3).Infof("Init: matched interface %v with local attachment %v", aPreExistingLocalIfc, ifcOwnerAtt)
		} else {
			err = ca.netFabric.DeleteLocalIfc(aPreExistingLocalIfc)
			if err != nil {
				return err
			}
			glog.V(3).Infof("Init: deleted orphan local interface %v", aPreExistingLocalIfc)
		}
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
		firstLocalAttInVN := ca.updateVNStateForExistingLocalAtt(aLocalAtt)
		if firstLocalAttInVN {
			glog.V(2).Infof("VN with ID %d became relevant: an Informer has been started", aLocalAtt.Spec.VNI)
		}
	}

	// Read all remote ifcs, for each interface find the attachment with the same MAC in the
	// cache for the remote attachments with the same VNI as the interface. If either the
	// attachment or the cache are not found, delete the interface, bind it to the attachment
	// otherwise.
	allPreExistingRemoteIfcs, err := ca.netFabric.ListRemoteIfcs()
	if err != nil {
		return fmt.Errorf("Failed initial remote network interfaces list: %s", err.Error())
	}
	for _, aPreExistingRemoteIfc := range allPreExistingRemoteIfcs {
		var ifcOwnerAtts []interface{}
		ifcMac := aPreExistingRemoteIfc.GuestMAC.String()
		remoteAttsInformer, remoteAttsInformerStopCh := ca.getRemoteAttsInformerForVNI(aPreExistingRemoteIfc.VNI)
		if remoteAttsInformer != nil {
			if !remoteAttsInformer.HasSynced() &&
				!k8scache.WaitForCacheSync(remoteAttsInformerStopCh, remoteAttsInformer.HasSynced) {
				return fmt.Errorf("Failed to sync cache of remote attachments for VNI %d", aPreExistingRemoteIfc.VNI)
			}
			ifcOwnerAtts, err = remoteAttsInformer.GetIndexer().ByIndex(attMACIndexName, ifcMac)
		}
		if len(ifcOwnerAtts) == 1 {
			// If we're here a remote attachment owning the interface has been found
			ifcOwnerAtt := ifcOwnerAtts[0].(*netv1a1.NetworkAttachment)
			remAttNSN := attNSN(ifcOwnerAtt)
			ca.addRemoteAttToVNState(remAttNSN.Name, ifcOwnerAtt.Spec.VNI)
			ca.remoteAttIfcs[remAttNSN] = aPreExistingRemoteIfc
			glog.V(3).Infof("Init: matched interface %v with remote attachment %v", aPreExistingRemoteIfc, ifcOwnerAtt)
		} else {
			// If we're here either there's no remote attachments cache associated with the interface vni (because there are
			// no local attachments with that vni), or no remote attachment owning the interface was found. Either ways, we need
			// to delete the interface.
			err = ca.netFabric.DeleteRemoteIfc(aPreExistingRemoteIfc)
			if err != nil {
				return err
			}
			glog.V(3).Infof("Init: deleted orphan remote interface %v", aPreExistingRemoteIfc)
		}
	}

	return nil
}

// getRemoteAttsIndexerForVNI accesses the map with all the vnStates but it's not thread-safe
// because it is meant to be used only at start-up, when there's only one thread running.
func (ca *ConnectionAgent) getRemoteAttsInformerForVNI(vni uint32) (k8scache.SharedIndexInformer, chan struct{}) {
	vnState := ca.vniToVnState[vni]
	if vnState == nil {
		return nil, nil
	}
	return vnState.remoteAttsInformer, vnState.remoteAttsInformerStopCh
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
	var (
		err         error
		localityStr string
	)
	if attRef.local {
		err = ca.processLocalAtt(attRef.nsn, attRef.vni)
		localityStr = "local"
	} else {
		err = ca.processRemoteAtt(attRef.nsn, attRef.vni)
		localityStr = "remote"
	}
	requeues := ca.queue.NumRequeues(attRef)
	if err != nil {
		glog.Warningf("Failed processing %s NetworkAttachment %s in VNI %d, requeuing (%d earlier requeues): %s",
			localityStr,
			attRef.nsn,
			attRef.vni,
			requeues,
			err.Error())
		ca.queue.AddRateLimited(attRef)
		return
	}
	glog.V(4).Infof("Finished %s NetworkAttachment %s in VNI %d with %d requeues", localityStr, attRef.nsn, attRef.vni, requeues)
	ca.queue.Forget(attRef)
}

func (ca *ConnectionAgent) processLocalAtt(nsn k8stypes.NamespacedName, vni uint32) error {
	att, err := ca.localAttsLister.NetworkAttachments(nsn.Namespace).Get(nsn.Name)

	if att != nil && att.Spec.VNI == vni {
		// If we're here the attachment was found and the VNIs match, so we proceed
		// to attempting to create a local interface.
		firstLocalAttInVN := ca.updateVNStateForExistingLocalAtt(att)
		if firstLocalAttInVN {
			glog.V(2).Infof("VN with ID %d became relevant: an Informer has been started", att.Spec.VNI)
		}
		ifcNameForStatus, statusIfcNameMaybeNeedsUpdate, err := ca.attemptToCreateLocalAttIfc(att)
		updatedAtt, statusUpdateErr := ca.setIfcNameAndHostIPInLocalAttStatusIfNeeded(att, ifcNameForStatus, statusIfcNameMaybeNeedsUpdate)
		if updatedAtt != nil {
			glog.V(2).Infof("Updated local att %s status with ifc name %s and host IP %s", nsn,
				updatedAtt.Status.IfcName,
				updatedAtt.Status.HostIP,
			)
		}
		return aggregateErrors("\n\t", err, statusUpdateErr)
	}

	if (err != nil && k8serrors.IsNotFound(err)) || (att != nil && att.Spec.VNI != vni) {
		// If we're here either the attachment has been deleted or its VNI is different wrt to the one
		// in the reference. In both cases: (1) The vnState for the reference vni should be updated to
		// reflect the fact that the local attachment is no longer there and (2) If an old interface (with
		// the same VNI as the one in the reference) exists it should be deleted.
		lastLocalAttInVN := ca.updateVNStateAfterLocalAttDeletion(nsn.Name, vni)
		if lastLocalAttInVN {
			glog.V(2).Infof("VN with ID %d became irrelevant: %s",
				vni,
				"its Informer has been stopped and references to remote atts in that VN have been enqueued")
		}
		deletedIfcName, ifcDeletionErr := ca.attemptToDeleteLocalAttIfc(nsn, vni)
		if err != nil && k8serrors.IsNotFound(err) && ifcDeletionErr == nil {
			// If we're here the attachment has been deleted and if its interface (associated with the reference vni)
			// has been deleted as well. We do a best effort attempt to clear host IP and ifc name from deleted attachment status,
			// no retries because the update might fail for a lot of legitimate reasons (e.g. the attachment has been deleted from
			// etcd, the status has already been updated to a new value by the CA on the new node of the attachment, etc...). Ideally we
			// would retry depending on the returned error, but it's a overhead and clearing the status is not that important.
			err = ca.clearHostIPAndIfcNameFromLocalAttStatus(nsn, deletedIfcName)
			if err == nil {
				glog.V(3).Infof("Cleared local att %s status after deletion from local atts cache", nsn)
			} else {
				glog.V(3).Infof("Could not clear local att %s status after deletion from local atts cache: %s", nsn, err.Error())
			}
		}
		return ifcDeletionErr
	}

	return nil
}

func (ca *ConnectionAgent) processRemoteAtt(nsn k8stypes.NamespacedName, vni uint32) error {
	var (
		att *netv1a1.NetworkAttachment
		err error
	)
	remoteAttsLister := ca.getRemoteAttListerForVNI(vni)
	if remoteAttsLister != nil {
		att, err = remoteAttsLister.Get(nsn.Name)
	}

	if remoteAttsLister == nil || (err != nil && k8serrors.IsNotFound(err)) {
		// If we're here either the lister where to lookup the attachment was not found
		// because the last local attachment in the same VN as this remote att was deleted,
		// or this remote attachment was deleted. If there's an interface for this remote
		// attachment with its vni it's should be deleted, and if the vnState for this attachment
		// exists it should be removed from it.
		ca.removeRemoteAttFromVNState(nsn.Name, vni)
		return ca.attemptToDeleteRemoteAttIfc(nsn, vni)
	}

	// If we're here the attachment has been found. Add it to its vnState and create its
	// Interface if possible. It is "possible" if: (1) There's no local interface associated
	// with the attachment and (2) either there's no remote interface associated with the
	// attachment, or there's one with the same vni as the one of the attachment.
	added := ca.addRemoteAttToVNState(nsn.Name, vni)
	if added {
		return ca.attemptToCreateRemoteAttIfc(att)
	}

	return nil
}

func (ca *ConnectionAgent) attemptToCreateLocalAttIfc(att *netv1a1.NetworkAttachment) (ifcForAttStatus string,
	ifcForAttStatusValid bool,
	err error) {

	vni := att.Spec.VNI
	hostIP := ca.hostIP
	guestIP := gonet.ParseIP(att.Status.IPv4)
	guestMAC := generateMACAddr(vni, guestIP)
	newIfc := netfabric.NetworkInterface{
		Name:     generateIfcName(guestMAC),
		VNI:      vni,
		GuestMAC: guestMAC,
		GuestIP:  guestIP,
		HostIP:   hostIP,
	}

	attNSN := attNSN(att)
	oldIfc, oldIfcExists, newIfcSet := ca.testAndSwapLocalAttIfcIfVNIsMatch(attNSN, newIfc)
	if !newIfcSet {
		err = fmt.Errorf("Attachment %s: found interface with vni %d, attempting to create local one with vni %d. Dropping attempt",
			attNSN,
			oldIfc.VNI,
			vni,
		)
		return
	}

	_, attHasRemoteIfc := ca.getRemoteAttIfc(attNSN)
	if attHasRemoteIfc {
		if oldIfcExists {
			ca.testAndSwapLocalAttIfcIfVNIsMatch(attNSN, oldIfc)
		} else {
			ca.removeLocalAttIfc(attNSN)
		}
		err = fmt.Errorf("Attachment %s: Attempting to create local interface, found a remote one. Dropping attempt", attNSN)
		return
	}

	newIfcNeedsToBeCreated := !oldIfcExists || ifcsDiffer(oldIfc, newIfc)
	if !newIfcNeedsToBeCreated {
		// If we're here the old ifc exists and it's equal to the new one
		ifcForAttStatus = oldIfc.Name
		ifcForAttStatusValid = true
		return
	}

	if oldIfcExists {
		// If we're here there's an old interface which differs from the new one.
		// We delete the old interface before creating a new one.
		err = ca.netFabric.DeleteLocalIfc(oldIfc)
		if err != nil {
			ca.testAndSwapLocalAttIfcIfVNIsMatch(attNSN, oldIfc)
			return
		}
	}
	err = ca.netFabric.CreateLocalIfc(newIfc)
	if err != nil {
		ca.removeLocalAttIfc(attNSN)
		return
	}
	ifcForAttStatus = newIfc.Name
	ifcForAttStatusValid = true
	return
}

func (ca *ConnectionAgent) attemptToCreateRemoteAttIfc(att *netv1a1.NetworkAttachment) error {
	vni := att.Spec.VNI
	hostIP := gonet.ParseIP(att.Status.HostIP)
	guestIP := gonet.ParseIP(att.Status.IPv4)
	guestMAC := generateMACAddr(vni, guestIP)
	newIfc := netfabric.NetworkInterface{
		Name:     generateIfcName(guestMAC),
		VNI:      vni,
		GuestMAC: guestMAC,
		GuestIP:  guestIP,
		HostIP:   hostIP,
	}

	attNSN := attNSN(att)
	oldIfc, oldIfcExists, newIfcSet := ca.testAndSwapRemoteAttIfcIfVNIsMatch(attNSN, newIfc)
	if !newIfcSet {
		return fmt.Errorf("Attachment %s: found interface with vni %d, attempting to create remote one with vni %d. Dropping attempt",
			attNSN,
			oldIfc.VNI,
			vni,
		)
	}

	_, attHasLocalIfc := ca.getLocalAttIfc(attNSN)
	if attHasLocalIfc {
		if oldIfcExists {
			ca.testAndSwapRemoteAttIfcIfVNIsMatch(attNSN, oldIfc)
		} else {
			ca.removeRemoteAttIfc(attNSN)
		}
		return fmt.Errorf("Attachment %s: Attempting to create remote interface, found a local one. Dropping attempt",
			attNSN,
		)
	}

	newIfcNeedsToBeCreated := !oldIfcExists || ifcsDiffer(oldIfc, newIfc)
	if !newIfcNeedsToBeCreated {
		// If we're here the new and the old interface are equals, so we return
		return nil
	}

	var err error
	if oldIfcExists {
		// If we're here there's an old interface which differs from the new one.
		// We delete the old interface before creating a new one.
		err = ca.netFabric.DeleteRemoteIfc(oldIfc)
		if err != nil {
			ca.testAndSwapRemoteAttIfcIfVNIsMatch(attNSN, oldIfc)
			return err
		}
	}
	// create the new interface
	err = ca.netFabric.CreateRemoteIfc(newIfc)
	if err != nil {
		ca.removeRemoteAttIfc(attNSN)
	}
	return err
}

func (ca *ConnectionAgent) attemptToDeleteLocalAttIfc(nsn k8stypes.NamespacedName, vni uint32) (string, error) {
	ifc, ifcFound := ca.getLocalAttIfc(nsn)
	if ifcFound && ifc.VNI == vni {
		err := ca.netFabric.DeleteLocalIfc(ifc)
		if err == nil {
			ca.removeLocalAttIfc(nsn)
			return ifc.Name, nil
		}
		return "", err
	}
	return "", nil
}

func (ca *ConnectionAgent) attemptToDeleteRemoteAttIfc(nsn k8stypes.NamespacedName, vni uint32) error {
	ifc, ifcFound := ca.getRemoteAttIfc(nsn)
	if ifcFound && ifc.VNI == vni {
		err := ca.netFabric.DeleteRemoteIfc(ifc)
		if err == nil {
			ca.removeRemoteAttIfc(nsn)
		}
		return err
	}
	return nil
}

func (ca *ConnectionAgent) updateVNStateForExistingLocalAtt(localAtt *netv1a1.NetworkAttachment) (firstInVN bool) {
	vni := localAtt.Spec.VNI

	ca.vniToVnStateMutex.Lock()
	defer ca.vniToVnStateMutex.Unlock()

	localAttVnState := ca.vniToVnState[vni]

	if localAttVnState == nil {
		remoteAttsInformer, remoteAttsLister := v1a1AttsCustomNamespaceInformerAndLister(ca.kcs,
			resyncPeriod,
			localAtt.Namespace,
			fromFieldsSelectorToTweakListOptionsFunc(ca.remoteAttInVNWithVirtualIPHostIPAndIfcSelector(vni)))

		remoteAttsInformer.AddIndexers(map[string]k8scache.IndexFunc{attMACIndexName: attachmentMACAddr})

		remoteAttsInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
			AddFunc:    ca.onRemoteAttAdded,
			UpdateFunc: ca.onRemoteAttUpdated,
			DeleteFunc: ca.onRemoteAttRemoved,
		})

		remoteAttsInformerStopCh := make(chan struct{})
		go remoteAttsInformer.Run(aggregateTwoStopChannels(ca.stopCh, remoteAttsInformerStopCh))

		localAttVnState = &vnState{
			remoteAttsInformer:       remoteAttsInformer,
			remoteAttsInformerStopCh: remoteAttsInformerStopCh,
			remoteAttsLister:         remoteAttsLister,
			namespace:                localAtt.Namespace,
			localAtts:                make(map[string]struct{}, 1),
		}
		ca.vniToVnState[vni] = localAttVnState
		firstInVN = true
	}

	localAttVnState.localAtts[localAtt.Name] = struct{}{}
	return
}

func (ca *ConnectionAgent) addRemoteAttToVNState(remAttName string, vni uint32) (added bool) {
	ca.vniToVnStateMutex.Lock()
	defer ca.vniToVnStateMutex.Unlock()
	vnState := ca.vniToVnState[vni]
	if vnState != nil {
		if vnState.remoteAtts == nil {
			vnState.remoteAtts = make(map[string]struct{}, 1)
		}
		vnState.remoteAtts[remAttName] = struct{}{}
		added = true
	}
	return
}

func (ca *ConnectionAgent) updateVNStateAfterLocalAttDeletion(localAttName string, vni uint32) (lastInVN bool) {
	vnState := ca.removeLocalAttGetVnStateIfLast(localAttName, vni)
	if vnState != nil {
		// If we're here the attachment with name localAttName is the last local attachment in its VNI.
		ca.stopRemAttInformerAndEnqueueRemAttRefs(vnState, vni)
		lastInVN = true
	}
	return
}

func (ca *ConnectionAgent) removeLocalAttGetVnStateIfLast(localAttName string, vni uint32) *vnState {
	ca.vniToVnStateMutex.Lock()
	defer ca.vniToVnStateMutex.Unlock()
	vnState := ca.vniToVnState[vni]
	if vnState != nil {
		delete(vnState.localAtts, localAttName)
		if len(vnState.localAtts) == 0 {
			delete(ca.vniToVnState, vni)
			return vnState
		}
	}
	return nil
}

func (ca *ConnectionAgent) stopRemAttInformerAndEnqueueRemAttRefs(state *vnState, vni uint32) {
	close(state.remoteAttsInformerStopCh)
	for aRemoteAttName := range state.remoteAtts {
		ca.queue.Add(attQueueRef{
			vni:   vni,
			local: false,
			nsn: k8stypes.NamespacedName{
				Namespace: state.namespace,
				Name:      aRemoteAttName,
			},
		})
	}
}

func (ca *ConnectionAgent) removeRemoteAttFromVNState(remAttName string, vni uint32) {
	ca.vniToVnStateMutex.Lock()
	defer ca.vniToVnStateMutex.Unlock()
	vnState := ca.vniToVnState[vni]
	if vnState != nil {
		delete(vnState.remoteAtts, remAttName)
	}
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

func (ca *ConnectionAgent) setIfcNameAndHostIPInLocalAttStatusIfNeeded(localAtt *netv1a1.NetworkAttachment,
	ifcName string,
	ifcNameMightNeedUpdate bool) (updatedAtt *netv1a1.NetworkAttachment, err error) {

	if ca.localAttNeedsStatusUpdate(localAtt, ifcName, ifcNameMightNeedUpdate) {
		updatedAtt, err = ca.setIfcNameAndHostIPInLocalAttStatus(localAtt, ifcName, ifcNameMightNeedUpdate)
	}
	return
}

func (ca *ConnectionAgent) localAttNeedsStatusUpdate(localAtt *netv1a1.NetworkAttachment,
	ifcName string,
	ifcNameMightNeedUpdate bool) bool {

	return !ca.hostIP.Equal(gonet.ParseIP(localAtt.Status.HostIP)) ||
		(ifcNameMightNeedUpdate && localAtt.Status.IfcName != ifcName)
}

func (ca *ConnectionAgent) setIfcNameAndHostIPInLocalAttStatus(localAtt *netv1a1.NetworkAttachment,
	ifcName string,
	ifcNameMightNeedUpdate bool) (updatedAtt *netv1a1.NetworkAttachment, err error) {

	att2 := localAtt.DeepCopy()
	att2.Status.HostIP = ca.hostIP.String()
	if ifcNameMightNeedUpdate {
		att2.Status.IfcName = ifcName
	}
	updatedAtt, err = ca.netv1a1Ifc.NetworkAttachments(att2.Namespace).Update(att2)
	return
}

func (ca *ConnectionAgent) clearHostIPAndIfcNameFromLocalAttStatus(localAttNSN k8stypes.NamespacedName, ifcName string) (err error) {
	patchData := prepareStatusPatchData(ca.hostIP, ifcName)
	_, err = ca.netv1a1Ifc.NetworkAttachments(localAttNSN.Namespace).Patch(localAttNSN.Name, k8stypes.JSONPatchType, patchData, statusSubRes)
	return
}

func prepareStatusPatchData(hostIP gonet.IP, ifcName string) []byte {
	return []byte(fmt.Sprintf(`[{"op": "test", "path": "/status/hostIP", "value": "%s"},
	{"op": "remove", "path": "/status/hostIP"},
	{"op": "test", "path": "/status/ifcName", "value": "%s"},
	{"op": "remove", "path": "/status/ifcName"}]`, hostIP, ifcName))
}

func ifcsDiffer(oldIfc, newIfc netfabric.NetworkInterface) bool {
	return oldIfc.VNI != newIfc.VNI ||
		oldIfc.Name != newIfc.Name ||
		!bytes.Equal(oldIfc.GuestMAC, newIfc.GuestMAC) ||
		!oldIfc.GuestIP.Equal(newIfc.GuestIP) ||
		!oldIfc.HostIP.Equal(newIfc.HostIP)
}

func (ca *ConnectionAgent) onRemoteAttAdded(obj interface{}) {
	remoteAtt := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v addition to remote NetworkAttachments cache for VNI %d", remoteAtt, remoteAtt.Spec.VNI)
	remoteAttRef := ca.fromAttToEnquableRef(remoteAtt)
	ca.queue.Add(remoteAttRef)
}

func (ca *ConnectionAgent) onRemoteAttUpdated(oldObj, newObj interface{}) {
	oldRemoteAtt := oldObj.(*netv1a1.NetworkAttachment)
	newRemoteAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment update from %#+v to %#+v in remote NetworkAttachments cache for VNI %d", oldRemoteAtt, newRemoteAtt, newRemoteAtt.Spec.VNI)
	newRemoteAttRef := ca.fromAttToEnquableRef(newRemoteAtt)
	ca.queue.Add(newRemoteAttRef)
}

func (ca *ConnectionAgent) onRemoteAttRemoved(obj interface{}) {
	peeledObj := peel(obj)
	remoteAtt := peeledObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of NetworkAttachment %#+v removal from remote NetworkAttachments cache for VNI %d", remoteAtt, remoteAtt.Spec.VNI)
	remoteAttRef := ca.fromAttToEnquableRef(remoteAtt)
	ca.queue.Add(remoteAttRef)
}

func (ca *ConnectionAgent) getLocalAttIfc(ifcOwnerAttNSN k8stypes.NamespacedName) (ifc netfabric.NetworkInterface, ifcFound bool) {
	ca.localAttIfcsMutex.RLock()
	defer ca.localAttIfcsMutex.RUnlock()
	ifc, ifcFound = ca.localAttIfcs[ifcOwnerAttNSN]
	return
}

func (ca *ConnectionAgent) getRemoteAttIfc(ifcOwnerAttNSN k8stypes.NamespacedName) (ifc netfabric.NetworkInterface, ifcFound bool) {
	ca.remoteAttIfcsMutex.RLock()
	defer ca.remoteAttIfcsMutex.RUnlock()
	ifc, ifcFound = ca.remoteAttIfcs[ifcOwnerAttNSN]
	return
}

func (ca *ConnectionAgent) testAndSwapLocalAttIfcIfVNIsMatch(ifcOwnerAttNSN k8stypes.NamespacedName,
	newIfc netfabric.NetworkInterface) (oldIfc netfabric.NetworkInterface, oldIfcExists, newIfcSet bool) {

	ca.localAttIfcsMutex.Lock()
	defer ca.localAttIfcsMutex.Unlock()
	oldIfc, oldIfcExists = ca.localAttIfcs[ifcOwnerAttNSN]
	if oldIfcExists && oldIfc.VNI != newIfc.VNI {
		newIfcSet = false
		return
	}
	ca.localAttIfcs[ifcOwnerAttNSN] = newIfc
	newIfcSet = true
	return
}

func (ca *ConnectionAgent) testAndSwapRemoteAttIfcIfVNIsMatch(ifcOwnerAttNSN k8stypes.NamespacedName,
	newIfc netfabric.NetworkInterface) (oldIfc netfabric.NetworkInterface, oldIfcExists, newIfcSet bool) {

	ca.remoteAttIfcsMutex.Lock()
	defer ca.remoteAttIfcsMutex.Unlock()
	oldIfc, oldIfcExists = ca.remoteAttIfcs[ifcOwnerAttNSN]
	if oldIfcExists && oldIfc.VNI != newIfc.VNI {
		newIfcSet = false
		return
	}
	ca.remoteAttIfcs[ifcOwnerAttNSN] = newIfc
	newIfcSet = true
	return
}

func (ca *ConnectionAgent) removeLocalAttIfc(ifcOwnerAttNSN k8stypes.NamespacedName) {
	ca.localAttIfcsMutex.Lock()
	defer ca.localAttIfcsMutex.Unlock()
	delete(ca.localAttIfcs, ifcOwnerAttNSN)
}

func (ca *ConnectionAgent) removeRemoteAttIfc(ifcOwnerAttNSN k8stypes.NamespacedName) {
	ca.remoteAttIfcsMutex.Lock()
	defer ca.remoteAttIfcsMutex.Unlock()
	delete(ca.remoteAttIfcs, ifcOwnerAttNSN)
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
	return strings.Join(allSelectors, ",")
}

// Return a string representing a field selector that matches NetworkAttachments
// that run on a remote node on the Virtual Network identified by the given VNI and
// have a virtual IP and the host IP field set.
func (ca *ConnectionAgent) remoteAttInVNWithVirtualIPHostIPAndIfcSelector(vni uint32) string {
	// remoteAttSelector expresses the constraint that the NetworkAttachment runs on a remote node.
	remoteAttSelector := attNodeFieldName + notEqual + ca.localNodeName

	// attWithAnIPSelector, attWithHostIPSelector and attWithIfcSelector express the constraints that the
	// NetworkAttachment has the fields storing virtual IP, host IP and ifc name sets, by saying that such
	// fields must not be equal to the empty string.
	attWithAnIPSelector := attIPFieldName + notEqual
	attWithHostIPSelector := attHostIPFieldName + notEqual
	attWithIfcSelector := attIfcFieldName + notEqual

	// attInSpecificVNSelctor expresses the constraint that the NetworkAttachment is in the Virtual
	// Network identified by vni.
	attInSpecificVNSelctor := attVNIFieldName + equal + fmt.Sprint(vni)

	// Build and return a selector which is a logical AND between all the selectors defined above.
	allSelectors := []string{remoteAttSelector, attWithAnIPSelector, attWithHostIPSelector, attWithIfcSelector, attInSpecificVNSelctor}
	return strings.Join(allSelectors, ",")
}

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

// attachmentMACAddr is an Index function that computes the MAC address of a NetworkAttachment
func attachmentMACAddr(obj interface{}) (subnets []string, err error) {
	att := obj.(*netv1a1.NetworkAttachment)
	return []string{generateMACAddr(att.Spec.VNI, gonet.ParseIP(att.Status.IPv4)).String()}, nil
}

// TODO factor out attNSN in pkg controller/utils, as it is used both by
// the connection agent and the IPAM controller.
func attNSN(netAtt *netv1a1.NetworkAttachment) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{Namespace: netAtt.Namespace,
		Name: netAtt.Name}
}

func generateIfcName(macAddr gonet.HardwareAddr) string {
	return "kos" + macAddr.String()
}

func generateMACAddr(vni uint32, guestIPv4 gonet.IP) gonet.HardwareAddr {
	guestIPBytes := guestIPv4.To4()
	mac := make([]byte, 6, 6)
	mac[5] = byte(vni >> 0)
	mac[4] = byte(vni >> 8)
	mac[3] = byte(vni>>16) | (guestIPBytes[3] << 5)
	mac[2] = (guestIPBytes[3] >> 3) | (guestIPBytes[2] << 5)
	mac[1] = (guestIPBytes[2] >> 3) | (guestIPBytes[1] << 5)
	mac[0] = (guestIPBytes[1] & 0xF8) | (guestIPBytes[0] & 0x04) | 2
	return mac
}

// TODO factor out peel in pkg controller/utils, as it is used both by
// the connection agent and the IPAM controller.
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
