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

package ipamctlr

import (
	"fmt"
	gonet "net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8swait "k8s.io/apimachinery/pkg/util/wait"
	k8scache "k8s.io/client-go/tools/cache"
	k8sworkqueue "k8s.io/client-go/util/workqueue"

	netv1a1 "k8s.io/examples/staging/kos/pkg/apis/network/v1alpha1"
	kosclientv1a1 "k8s.io/examples/staging/kos/pkg/client/clientset/versioned/typed/network/v1alpha1"
	netlistv1a1 "k8s.io/examples/staging/kos/pkg/client/listers/network/v1alpha1"
)

const (
	owningAttachmentIdxName = "owningAttachment"
	attachmentSubnetIdxName = "subnet"
)

type IPAMController struct {
	netIfc         kosclientv1a1.NetworkV1alpha1Interface
	subnetInformer k8scache.SharedInformer
	subnetLister   netlistv1a1.SubnetLister
	netattInformer k8scache.SharedIndexInformer
	netattLister   netlistv1a1.NetworkAttachmentLister
	lockInformer   k8scache.SharedIndexInformer
	lockLister     netlistv1a1.IPLockLister
	queue          k8sworkqueue.RateLimitingInterface
	workers        int
	attsMutex      sync.Mutex
	atts           map[k8stypes.NamespacedName]*NetworkAttachmentData
	subnetsMutex   sync.Mutex
	subnets        map[SubnetKey]*SubnetData
}

// NetworkAttachmentData holds the local state for a NetworkAttachment.
// The fields can only be accessed by a worker thread working on
// the NetworkAttachment.
// The data for a given attachment is used for two things:
// 1. to identify the SubnetData that holds a reference to the attachment,
// 2. to remember a status update while it is in flight.
// vni, subnetBaseU, and prefixLen are set properly if the attachment is
// referenced from the corresponding SubnetData; otherwise they MAY be zero.
// When the attachment's ResourceVersion is either anticipatingResourceVersion
// or anticiaptedResourceVersion and anticipatedIPv4 != nil then that address
// has been written into the attachment's status and there exists an IPLock
// that supports this, even if this controller has not yet been notified about
// that lock; when any other ResourceVersion
// is seen these three fields get set to their zero value.
type NetworkAttachmentData struct {
	vni                         uint32
	subnetBaseU                 uint32
	prefixLen                   int
	anticipatedIPv4             gonet.IP
	anticipatingResourceVersion string
	anticipatedResourceVersion  string
}

type SubnetKey struct {
	VNI       uint32
	baseU     uint32
	prefixLen int
}

// SubnetData holds what we care about a subnet.
// One of these is retained exactly as long as the subnet has an IPLock
// (as judged at notification time) or a NetworkAttachment (as judged
// in a queue worker thread).
// The fields that characterize the subnet are immutable.
type SubnetData struct {
	vni         uint32
	baseAddress uint32
	prefixLen   int

	// atts holds references to the NetworkAttachments of this subnet.
	// Every referenced attachment has a NetworkAttachmentData whose
	// values match this subnet.
	// Access only while holding IPAMController.attsMutex .
	atts map[k8stypes.NamespacedName]struct{}

	// locks holds references to the IPLocks of this subnet.
	// Access only while holding IPAMController.subnetsMutex .
	locks map[k8stypes.NamespacedName]struct{}

	addrsMutex sync.Mutex

	// addrs indicates which addresses are in use.
	// Access only while holding addrsMutex.
	addrs AddressSet
}

func NewIPAMController(netIfc kosclientv1a1.NetworkV1alpha1Interface,
	subnetInformer k8scache.SharedInformer,
	subnetLister netlistv1a1.SubnetLister,
	netattInformer k8scache.SharedIndexInformer,
	netattLister netlistv1a1.NetworkAttachmentLister,
	lockInformer k8scache.SharedIndexInformer,
	lockLister netlistv1a1.IPLockLister,
	queue k8sworkqueue.RateLimitingInterface,
	workers int) (ctlr *IPAMController, err error) {

	ctlr = &IPAMController{
		netIfc:         netIfc,
		subnetInformer: subnetInformer,
		subnetLister:   subnetLister,
		netattInformer: netattInformer,
		netattLister:   netattLister,
		lockInformer:   lockInformer,
		lockLister:     lockLister,
		queue:          queue,
		workers:        workers,
		atts:           make(map[k8stypes.NamespacedName]*NetworkAttachmentData),
		subnets:        make(map[SubnetKey]*SubnetData),
	}
	return
}

