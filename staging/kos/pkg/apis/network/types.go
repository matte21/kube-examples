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

package network

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NetworkAttachmentList is a list of NetworkAttachment objects.
type NetworkAttachmentList struct {
	metav1.TypeMeta
	metav1.ListMeta

	Items []NetworkAttachment
}

type NetworkAttachmentSpec struct {
	// Node is the name of the node where the attachment should appear
	Node string

	// Subnet is the object name of the subnet of this attachment
	Subnet string

	// VNI identifies the virtual network this NetworkAttachment belongs to.
	// Valid values are in the range [1,2097151].
	VNI uint32
}

type NetworkAttachmentStatus struct {
	Errors NetworkAttachmentErrors

	// LockUID is the UID of the IPLock object holding this attachment's
	// IP address, or the empty string when there is no address
	LockUID string

	// IPv4 is non-empty when an address has been assigned
	IPv4 string

	// IfcName is the name of the network interface that implements this
	// attachment on its node, or the empty string to indicate no
	// implementation.
	IfcName string

	// HostIP is the IP address of the node the attachment is bound to.
	HostIP string
}

type NetworkAttachmentErrors struct {
	// IPAM holds errors about the IP Address Management for this attachment
	IPAM []string

	// Host holds errors from the node where this attachment is placed
	Host []string
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type NetworkAttachment struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   NetworkAttachmentSpec
	Status NetworkAttachmentStatus
}

// SubnetSpec is the desired state of a subnet.
// For a given VNI, all the subnets having that VNI:
// - have disjoint IP ranges, and
// - are in the same Kubernetes API namespace.
type SubnetSpec struct {
	// IPv4 is the CIDR notation for the v4 address range of this subnet
	IPv4 string

	// VNI identifies the virtual network.
	// Valid values are in the range [1,2097151].
	VNI uint32
}

type SubnetStatus struct {
	Errors []string
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type Subnet struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec   SubnetSpec
	Status SubnetStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SubnetList is a list of Subnet objects.
type SubnetList struct {
	metav1.TypeMeta
	metav1.ListMeta

	// Items is a list of Subnets
	Items []Subnet
}

type IPLockSpec struct {
	SubnetName string
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPLock struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec IPLockSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IPLockList is a list of IPLock objects.
type IPLockList struct {
	metav1.TypeMeta
	metav1.ListMeta

	// Items is a list of IPLocks
	Items []IPLock
}
