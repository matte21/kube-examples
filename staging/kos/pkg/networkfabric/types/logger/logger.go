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

// A fake network interface fabric useful for debugging/testing.
// It does nothing but logging.
type Logger struct {
	logLvl glog.Level
	// TODO consider braking this into two separate lists for local ifcs and
	// remote ifcs respectively
	ifcs map[string]netfabric.NetworkInterface
}

func NewLogger(logLvl glog.Level) (l *Logger) {
	l = &Logger{
		logLvl: logLvl,
		ifcs:   make(map[string]netfabric.NetworkInterface),
	}
	return
}

func (l *Logger) Type() string {
	return fabricType
}

func (l *Logger) CreateLocalIfc(ifc netfabric.NetworkInterface) error {
	l.ifcs[ifc.Name] = ifc
	glog.V(l.logLvl).Infof("Created local interface %v\n", ifc)
	return nil
}

func (l *Logger) DeleteLocalIfc(ifc netfabric.NetworkInterface) error {
	delete(l.ifcs, ifc.Name)
	glog.V(l.logLvl).Infof("Deleted local interface %v\n", ifc)
	return nil
}

func (l *Logger) CreateRemoteIfc(ifc netfabric.NetworkInterface) error {
	l.ifcs[ifc.Name] = ifc
	glog.V(l.logLvl).Infof("Created remote interface %v\n", ifc)
	return nil
}

func (l *Logger) DeleteRemoteIfc(ifc netfabric.NetworkInterface) error {
	delete(l.ifcs, ifc.Name)
	glog.V(l.logLvl).Infof("Deleted remote interface %v\n", ifc)
	return nil
}

func (l *Logger) List() ([]netfabric.NetworkInterface, error) {
	ifcsList := make([]netfabric.NetworkInterface, 0, len(l.ifcs))
	for _, ifc := range l.ifcs {
		ifcsList = append(ifcsList, ifc)
	}
	return ifcsList, nil
}