func (ctlr *IPAMController) Run(stopCh <-chan struct{}) error {
	defer k8sutilruntime.HandleCrash()
	defer ctlr.queue.ShutDown()
	ctlr.netattInformer.AddIndexers(map[string]k8scache.IndexFunc{attachmentSubnetIdxName: AttachmentSubnets})
	ctlr.lockInformer.AddIndexers(map[string]k8scache.IndexFunc{owningAttachmentIdxName: OwningAttachments})
	ctlr.subnetInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		ctlr.OnSubnetCreate,
		ctlr.OnSubnetUpdate,
		ctlr.OnSubnetDelete})
	ctlr.netattInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		ctlr.OnAttachmentCreate,
		ctlr.OnAttachmentUpdate,
		ctlr.OnAttachmentDelete})
	ctlr.lockInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		ctlr.OnLockCreate,
		ctlr.OnLockUpdate,
		ctlr.OnLockDelete})
	go ctlr.lockInformer.Run(stopCh)
	go ctlr.netattInformer.Run(stopCh)
	go ctlr.subnetInformer.Run(stopCh)
	glog.V(2).Infof("Informer Runs forked\n")
	if !k8scache.WaitForCacheSync(stopCh, ctlr.subnetInformer.HasSynced, ctlr.lockInformer.HasSynced, ctlr.netattInformer.HasSynced) {
		return fmt.Errorf("Caches failed to sync")
	}
	glog.V(2).Infof("Caches synced\n")
	for i := 0; i < ctlr.workers; i++ {
		go k8swait.Until(ctlr.processQueue, time.Second, stopCh)
	}
	glog.V(4).Infof("Launched %d workers\n", ctlr.workers)
	<-stopCh
	return nil
}

func (ctlr *IPAMController) OnSubnetCreate(obj interface{}) {
	subnet := obj.(*netv1a1.Subnet)
	ctlr.OnSubnetNotify(subnet, "creation")
}

func (ctlr *IPAMController) OnSubnetUpdate(oldObj, newObj interface{}) {
	subnet := newObj.(*netv1a1.Subnet)
	ctlr.OnSubnetNotify(subnet, "update")
}

func (ctlr *IPAMController) OnSubnetDelete(obj interface{}) {
	subnet := obj.(*netv1a1.Subnet)
	ctlr.OnSubnetNotify(subnet, "deletion")
}

func (ctlr *IPAMController) OnSubnetNotify(subnet *netv1a1.Subnet, op string) {
	indexer := ctlr.netattInformer.GetIndexer()
	subnetAttachments, err := indexer.ByIndex(attachmentSubnetIdxName, subnet.Name)
	if err != nil {
		glog.Errorf("NetworkAttachment indexer .ByIndex(%q, %q) failed: %s\n", attachmentSubnetIdxName, subnet.Name, err.Error())
		return
	}
	glog.V(4).Infof("Notified of %s of Subnet %s/%s, queuing %d attachments\n", op, subnet.Namespace, subnet.Name, len(subnetAttachments))
	for _, attObj := range subnetAttachments {
		att := attObj.(*netv1a1.NetworkAttachment)
		ctlr.queue.Add(AttNSN(att))
		glog.V(5).Infof("Queuing %s/%s due to notification of %s of Subnet %s/%s\n", att.Namespace, att.Name, op, subnet.Namespace, subnet.Name)
	}
}

func (ctlr *IPAMController) OnAttachmentCreate(obj interface{}) {
	att := obj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of creation of NetworkAttachment %#+v\n", att)
	ctlr.queue.Add(AttNSN(att))
}

