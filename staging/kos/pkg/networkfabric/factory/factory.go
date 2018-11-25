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

	netfabric "k8s.io/examples/staging/kos/pkg/networkfabric"
	"k8s.io/examples/staging/kos/pkg/networkfabric/types/logger"
)

const (
	// TODO make logLvl settable from the outside (env variable, command line flag, etc...)
	logLvl     = glog.Level(1)
	loggerType = "logger"
)

func NewNetFabricForType(netFabricType string) netfabric.Interface {
	// TODO add openflow based fabric
	switch netFabricType {
	case loggerType:
		return logger.NewLogger(logLvl)
	default:
		// Logger is the default fabric
		return logger.NewLogger(logLvl)
	}
}
