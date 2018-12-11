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
	"github.com/golang/glog"
	"sync"
)

const name = "logger"

// Logger is a fake network interface fabric useful
// for debugging/testing. It does nothing but logging.
type logger struct {
	localIfcsMutex sync.RWMutex
	localIfcs      map[string]NetworkInterface

	remoteIfcsMutex sync.RWMutex
	remoteIfcs      map[string]NetworkInterface
}

func (l *logger) Name() string {
	return name
}

func (l *logger) CreateLocalIfc(ifc NetworkInterface) error {
	l.localIfcsMutex.Lock()
	l.localIfcs[ifc.Name] = ifc
	l.localIfcsMutex.Unlock()
	glog.Infof("Created local interface %v\n", ifc)
	return nil
}

func (l *logger) DeleteLocalIfc(ifc NetworkInterface) error {
	l.localIfcsMutex.Lock()
	delete(l.localIfcs, ifc.Name)
	l.localIfcsMutex.Unlock()

	glog.Infof("Deleted local interface %v\n", ifc)
	return nil
}

func (l *logger) CreateRemoteIfc(ifc NetworkInterface) error {
	l.remoteIfcsMutex.Lock()
	l.remoteIfcs[ifc.Name] = ifc
	l.remoteIfcsMutex.Unlock()
	glog.Infof("Created remote interface %v\n", ifc)
	return nil
}

func (l *logger) DeleteRemoteIfc(ifc NetworkInterface) error {
	l.remoteIfcsMutex.Lock()
	delete(l.remoteIfcs, ifc.Name)
	l.remoteIfcsMutex.Unlock()
	glog.Infof("Deleted remote interface %v\n", ifc)
	return nil
}

func (l *logger) ListLocalIfcs() ([]NetworkInterface, error) {
	l.localIfcsMutex.RLock()
	defer l.localIfcsMutex.RUnlock()
	localIfcsList := make([]NetworkInterface, 0, len(l.localIfcs))
	for _, ifc := range l.localIfcs {
		localIfcsList = append(localIfcsList, ifc)
	}
	return localIfcsList, nil
}

func (l *logger) ListRemoteIfcs() ([]NetworkInterface, error) {
	l.remoteIfcsMutex.RLock()
	defer l.remoteIfcsMutex.RUnlock()
	remoteIfcsList := make([]NetworkInterface, 0, len(l.remoteIfcs))
	for _, ifc := range l.remoteIfcs {
		remoteIfcsList = append(remoteIfcsList, ifc)
	}
	return remoteIfcsList, nil
}

func init() {
	registerFactory(name, func() Interface {
		return &logger{
			localIfcs:  make(map[string]NetworkInterface),
			remoteIfcs: make(map[string]NetworkInterface),
		}
	})
}