func (ctlr *IPAMController) OnAttachmentUpdate(oldObj, newObj interface{}) {
	oldAtt := oldObj.(*netv1a1.NetworkAttachment)
	newAtt := newObj.(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of update of NetworkAttachment from %#+v to %#+v\n", oldAtt, newAtt)
	ctlr.queue.Add(AttNSN(newAtt))
}

func (ctlr *IPAMController) OnAttachmentDelete(obj interface{}) {
	att := Peel(obj).(*netv1a1.NetworkAttachment)
	glog.V(5).Infof("Notified of deletion of NetworkAttachment %#+v\n", att)
	ctlr.queue.Add(AttNSN(att))
}

func (ctlr *IPAMController) OnLockCreate(obj interface{}) {
	ipl := obj.(*netv1a1.IPLock)
	ctlr.OnLockNotify(ipl, "create", true)
}

func (ctlr *IPAMController) OnLockUpdate(old, new interface{}) {
	newIPL := new.(*netv1a1.IPLock)
	ctlr.OnLockNotify(newIPL, "update", true)
}

func (ctlr *IPAMController) OnLockDelete(obj interface{}) {
	ipl := obj.(*netv1a1.IPLock)
	ctlr.OnLockNotify(ipl, "delete", false)
}

func (ctlr *IPAMController) OnLockNotify(ipl *netv1a1.IPLock, op string, exists bool) {
	glog.V(4).Infof("Notified of %s of IPLock %s/%s=%s\n", op, ipl.Namespace, ipl.Name, string(ipl.UID))
	vni, subnetBaseU, offset, prefixLen, err := parseIPLockName(ipl.Name)
	if err != nil {
		glog.Errorf("Error parsing IPLock name %q: %s\n", ipl.Name, err.Error())
		return
	}
	snd := ctlr.getSubnetDataForLock(vni, subnetBaseU, prefixLen, ipl, exists)
	var changed bool
	var addrOp string
	if exists {
		addrOp = "ensure"
		changed = snd.TakeAddress(offset)
	} else {
		addrOp = "release"
		changed = snd.ReleaseAddress(offset)
	}
	glog.V(4).Infof("At notify of %s of IPLock %s/%s, %s %s, changed=%v\n", op, ipl.Namespace, ipl.Name, addrOp, Uint32ToIPv4(subnetBaseU+offset), changed)
	if !exists {
		ctlr.clearSubnetDataForLock(vni, subnetBaseU, prefixLen, ipl.Namespace, ipl.Name)
	}
}

func (ctlr *IPAMController) processQueue() {
	for {
		item, stop := ctlr.queue.Get()
		if stop {
			return
		}
		nsn := item.(k8stypes.NamespacedName)
		ctlr.processQueueItem(nsn)
	}
}

func (ctlr *IPAMController) processQueueItem(nsn k8stypes.NamespacedName) {
	defer ctlr.queue.Done(nsn)
	err := ctlr.processNetworkAttachment(nsn.Namespace, nsn.Name)
	requeues := ctlr.queue.NumRequeues(nsn)
	if err == nil {
		glog.V(4).Infof("Finished %s with %d requeues\n", nsn, requeues)
		ctlr.queue.Forget(nsn)
		return
	}
	glog.Warningf("Failed processing %s (%d requeues): %s\n", nsn, requeues, err.Error())
	ctlr.queue.AddRateLimited(nsn)
}

func (ctlr *IPAMController) processNetworkAttachment(ns, name string) error {
	att, err := ctlr.netattLister.NetworkAttachments(ns).Get(name)
	if err != nil && !k8serrors.IsNotFound(err) {
		// This should never happen.  No point in retrying.
		glog.Errorf("NetworkAttachment Lister failed to lookup %s/%s: %s\n",
			ns, name, err.Error())
		return nil
	}
	nadat := ctlr.getNetworkAttachmentData(ns, name, att != nil)
	subnetName, desiredVNI, desiredBaseU, desiredPrefixLen, lockInStatus, lockForStatus, err, ok := ctlr.analyzeAndRelease(ns, name, att, nadat)
	if err != nil || !ok {
		return err
	}
	if att == nil {
		if nadat != nil {
			ctlr.clearSubnetDataForAttachment(nadat.vni, nadat.subnetBaseU, nadat.prefixLen, ns, name)
			ctlr.clearNetworkAttachmentData(ns, name)
		}
		return nil
	}
	if lockInStatus.Obj != nil {
		return nil
	}
	var ipForStatus gonet.IP
	if lockForStatus.Obj != nil {
		ipForStatus = lockForStatus.GetIP()
		if ipForStatus.Equal(nadat.anticipatedIPv4) {
			return nil
		}
	} else if nadat.anticipatedIPv4 != nil {
		return nil
	} else {
		lockForStatus, ipForStatus, err = ctlr.pickAndLockAddress(ns, name, att, nadat, subnetName, desiredVNI, desiredBaseU, desiredPrefixLen)
		if err != nil {
			return err
		}
	}
	return ctlr.setIPInStatus(ns, name, att, nadat, lockForStatus, ipForStatus)
}

func (ctlr *IPAMController) analyzeAndRelease(ns, name string, att *netv1a1.NetworkAttachment, nadat *NetworkAttachmentData) (subnetName string, desiredVNI, desiredBaseU uint32, desiredPrefixLen int, lockInStatus, lockForStatus ParsedLock, err error, ok bool) {
	statusLockUID := "<none>"
	ipInStatus := ""
	attUID := "."
	attRV := "."
	var subnet *netv1a1.Subnet
	if att != nil {
		statusLockUID = att.Status.LockUID
		subnetName = att.Spec.Subnet
		ipInStatus = att.Status.IPv4
		attRV = att.ResourceVersion
		attUID = string(att.UID)
		subnet, err = ctlr.subnetLister.Subnets(ns).Get(subnetName)
		if err != nil && !k8serrors.IsNotFound(err) {
			glog.Errorf("Subnet Lister failed to lookup %s, referenced from attachment %s/%s: %s\n", subnetName, ns, name, err.Error())
			err = nil
			return
		}
		if subnet != nil {
			desiredVNI = subnet.Spec.VNI
			var ipNet *gonet.IPNet
			_, ipNet, err = gonet.ParseCIDR(subnet.Spec.IPv4)
			if err != nil {
				glog.Warningf("NetworkAttachment %s/%s references subnet %s, which has malformed Spec.IPv4 %q: %s\n", ns, name, subnetName, subnet.Spec.IPv4, err.Error())
				// Subnet update should trigger reconsideration of this attachment
				err = nil
				return
			}
			desiredBaseU = IPv4ToUint32(ipNet.IP)
			desiredPrefixLen, _ = ipNet.Mask.Size()
		} else {
			glog.Errorf("NetworkAttachment %s/%s references Subnet %s, which does not exist now\n", ns, name, subnetName)
			// This attachment will be requeued upon notification of subnet creation
			err = nil
			return
		}
	}
	var ownedObjs []interface{}
	iplIndexer := ctlr.lockInformer.GetIndexer()
	ownedObjs, err = iplIndexer.ByIndex(owningAttachmentIdxName, name)
	if err != nil {
		glog.Errorf("iplIndexer.ByIndex(%s, %s) failed: %s\n", owningAttachmentIdxName, name, err.Error())
		// Retry unlikely to help
		err = nil
		return
	}
	var timeSlippers, undesiredLocks, usableLocks ParsedLockList
	for _, ownedObj := range ownedObjs {
		ipl := ownedObj.(*netv1a1.IPLock)
		parsed, parseErr := NewParsedLock(ipl)
		if parseErr != nil {
			continue
		}
		_, ownerUID := GetOwner(ipl, "NetworkAttachment")
		if att != nil && ownerUID != att.UID {
			// This is for an older or newer edition of `att`; ignore it.
			// The garbage collector will get it if need be.
			// That may take a while, but that is better than deleting a lock
			// owned by a more recent edition of `att`.
			timeSlippers = timeSlippers.Append(parsed)
			continue
		}
		if parsed.VNI != desiredVNI || parsed.SubnetBaseU != desiredBaseU || parsed.PrefixLen != desiredPrefixLen {
			undesiredLocks = undesiredLocks.Append(parsed)
			continue
		}
		if string(parsed.UID) == statusLockUID && att != nil && att.Status.IPv4 != "" && att.Status.IPv4 == parsed.GetIP().String() {
			lockInStatus = parsed
		}
		usableLocks = usableLocks.Append(parsed)
	}
	if nadat != nil && (att == nil || nadat.anticipatingResourceVersion != att.ResourceVersion && nadat.anticipatedResourceVersion != att.ResourceVersion) {
		nadat.anticipatingResourceVersion = ""
		nadat.anticipatedResourceVersion = ""
		nadat.anticipatedIPv4 = nil
	}
	var usableToRelease ParsedLockList
	if att == nil {
		usableToRelease = usableLocks
	} else if lockInStatus.Obj != nil {
		usableToRelease, _ = usableLocks.RemFunc(lockInStatus)
	} else if len(usableLocks) > 0 {
		// Make a deterministic choice, so that if there are multiple
		// controllers they have a fighting chance of making the same decision.
		// Pick the newest, in case it is from an operator trying to fix something.
		lockForStatus = usableLocks.Best()
		usableToRelease, _ = usableLocks.RemFunc(lockForStatus)
	}
	locksToRelease, _ := undesiredLocks.AddListFunc(usableToRelease)
	anticipatedIPStr := "."
	if nadat != nil && nadat.anticipatedIPv4 != nil {
		anticipatedIPStr = nadat.anticipatedIPv4.String()
	}
	glog.V(4).Infof("processNetworkAttachment analyzed; na=%s/%s=%s, naRV=%s, subnet=%s, shouldExist=%v, desiredVNI=%x, desiredBaseU=%x, desiredPrefixLen=%d, lockInStatus=%s, lockForStatus=%s, locksToRelease=%s, timeSlippers=%s, Status.IPv4=%q, anticipatedIP=%s", ns, name, attUID, attRV, subnetName, att != nil, desiredVNI, desiredBaseU, desiredPrefixLen, lockInStatus, lockForStatus, locksToRelease, timeSlippers, ipInStatus, anticipatedIPStr)
	for _, lockToRelease := range locksToRelease {
		err = ctlr.deleteIPLockObject(lockToRelease)
		if err != nil {
			return
		}
	}
	ok = true
	return
}

func (ctlr *IPAMController) deleteIPLockObject(parsed ParsedLock) error {
	lockOps := ctlr.netIfc.IPLocks(parsed.ns)
	delOpts := k8smetav1.DeleteOptions{
		Preconditions: &k8smetav1.Preconditions{UID: &parsed.UID},
	}
	err := lockOps.Delete(parsed.name, &delOpts)
	if err == nil {
		glog.V(4).Infof("Deleted IPLock %s/%s=%s\n", parsed.ns, parsed.name, string(parsed.UID))
	} else if k8serrors.IsNotFound(err) || k8serrors.IsGone(err) {
		glog.V(4).Infof("IPLock %s/%s=%s is undesired and already gone\n", parsed.ns, parsed.name, string(parsed.UID))
	} else {
		return err
	}
	return nil
}

func (ctlr *IPAMController) pickAndLockAddress(ns, name string, att *netv1a1.NetworkAttachment, nadat *NetworkAttachmentData, subnetName string, desiredVNI, desiredBaseU uint32, desiredPrefixLen int) (lockForStatus ParsedLock, ipForStatus gonet.IP, err error) {
	if nadat.vni != desiredVNI || nadat.subnetBaseU != desiredBaseU || nadat.prefixLen != desiredPrefixLen {
		ctlr.clearSubnetDataForAttachment(nadat.vni, nadat.subnetBaseU, nadat.prefixLen, ns, name)
		nadat.vni = desiredVNI
		nadat.subnetBaseU = desiredBaseU
		nadat.prefixLen = desiredPrefixLen
	}
	snd := ctlr.getSubnetDataForAttachment(desiredVNI, desiredBaseU, desiredPrefixLen, att)
	offset, ok := snd.PickAddress()
	if !ok {
		err = fmt.Errorf("No IP address available in %x/%x/%d for %s/%s", desiredVNI, desiredBaseU, desiredPrefixLen, ns, name)
		return
	}
	ipForStatus = Uint32ToIPv4(desiredBaseU + offset)
	glog.V(4).Infof("Picked address %s from %x/%x/%d for %s/%s\n", ipForStatus, desiredVNI, desiredBaseU, desiredPrefixLen, ns, name)

	// Now, try to lock that address

	lockName := makeIPLockName4(desiredVNI, desiredBaseU, offset, desiredPrefixLen)
	lockForStatus = ParsedLock{ns, lockName, desiredVNI, desiredBaseU, desiredPrefixLen, offset, k8stypes.UID(""), time.Time{}, nil}
	aTrue := true
	owners := []k8smetav1.OwnerReference{{
		APIVersion: netv1a1.SchemeGroupVersion.String(),
		Kind:       "NetworkAttachment",
		Name:       name,
		UID:        att.UID,
		Controller: &aTrue,
	}}
	ipl := &netv1a1.IPLock{
		ObjectMeta: k8smetav1.ObjectMeta{
			Namespace:       ns,
			Name:            lockName,
			OwnerReferences: owners,
		},
		Spec: netv1a1.IPLockSpec{SubnetName: subnetName},
	}
	lockOps := ctlr.netIfc.IPLocks(ns)
	var ipl2 *netv1a1.IPLock
	for {
		ipl2, err = lockOps.Create(ipl)
		if err == nil {
			glog.V(4).Infof("Locked IP address %s for %s/%s=%s, lockName=%s, lockUID=%s\n", ipForStatus, ns, name, string(att.UID), lockName, string(ipl2.UID))
			break
		} else if k8serrors.IsAlreadyExists(err) {
			// Maybe it is ours
			var err2 error
			ipl2, err2 = lockOps.Get(lockName, k8smetav1.GetOptions{})
			var ownerName string
			var ownerUID k8stypes.UID
			if err2 == nil {
				ownerName, ownerUID = GetOwner(ipl2, "NetworkAttachment")
			} else if k8serrors.IsNotFound(err2) {
				// It was just there, now it is gone; try again to create
				glog.Warningf("IPLock %s disappeared before our eyes\n", lockName)
				continue
			} else {
				err = fmt.Errorf("Failed to fetch allegedly existing IPLock %s for %s/%s: %s\n", lockName, ns, name, err2.Error())
				return
			}
			if ownerName == name && ownerUID == att.UID {
				// Yes, it's ours!
				glog.V(4).Infof("Recovered lockName=%s, lockUID=%s on address %s for %s/%s=%s\n", lockName, string(ipl2.UID), ipForStatus, ns, name, string(att.UID))
				err = nil
				break
			} else {
				glog.V(4).Infof("Collision at IPLock %s for %s/%s=%s, owner is %s=%s\n", lockName, ns, name, string(att.UID), ownerName, string(ownerUID))
				// The cache in snd failed to avoid this collision.
				// Leave the bit set it the cache, something else is holding it.
				// Retry in a while
				err = fmt.Errorf("cache incoherence at %s", lockName)
				return
			}
		}
		releaseOK := snd.ReleaseAddress(offset)
		if k8serrors.IsInvalid(err) || strings.Contains(strings.ToLower(err.Error()), "invalid") {
			glog.Errorf("Permanent error creating IPLock %s for %s/%s (releaseOK=%v): %s\n", lockName, ns, name, releaseOK, err.Error())
			err = nil
		} else {
			glog.Warningf("Transient error creating IPLock %s for %s/%s (releaseOK=%v): %s\n", lockName, ns, name, releaseOK, err.Error())
			err = fmt.Errorf("Create of IPLock %s for %s/%s failed: %s", lockName, ns, name, err.Error())
		}
		return
	}
	lockForStatus.UID = ipl2.UID
	lockForStatus.CreationTime = ipl2.CreationTimestamp.Time
	lockForStatus.Obj = ipl2
	return
}

func (ctlr *IPAMController) getSubnetDataForAttachment(VNI, baseU uint32, prefixLen int, att *netv1a1.NetworkAttachment) *SubnetData {
	added := false
	ctlr.subnetsMutex.Lock()
	defer func() {
		ctlr.subnetsMutex.Unlock()
		if added {
			glog.V(4).Infof("Created SubnetData %x/%x/%d due to attachment %s/%s\n", VNI, baseU, prefixLen, att.Namespace, att.Name)
		}
	}()
	snd := ctlr.subnets[SubnetKey{VNI, baseU, prefixLen}]
	if snd == nil {
		snd = &SubnetData{vni: VNI, baseAddress: baseU, prefixLen: prefixLen,
			atts:  make(map[k8stypes.NamespacedName]struct{}),
			locks: make(map[k8stypes.NamespacedName]struct{}),
			addrs: NewBitmapAddressSet(1 << uint(32-prefixLen))}
		ctlr.subnets[SubnetKey{VNI, baseU, prefixLen}] = snd
		added = true
	}
	snd.atts[k8stypes.NamespacedName{att.Namespace, att.Name}] = struct{}{}
	return snd
}

func (ctlr *IPAMController) clearSubnetDataForAttachment(VNI, baseU uint32, prefixLen int, ns, name string) {
	removed := false
	ctlr.subnetsMutex.Lock()
	defer func() {
		ctlr.subnetsMutex.Unlock()
		if removed {
			glog.V(4).Infof("Removed SubnetData %x/%x/%d, last attachment was %s/%s\n", VNI, baseU, prefixLen, ns, name)
		}
	}()
	snd := ctlr.subnets[SubnetKey{VNI, baseU, prefixLen}]
	if snd == nil {
		return
	}
	if _, ok := snd.atts[k8stypes.NamespacedName{ns, name}]; !ok {
		return
	}
	delete(snd.atts, k8stypes.NamespacedName{ns, name})
	if len(snd.atts) > 0 || len(snd.locks) > 0 {
		return
	}
	delete(ctlr.subnets, SubnetKey{VNI, baseU, prefixLen})
	removed = true
	return
}

func (ctlr *IPAMController) getSubnetDataForLock(VNI, baseU uint32, prefixLen int, ipl *netv1a1.IPLock, addIfMissing bool) *SubnetData {
	added := false
	ctlr.subnetsMutex.Lock()
	defer func() {
		ctlr.subnetsMutex.Unlock()
		if added {
			glog.V(4).Infof("Created SubnetData due to lock %s/%s\n", ipl.Namespace, ipl.Name)
		}
	}()
	snd := ctlr.subnets[SubnetKey{VNI, baseU, prefixLen}]
	if snd == nil {
		if !addIfMissing {
			return nil
		}
		snd = &SubnetData{vni: VNI, baseAddress: baseU, prefixLen: prefixLen,
			atts:  make(map[k8stypes.NamespacedName]struct{}),
			locks: make(map[k8stypes.NamespacedName]struct{}),
			addrs: NewBitmapAddressSet(1 << uint(32-prefixLen))}
		ctlr.subnets[SubnetKey{VNI, baseU, prefixLen}] = snd
		added = true
	}
	snd.locks[k8stypes.NamespacedName{ipl.Namespace, ipl.Name}] = struct{}{}
	return snd
}

func (ctlr *IPAMController) clearSubnetDataForLock(VNI, baseU uint32, prefixLen int, ns, lockName string) {
	removed := false
	ctlr.subnetsMutex.Lock()
	defer func() {
		ctlr.subnetsMutex.Unlock()
		if removed {
			glog.V(4).Infof("Removed SubnetData, last lock was %s/%s\n", ns, lockName)
		}
	}()
	snd := ctlr.subnets[SubnetKey{VNI, baseU, prefixLen}]
	if snd == nil {
		return
	}
	if _, ok := snd.locks[k8stypes.NamespacedName{ns, lockName}]; !ok {
		return
	}
	delete(snd.locks, k8stypes.NamespacedName{ns, lockName})
	if len(snd.atts) > 0 || len(snd.locks) > 0 {
		return
	}
	delete(ctlr.subnets, SubnetKey{VNI, baseU, prefixLen})
	removed = true
	return
}

func (snd *SubnetData) PickAddress() (offset uint32, ok bool) {
	snd.addrsMutex.Lock()
	defer func() { snd.addrsMutex.Unlock() }()
	offsetI := snd.addrs.PickOne()
	if offsetI < 0 {
		return 0, false
	}
	return uint32(offsetI), true
}

func (snd *SubnetData) TakeAddress(offset uint32) (changed bool) {
	snd.addrsMutex.Lock()
	defer func() { snd.addrsMutex.Unlock() }()
	changed = snd.addrs.Take(int(offset))
	return
}

func (snd *SubnetData) ReleaseAddress(offset uint32) (changed bool) {
	snd.addrsMutex.Lock()
	defer func() { snd.addrsMutex.Unlock() }()
	changed = snd.addrs.Release(int(offset))
	return
}

func (ctlr *IPAMController) setIPInStatus(ns, name string, att *netv1a1.NetworkAttachment, nadat *NetworkAttachmentData, lockForStatus ParsedLock, ipForStatus gonet.IP) error {
	att2 := att.DeepCopy()
	att2.Status.LockUID = string(lockForStatus.UID)
	att2.Status.IPv4 = ipForStatus.String()
	attachmentOps := ctlr.netIfc.NetworkAttachments(ns)
	att3, err := attachmentOps.Update(att2)
	if err == nil {
		glog.V(4).Infof("Recorded locked address %s in status of %s/%s, old ResourceVersion=%s, new ResourceVersion=%s\n", ipForStatus, ns, name, att.ResourceVersion, att3.ResourceVersion)
		nadat.anticipatingResourceVersion = att.ResourceVersion
		nadat.anticipatedResourceVersion = att3.ResourceVersion
		nadat.anticipatedIPv4 = ipForStatus
		return nil
	}
	if k8serrors.IsNotFound(err) {
		glog.V(4).Infof("NetworkAttachment %s/%s was deleted while address %s was allocated\n", ns, name, ipForStatus)
		return nil
	}
	return fmt.Errorf("Failed to update status of NetworkAttachment %s/%s to record address %s: %s", ns, name, ipForStatus, err.Error())
}

func (ctlr *IPAMController) getNetworkAttachmentData(ns, name string, addIfMissing bool) *NetworkAttachmentData {
	added := false
	ctlr.attsMutex.Lock()
	defer func() {
		ctlr.attsMutex.Unlock()
		if added {
			glog.V(4).Infof("Created NetworkAttachmentData for %s/%s\n", ns, name)
		}
	}()
	nadata := ctlr.atts[k8stypes.NamespacedName{ns, name}]
	if nadata == nil {
		if !addIfMissing {
			return nil
		}
		nadata = &NetworkAttachmentData{}
		ctlr.atts[k8stypes.NamespacedName{ns, name}] = nadata
		added = true
	}
	return nadata
}

func (ctlr *IPAMController) clearNetworkAttachmentData(ns, name string) {
	had := false
	ctlr.attsMutex.Lock()
	defer func() {
		ctlr.attsMutex.Unlock()
		if had {
			glog.V(4).Infof("Deleted NetworkAttachmentData for %s/%s\n", ns, name)
		}
	}()
	_, had = ctlr.atts[k8stypes.NamespacedName{ns, name}]
	if had {
		delete(ctlr.atts, k8stypes.NamespacedName{ns, name})
	}
}

func AttachmentSubnets(obj interface{}) (subnets []string, err error) {
	att := obj.(*netv1a1.NetworkAttachment)
	return []string{att.Spec.Subnet}, nil
}

var _ k8scache.IndexFunc = AttachmentSubnets

func OwningAttachments(obj interface{}) (owners []string, err error) {
	meta := obj.(k8smetav1.Object)
	owners = make([]string, 0, 1)
	for _, oref := range meta.GetOwnerReferences() {
		if oref.Kind == "NetworkAttachment" && oref.Controller != nil && *oref.Controller {
			owners = append(owners, oref.Name)
		}
	}
	return
}

var _ k8scache.IndexFunc = OwningAttachments

func GetOwner(obj k8smetav1.Object, ownerKind string) (name string, uid k8stypes.UID) {
	for _, oref := range obj.GetOwnerReferences() {
		if oref.Kind == ownerKind && oref.Controller != nil && *oref.Controller {
			name = oref.Name
			uid = oref.UID
		}
	}
	return
}

// Peel removes the k8scache.DeletedFinalStateUnknown wrapper,
// if any, and returns the result as a k8sruntime.Object.
func Peel(obj interface{}) k8sruntime.Object {
	switch o := obj.(type) {
	case *k8scache.DeletedFinalStateUnknown:
		return o.Obj.(k8sruntime.Object)
	case k8sruntime.Object:
		return o
	default:
		panic(obj)
	}
}

func SubnetNSN(obj *netv1a1.Subnet) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{obj.Namespace, obj.Name}
}

