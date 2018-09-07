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

package networkinitializer_test

import (
	"testing"
	"time"

	"k8s.io/apiserver/pkg/admission"
	"k8s.io/examples/staging/kos/pkg/admission/networkinitializer"
	"k8s.io/examples/staging/kos/pkg/client/clientset/internalversion/fake"
	informers "k8s.io/examples/staging/kos/pkg/client/informers/internalversion"
)

// TestWantsInternalNetworkInformerFactory ensures that the informer factory is injected
// when the WantsInternalNetworkInformerFactory interface is implemented by a plugin.
func TestWantsInternalNetworkInformerFactory(t *testing.T) {
	cs := &fake.Clientset{}
	sf := informers.NewSharedInformerFactory(cs, time.Duration(1)*time.Second)
	target := networkinitializer.New(sf)

	wantNetworkInformerFactory := &wantInternalNetworkInformerFactory{}
	target.Initialize(wantNetworkInformerFactory)
	if wantNetworkInformerFactory.sf != sf {
		t.Errorf("expected informer factory to be initialized")
	}
}

// wantInternalNetworkInformerFactory is a test stub that fulfills the WantsInternalNetworkInformerFactory interface
type wantInternalNetworkInformerFactory struct {
	sf informers.SharedInformerFactory
}

func (self *wantInternalNetworkInformerFactory) SetInternalNetworkInformerFactory(sf informers.SharedInformerFactory) {
	self.sf = sf
}
func (self *wantInternalNetworkInformerFactory) Admit(a admission.Attributes) error { return nil }
func (self *wantInternalNetworkInformerFactory) Handles(o admission.Operation) bool { return false }
func (self *wantInternalNetworkInformerFactory) ValidateInitialization() error      { return nil }

var _ admission.Interface = &wantInternalNetworkInformerFactory{}
var _ networkinitializer.WantsInternalNetworkInformerFactory = &wantInternalNetworkInformerFactory{}
