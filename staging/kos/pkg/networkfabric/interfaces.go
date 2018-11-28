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

type Interface interface {
	Type() string
	CreateLocalIfc(NetworkInterface) error
	DeleteLocalIfc(NetworkInterface) error
	CreateRemoteIfc(NetworkInterface) error
	DeleteRemoteIfc(NetworkInterface) error
	// TODO consider having two separate List() methods for
	// remote interfaces and local interfaces respectively.
	List() ([]NetworkInterface, error)
}

type NetworkInterface struct {
	Name     string
	VNI      uint32
	GuestMAC net.HardwareAddr
	GuestIP  net.IP
	HostIP   net.IP
}