func AttNSN(obj *netv1a1.NetworkAttachment) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{obj.Namespace, obj.Name}
}

func LockNSN(obj *netv1a1.IPLock) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{obj.Namespace, obj.Name}
}

func IPv4ToUint32(ip gonet.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 + uint32(v4[1])<<16 + uint32(v4[2])<<8 + uint32(v4[3])
}

func Uint32ToIPv4(i uint32) gonet.IP {
	return gonet.IPv4(uint8(i>>24), uint8(i>>16), uint8(i>>8), uint8(i))
}

func makeIPLockName(subnet *netv1a1.Subnet, ip gonet.IP) string {
	_, ipNet, _ := gonet.ParseCIDR(subnet.Spec.IPv4)
	return makeIPLockName3(subnet.Spec.VNI, ipNet, ip)
}

func makeIPLockName3(VNI uint32, ipNet *gonet.IPNet, ip gonet.IP) string {
	baseU := IPv4ToUint32(ipNet.IP)
	ipU := IPv4ToUint32(ip)
	prefixLen, _ := ipNet.Mask.Size()
	return makeIPLockName4(VNI, baseU, ipU-baseU, prefixLen)
}

func makeIPLockName4(VNI uint32, baseU, offset uint32, prefixLen int) string {
	return fmt.Sprintf("v1-%x-%x-%x-%d", VNI, baseU, offset, prefixLen)
}

