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

package networkfabric

import (
	"net"
)

// Interface is the interface of a network fabric which allows
// the user to implement Netowrk Interfaces. The implementer
// MUST return nil when the user attempts to delete an Interface
// which does not exist.
type Interface interface {
	Name() string
	CreateLocalIfc(NetworkInterface) error
	DeleteLocalIfc(NetworkInterface) error
	CreateRemoteIfc(NetworkInterface) error
	DeleteRemoteIfc(NetworkInterface) error
	ListLocalIfcs() ([]NetworkInterface, error)
	ListRemoteIfcs() ([]NetworkInterface, error)
}

type NetworkInterface struct {
	Name     string
	VNI      uint32
	GuestMAC net.HardwareAddr
	GuestIP  net.IP
	HostIP   net.IP
}
