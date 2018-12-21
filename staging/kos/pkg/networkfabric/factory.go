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
	"fmt"
	"strings"
)

var fabricsRegistry map[string]Interface

// registerFabric registers a network fabric under the given name.
// After a successful invocation, an instance of the registered
// network fabric can be obtained by invoking NetFabricForName
// using the same name used for registration as the input parameter.
// This function is meant to be used ONLY in init() functions of network
// fabric implementers. Invoking registerFabric more than once with
// the same name panics.
func registerFabric(name string, fabric Interface) {
	if fabricsRegistry == nil {
		fabricsRegistry = make(map[string]Interface, 1)
	}
	if _, nameAlreadyInUse := fabricsRegistry[name]; nameAlreadyInUse {
		panic(fmt.Sprintf("a fabric with name %s is already registered. Use a different name", name))
	}
	fabricsRegistry[name] = fabric
}

// NetFabricForName returns the network fabric registered under the given name.
// An error is returned if no such fabric is found.
func NetFabricForName(name string) (Interface, error) {
	netFabric, nameIsRegistered := fabricsRegistry[name]
	if !nameIsRegistered {
		var err error
		if len(fabricsRegistry) > 0 {
			registeredNames := make([]string, 0, len(fabricsRegistry))
			for aRegisteredName := range fabricsRegistry {
				registeredNames = append(registeredNames, aRegisteredName)
			}
			err = fmt.Errorf("No fabric is registered with name %s. Registered names are: %s", name, strings.Join(registeredNames, ","))
		} else {
			err = fmt.Errorf("No fabric is registered with name %s because there are no registered fabrics", name)
		}
		return nil, err
	}
	return netFabric, nil
}