func parseIPLockName(lockName string) (VNI uint32, subnetBaseU, offset uint32, prefixLen int, err error) {
	parts := strings.Split(lockName, "-")
	if len(parts) != 5 || parts[0] != "v1" {
		return 0, 0, 0, 0, fmt.Errorf("Lock name %q is malformed", lockName)
	}
	vni64, err2 := strconv.ParseUint(parts[1], 16, 32)
	if err2 != nil {
		return 0, 0, 0, 0, fmt.Errorf("VNI in lockName %q is malformed: %s", lockName, err2)
	}
	VNI = uint32(vni64)
	base64, err2 := strconv.ParseUint(parts[2], 16, 32)
	if err2 != nil {
		return 0, 0, 0, 0, fmt.Errorf("Base address in lockName %q is malformed: %s", lockName, err2)
	}
	subnetBaseU = uint32(base64)
	offset64, err2 := strconv.ParseUint(parts[3], 16, 32)
	if err2 != nil {
		return 0, 0, 0, 0, fmt.Errorf("Prefixlen in lockName %q is malformed: %s", lockName, err2)
	}
	offset = uint32(offset64)
	prefixLen64, err := strconv.ParseInt(parts[4], 10, 32)
	if err2 != nil {
		return 0, 0, 0, 0, fmt.Errorf("Prefixlen in lockName %q is malformed: %s", lockName, err2)
	}
	prefixLen = int(prefixLen64)
	return
}

