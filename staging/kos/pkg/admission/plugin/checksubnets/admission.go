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

package checksubnets

import (
	"fmt"
	"io"
	gonet "net"
	"strings"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/admission"
	serveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/examples/staging/kos/pkg/admission/networkinitializer"
	"k8s.io/examples/staging/kos/pkg/apis/network"
	informers "k8s.io/examples/staging/kos/pkg/client/informers/internalversion"
	listers "k8s.io/examples/staging/kos/pkg/client/listers/network/internalversion"
)

// Register registers a plugin
func Register(options *serveroptions.AdmissionOptions) {
	options.RecommendedPluginOrder = append(options.RecommendedPluginOrder, "CheckSubnets")
	options.Plugins.Register("CheckSubnets", func(config io.Reader) (admission.Interface, error) {
		return New()
	})
	glog.V(1).Infof("RecommendedPluginOrder = %#+v\n", options.RecommendedPluginOrder)
	glog.V(1).Infof("Registered admission plugins = %#+v\n", options.Plugins.Registered())
}

type CheckSubnets struct {
	*admission.Handler
	lister listers.SubnetLister
}

var _ = networkinitializer.WantsInternalNetworkInformerFactory(&CheckSubnets{})

// Admit checks the invariants that apply among Subnet objects.
// Other kinds of objects get a pass.
func (d *CheckSubnets) Admit(a admission.Attributes) error {
	// we are only interested in Subnets
	if a.GetKind().GroupKind() != network.Kind("Subnet") {
		return nil
	}

	thisSubnet := a.GetObject().(*network.Subnet)
	thisPS := ParseSubnet(thisSubnet)

	allSubnets, err := d.lister.List(labels.Everything())
	if err != nil {
		return err
	}
	glog.V(5).Infof("allSubnets=%#+v\n", allSubnets)
	complaints := []string{}
	for _, aSubnet := range allSubnets {
		if aSubnet.Spec.VNI != thisSubnet.Spec.VNI {
			continue
		}
		glog.V(4).Infof("Checking subnet %s/%s against %s/%s\n", thisSubnet.Namespace, thisSubnet.Name, aSubnet.Namespace, aSubnet.Name)
		if aSubnet.Namespace != thisSubnet.Namespace {
			complaints = append(complaints, fmt.Sprintf("subnet %s/%s has the same VNI", aSubnet.Namespace, aSubnet.Name))
		}
		ps := ParseSubnet(aSubnet)
		if thisPS.Overlaps(ps) {
			complaints = append(complaints, fmt.Sprintf("subnet %s/%s has overlapping IPv4 range %s", aSubnet.Namespace, aSubnet.Name, aSubnet.Spec.IPv4))
		}
	}
	glog.V(3).Infof("Check of subnet %s/%s against %d produced %d complaints\n", thisSubnet.Namespace, thisSubnet.Name, len(allSubnets), len(complaints))
	if len(complaints) > 0 {
		return errors.NewForbidden(
			a.GetResource().GroupResource(),
			a.GetName(),
			fmt.Errorf(strings.Join(complaints, ", ")))
	}

	return nil
}

type ParsedSubnet struct {
	BaseU, LastU uint32
}

func ParseSubnet(subnet *network.Subnet) (ps ParsedSubnet) {
	_, ipNet, err := gonet.ParseCIDR(subnet.Spec.IPv4)
	if err != nil {
		return
	}
	ps.BaseU = IPv4ToUint32(ipNet.IP)
	ones, bits := ipNet.Mask.Size()
	delta := uint32(uint64(1)<<uint(bits-ones) - 1)
	ps.LastU = ps.BaseU + delta
	return
}

func IPv4ToUint32(ip gonet.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 + uint32(v4[1])<<16 + uint32(v4[2])<<8 + uint32(v4[3])
}

func (ps1 ParsedSubnet) Overlaps(ps2 ParsedSubnet) bool {
	return ps1.BaseU <= ps2.LastU && ps1.LastU >= ps2.BaseU
}

// SetInternalNetworkInformerFactory gets Lister from SharedInformerFactory.
// The lister knows how to list Subnets.
func (d *CheckSubnets) SetInternalNetworkInformerFactory(f informers.SharedInformerFactory) {
	d.lister = f.Network().InternalVersion().Subnets().Lister()
	glog.V(1).Infoln("Subnets lister created")
}

// ValidaValidateInitializationte checks whether the plugin was correctly initialized.
func (d *CheckSubnets) ValidateInitialization() error {
	if d.lister == nil {
		return fmt.Errorf("missing subnet lister")
	}
	return nil
}

// New creates a new subnet checking admission plugin
func New() (*CheckSubnets, error) {
	return &CheckSubnets{
		Handler: admission.NewHandler(admission.Create),
	}, nil
}
