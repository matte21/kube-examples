/*
Copyright 2019 The Kubernetes Authors.

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
	"github.com/golang/glog"
	"sync"
)

const name = "logger"

// Logger is a fake network interface fabric useful
// for debugging/testing. It does nothing but logging.
type logger struct {
	localIfcsMutex sync.RWMutex
	localIfcs      map[string]LocalNetIfc

	remoteIfcsMutex sync.RWMutex
	remoteIfcs      map[string]RemoteNetIfc
}

func (l *logger) Name() string {
	return name
}

func (l *logger) CreateLocalIfc(ifc LocalNetIfc) error {
	l.localIfcsMutex.Lock()
	l.localIfcs[ifc.Name] = ifc
	l.localIfcsMutex.Unlock()
	glog.Infof("Created local interface %#+v", ifc)
	return nil
}

func (l *logger) DeleteLocalIfc(ifc LocalNetIfc) error {
	l.localIfcsMutex.Lock()
	delete(l.localIfcs, ifc.Name)
	l.localIfcsMutex.Unlock()
	glog.Infof("Deleted local interface %#+v", ifc)
	return nil
}

func (l *logger) CreateRemoteIfc(ifc RemoteNetIfc) error {
	l.remoteIfcsMutex.Lock()
	l.remoteIfcs[ifc.GuestMAC.String()] = ifc
	l.remoteIfcsMutex.Unlock()
	glog.Infof("Created remote interface %#+v", ifc)
	return nil
}

func (l *logger) DeleteRemoteIfc(ifc RemoteNetIfc) error {
	l.remoteIfcsMutex.Lock()
	delete(l.remoteIfcs, ifc.GuestMAC.String())
	l.remoteIfcsMutex.Unlock()
	glog.Infof("Deleted remote interface %#+v", ifc)
	return nil
}

func (l *logger) ListLocalIfcs() ([]LocalNetIfc, error) {
	l.localIfcsMutex.RLock()
	defer l.localIfcsMutex.RUnlock()
	localIfcsList := make([]LocalNetIfc, 0, len(l.localIfcs))
	for _, ifc := range l.localIfcs {
		localIfcsList = append(localIfcsList, ifc)
	}
	return localIfcsList, nil
}

func (l *logger) ListRemoteIfcs() ([]RemoteNetIfc, error) {
	l.remoteIfcsMutex.RLock()
	defer l.remoteIfcsMutex.RUnlock()
	remoteIfcsList := make([]RemoteNetIfc, 0, len(l.remoteIfcs))
	for _, ifc := range l.remoteIfcs {
		remoteIfcsList = append(remoteIfcsList, ifc)
	}
	return remoteIfcsList, nil
}

func init() {
	registerFabric(name, &logger{
		localIfcs:  make(map[string]LocalNetIfc),
		remoteIfcs: make(map[string]RemoteNetIfc),
	})
}