// ParsedLock characterizes an IPLock object and
// optionally including a pointer to the object.
// The subnet's address block is included so that if that
// ever changes the old locks will be deemed undesired.
type ParsedLock struct {
	ns   string
	name string

	VNI uint32

	// SubnetBaseU is the first address of the subnet,
	// expressed as a number.
	SubnetBaseU uint32

	PrefixLen int

	// Offset is the difference between the locked address and SubnetBase
	Offset uint32

	// UID identifies the lock object
	UID k8stypes.UID

	// CreationTime characterizes the lock object
	CreationTime time.Time
	Obj          *netv1a1.IPLock
}

var zeroParsedLock ParsedLock

func NewParsedLock(ipl *netv1a1.IPLock) (ans ParsedLock, err error) {
	vni, base, offset, prefixLen, err := parseIPLockName(ipl.Name)
	if err == nil {
		ans = ParsedLock{ipl.Namespace, ipl.Name, vni, base, prefixLen, offset, ipl.UID, ipl.CreationTimestamp.Time, ipl}
	}
	return
}

var _ fmt.Stringer = ParsedLock{}

func (x ParsedLock) String() string {
	return fmt.Sprintf("%d/%x+%x/%d=%s@%s", x.VNI, x.SubnetBaseU, x.Offset, x.PrefixLen, string(x.UID), x.CreationTime)
}

