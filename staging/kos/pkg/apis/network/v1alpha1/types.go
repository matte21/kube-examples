/*
Copyright 2017 The Kubernetes Authors.

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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NetworkAttachmentList is a list of NetworkAttachment objects.
type NetworkAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []NetworkAttachment `json:"items" protobuf:"bytes,2,rep,name=items"`
}

type NetworkAttachmentSpec struct {
	// Node is the name of the node where the attachment should appear
	Node string `json:"node,omitempty" protobuf:"bytes,1,opt,name=node"`

	// Subnet is the object name of the subnet of this attachment
	Subnet string `json:"subnet,omitempty" protobuf:"bytes,2,opt,name=subnet"`
}

type NetworkAttachmentStatus struct {
	Errors NetworkAttachmentErrors `json:"errors,omitempty" protobuf:"bytes,1,opt,name=errors"`

	// LockUID is the UID of the IPLock object holding this attachment's
	// IP address, or the empty string when there is no address
	LockUID string `json:"lockUID,omitempty" protobuf:"bytes,2,opt,name=lockUID"`

	// IPv4 is non-empty when an address has been assigned
	IPv4 string `json:"ipv4,omitempty" protobuf:"bytes,3,opt,name=ipv4"`

	// IfcName is the name of the network interface that implements this
	// attachment on its node, or the empty string to indicate no
	// implementation.
	IfcName string `json:"ifcName,omitempty" protobuf:"bytes,4,opt,name=ifcname"`
}

type NetworkAttachmentErrors struct {
	// IPAM holds errors about the IP Address Management for this attachment
	IPAM []string `json:"ipam,omitempty" protobuf:"bytes,1,opt,name=ipam"`

	// Host holds errors from the node where this attachment is placed
	Host []string `json:"host,omitempty" protobuf:"bytes,2,opt,name=host"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type NetworkAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   NetworkAttachmentSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status NetworkAttachmentStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// SubnetSpec is the desired state of a subnet.
// For a given VNI, all the subnets having that VNI:
// - have disjoint IP ranges, and
// - are in the same Kubernetes API namespace.
type SubnetSpec struct {
	// IPv4 is the CIDR notation for the v4 address range of this subnet
	IPv4 string `json:"ipv4" protobuf:"bytes,1,name=ipv4"`

	// VNI identifies the virtual network.
	// Valid values are in the range [1,2097151].
	VNI uint32 `json:"vni" protobuf:"bytes,2,name=vni"`
}

type SubnetStatus struct {
	Errors []string `json:"errors,omitempty" protobuf:"bytes,1,opt,name=errors"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type Subnet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   SubnetSpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status SubnetStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SubnetList is a list of Subnet objects.
type SubnetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []Subnet `json:"items" protobuf:"bytes,2,rep,name=items"`
}

type IPLockSpec struct {
	SubnetName string `json:"subnetName" protobuf:"bytes,1,name=subnetName"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type IPLock struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec IPLockSpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IPLockList is a list of IPLock objects.
type IPLockList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []IPLock `json:"items" protobuf:"bytes,2,rep,name=items"`
}
