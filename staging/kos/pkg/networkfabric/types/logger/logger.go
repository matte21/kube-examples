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

package logger

import (
	"github.com/golang/glog"

	netfabric "k8s.io/examples/staging/kos/pkg/networkfabric"
)

const fabricType = "logger"

// Logger is a fake network interface fabric useful
// for debugging/testing. It does nothing but logging.
type Logger struct {
	logLvl     glog.Level
	localIfcs  map[string]netfabric.NetworkInterface
	remoteIfcs map[string]netfabric.NetworkInterface
}

func NewLogger(logLvl glog.Level) (l *Logger) {
	l = &Logger{
		logLvl:     logLvl,
		localIfcs:  make(map[string]netfabric.NetworkInterface),
		remoteIfcs: make(map[string]netfabric.NetworkInterface),
	}
	return
}

func (l *Logger) Type() string {
	return fabricType
}

func (l *Logger) CreateLocalIfc(ifc netfabric.NetworkInterface) error {
	l.localIfcs[ifc.Name] = ifc
	glog.V(l.logLvl).Infof("Created local interface %v\n", ifc)
	return nil
}

func (l *Logger) DeleteLocalIfc(ifc netfabric.NetworkInterface) error {
	delete(l.localIfcs, ifc.Name)
	glog.V(l.logLvl).Infof("Deleted local interface %v\n", ifc)
	return nil
}

func (l *Logger) CreateRemoteIfc(ifc netfabric.NetworkInterface) error {
	l.remoteIfcs[ifc.Name] = ifc
	glog.V(l.logLvl).Infof("Created remote interface %v\n", ifc)
	return nil
}

func (l *Logger) DeleteRemoteIfc(ifc netfabric.NetworkInterface) error {
	delete(l.remoteIfcs, ifc.Name)
	glog.V(l.logLvl).Infof("Deleted remote interface %v\n", ifc)
	return nil
}

func (l *Logger) ListLocalIfcs() ([]netfabric.NetworkInterface, error) {
	localIfcsList := make([]netfabric.NetworkInterface, 0, len(l.localIfcs))
	for _, ifc := range l.localIfcs {
		localIfcsList = append(localIfcsList, ifc)
	}
	return localIfcsList, nil
}

func (l *Logger) ListRemoteIfcs() ([]netfabric.NetworkInterface, error) {
	remoteIfcsList := make([]netfabric.NetworkInterface, 0, len(l.remoteIfcs))
	for _, ifc := range l.remoteIfcs {
		remoteIfcsList = append(remoteIfcsList, ifc)
	}
	return remoteIfcsList, nil
}