func (x ParsedLock) GetIP() gonet.IP {
	return Uint32ToIPv4(x.SubnetBaseU + x.Offset)
}

func (x ParsedLock) GetIPNet() *gonet.IPNet {
	base := Uint32ToIPv4(x.SubnetBaseU)
	mask := gonet.CIDRMask(x.PrefixLen, 32)
	return &gonet.IPNet{base, mask}
}

func (x ParsedLock) Equal(y ParsedLock) bool {
	return x.VNI == y.VNI && x.UID == y.UID &&
		x.CreationTime == y.CreationTime && x.SubnetBaseU == y.SubnetBaseU &&
		x.Offset == y.Offset && x.PrefixLen == y.PrefixLen
}

func (x ParsedLock) IsBetterThan(y ParsedLock) bool {
	if x.CreationTime != y.CreationTime {
		return x.CreationTime.After(y.CreationTime)
	}
	return strings.Compare(string(x.UID), string(y.UID)) > 0
}

type ParsedLockList []ParsedLock

func (list ParsedLockList) String() string {
	var b strings.Builder
	b.WriteString("[")
	for idx, parsed := range list {
		if idx > 0 {
			b.WriteString(", ")
		}
		b.WriteString(parsed.String())
	}
	b.WriteString("]")
	return b.String()
}

func (list ParsedLockList) Best() ParsedLock {
	if len(list) == 0 {
		return ParsedLock{}
	}
	ans := list[0]
	for _, elt := range list[1:] {
		if elt.IsBetterThan(ans) {
			ans = elt
		}
	}
	return ans
}

func (list ParsedLockList) Has(elt ParsedLock) bool {
	if len(list) == 0 {
		return false
	}
	for _, x := range list {
		if x.Equal(elt) {
			return true
		}
	}
	return false
}

func (list ParsedLockList) Append(elt ...ParsedLock) ParsedLockList {
	return ParsedLockList(append(list, elt...))
}

func (list ParsedLockList) AddFunc(elt ParsedLock) (with ParsedLockList, diff bool) {
	if len(list) == 0 {
		return []ParsedLock{elt}, true
	}
	for _, x := range list {
		if x.Equal(elt) {
			return list, false
		}
	}
	with = make([]ParsedLock, 0, 1+len(list))
	with = append(with, list...)
	with = append(with, elt)
	return with, true
}

func (list ParsedLockList) AddListFunc(list2 ParsedLockList) (with ParsedLockList, diff bool) {
	with, diff = list, false
	for _, elt := range list2 {
		var diffHere bool
		with, diffHere = with.AddFunc(elt)
		diff = diff || diffHere
	}
	return
}

func (list ParsedLockList) RemFunc(elt ParsedLock) (sans ParsedLockList, diff bool) {
	if len(list) == 0 {
		return nil, false
	}
	l := len(list)
	if l == 1 {
		if elt.Equal(list[0]) {
			return nil, true
		} else {
			return list, false
		}
	}
	for i, x := range list {
		if x.Equal(elt) {
			sans = make([]ParsedLock, 0, len(list)-1)
			sans = append(sans, list[0:i]...)
			if i+1 < l {
				sans = append(sans, list[i+1:]...)
			}
			return sans, true
		}
	}
	return list, false
}
