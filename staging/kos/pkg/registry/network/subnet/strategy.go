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

package subnet

import (
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"

	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/examples/staging/kos/pkg/apis/network"
	informers "k8s.io/examples/staging/kos/pkg/client/informers/internalversion"
	listers "k8s.io/examples/staging/kos/pkg/client/listers/network/internalversion"
)

// NewStrategy creates and returns a pointer to a subnetStrategy instance
func NewStrategy(typer runtime.ObjectTyper, listerFactory informers.SharedInformerFactory) *subnetStrategy {
	subnetLister := listerFactory.Network().InternalVersion().Subnets().Lister()
	return &subnetStrategy{typer,
		names.SimpleNameGenerator,
		subnetLister}
}

// GetAttrs returns labels.Set, fields.Set, the presence of Initializers if any
// and error in case the given runtime.Object is not a Subnet
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, bool, error) {
	subnet, ok := obj.(*network.Subnet)
	if !ok {
		return nil, nil, false, fmt.Errorf("given object is not a Subnet")
	}
	return labels.Set(subnet.ObjectMeta.Labels), SelectableFields(subnet), subnet.Initializers != nil, nil
}

// MatchSubnet is the filter used by the generic etcd backend to watch events
// from etcd to clients of the apiserver only interested in specific labels/fields.
func MatchSubnet(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: GetAttrs,
	}
}

// SelectableFields returns a field set that represents the object.
func SelectableFields(obj *network.Subnet) fields.Set {
	return generic.ObjectMetaFieldsSet(&obj.ObjectMeta, true)
}

type subnetStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	subnetLister listers.SubnetLister
}

var _ rest.RESTCreateStrategy = &subnetStrategy{}
var _ rest.RESTUpdateStrategy = &subnetStrategy{}
var _ rest.RESTDeleteStrategy = &subnetStrategy{}

func (*subnetStrategy) NamespaceScoped() bool {
	return true
}

func (*subnetStrategy) PrepareForCreate(ctx genericapirequest.Context, obj runtime.Object) {
	if subnet, isSubnet := obj.(*network.Subnet); isSubnet {
		subnet.Status = network.SubnetStatus{}
	}
}

func (*subnetStrategy) PrepareForUpdate(ctx genericapirequest.Context, obj, old runtime.Object) {
}

func (s *subnetStrategy) Validate(ctx genericapirequest.Context, obj runtime.Object) field.ErrorList {
	var validationErrors field.ErrorList

	thisSubnet, isSubnet := obj.(*network.Subnet)
	if !isSubnet {
		// If we're here it's impossible to continue validating the object because it's not a subnet, thus
		// we return immediately.
		// ? Consider adding a more informative error message?
		// ? Consider invoking panic() instead. This is a severe bug that should never happen.
		validationErrors = append(validationErrors, field.InternalError(nil, fmt.Errorf("server internal error, try again later")))
		return validationErrors
	}

	vniRangeErrors := s.checkVNIRange(thisSubnet)
	cidrFormatErrors := s.checkCIDRFormat(thisSubnet)

	if len(vniRangeErrors) > 0 && len(cidrFormatErrors) > 0 {
		validationErrors = append(validationErrors, vniRangeErrors...)
		validationErrors = append(validationErrors, cidrFormatErrors...)

		// If we are here, both the VNI range and the CIDR format of the created subnet are invalid.
		// All of the successive validation steps rely on at least one of these two values being valid,
		// thus we return immediately
		return validationErrors
	}

	// We need to check the under-validation subnet against subnets with the same VNI only. Other subnets can be ignored
	allSubnetsWithSameVNI, err := s.allSubnetsWithVNI(thisSubnet.Spec.VNI)
	if err != nil {
		// If we're here it was not possible to fetch the subnets with the same VNI as the one under creation,
		// thus validation cannot proceed and we return immediately
		// ? Consider adding a more informative error message?
		validationErrors = append(validationErrors, field.InternalError(nil, fmt.Errorf("server internal error, try again later")))
		return validationErrors
	}

	var sameVNISameNamespaceErrors field.ErrorList
	if len(vniRangeErrors) == 0 {
		sameVNISameNamespaceErrors = s.checkSameVNISameNamespace(thisSubnet, allSubnetsWithSameVNI)
	}

	var cidrDoesNotOverlapErrors field.ErrorList
	if len(cidrFormatErrors) == 0 {
		cidrDoesNotOverlapErrors = s.checkCIDRDoesNotOverlap(thisSubnet, allSubnetsWithSameVNI)
	}

	validationErrors = append(validationErrors, vniRangeErrors...)
	validationErrors = append(validationErrors, cidrFormatErrors...)
	validationErrors = append(validationErrors, sameVNISameNamespaceErrors...)
	validationErrors = append(validationErrors, cidrDoesNotOverlapErrors...)

	if len(validationErrors) > 0 {
		return validationErrors
	}

	return nil
}

func (*subnetStrategy) AllowCreateOnUpdate() bool {
	return false
}

func (*subnetStrategy) AllowUnconditionalUpdate() bool {
	return false
}

func (*subnetStrategy) Canonicalize(obj runtime.Object) {
}

func (s *subnetStrategy) ValidateUpdate(ctx genericapirequest.Context, obj, old runtime.Object) field.ErrorList {
	if _, isSubnet := old.(*network.Subnet); !isSubnet {
		// ? Consider adding a more informative error message?
		// ? Consider invoking panic() instead. This is a severe bug that should never happen.
		return field.ErrorList{field.InternalError(nil, fmt.Errorf("server internal error, try again later"))}
	}
	return s.Validate(ctx, obj)
}

// ! These two consts harm code reusability, because they're set to the OVS-specific values.
// ! OVS is the low-level networking implementation used for this project, but this code
// ! should not depend on OVS being used.
// TODO As a fix, make these consts fields of a subnetStrategy, and have them
// injected by the creator of that strategy, so that such creator can set them to the values
// associated with the low-level networking implementation used.
const (
	minVNI uint32 = 1
	maxVNI uint32 = 2097151
)

func (*subnetStrategy) checkVNIRange(subnet *network.Subnet) field.ErrorList {
	errors := field.ErrorList{}
	vni := subnet.Spec.VNI
	if vni < minVNI || vni > maxVNI {
		errorMsg := fmt.Sprintf("must be in the range [%d,%d]", minVNI, maxVNI)
		errors = append(errors, field.Invalid(field.NewPath("spec", "vni"), fmt.Sprintf("%d", vni), errorMsg))
	}
	return errors
}

func (*subnetStrategy) checkCIDRFormat(subnet *network.Subnet) field.ErrorList {
	errors := field.ErrorList{}
	cidr := subnet.Spec.IPv4
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		errors = append(errors, field.Invalid(field.NewPath("spec", "ipv4"), cidr, err.Error()))
	}
	return errors
}

func (s *subnetStrategy) checkSameVNISameNamespace(thisSubnet *network.Subnet, allSubnetsWithSameVNI []*network.Subnet) field.ErrorList {
	errors := field.ErrorList{}

	for _, aSubnet := range allSubnetsWithSameVNI {
		if aSubnet.Namespace != thisSubnet.Namespace {
			errorMsg := fmt.Sprintf("subnet %s/%s has the same VNI but different namespace with respect to subnet %s/%s. "+
				"Subnets with the same VNI MUST reside in the same namespace",
				thisSubnet.Namespace,
				thisSubnet.Name,
				aSubnet.Namespace,
				aSubnet.Name)
			errors = append(errors, field.Forbidden(field.NewPath("metadata", "namespace"), errorMsg))
		}
	}

	return errors
}

func (s *subnetStrategy) checkCIDRDoesNotOverlap(thisSubnet *network.Subnet, allSubnetsWithSameVNI []*network.Subnet) field.ErrorList {
	errors := field.ErrorList{}

	// No need to check if parse was successful, as the check has been already preformed in method checkCIDRFormat
	thisSubnetParsed := parseSubnet(thisSubnet)

	for _, aSubnet := range allSubnetsWithSameVNI {
		// No need to check if parse was successuful, as aSubnet has already been created. If it couldn't
		// be parsed its creation would have failed
		aSubnetParsed := parseSubnet(aSubnet)
		if thisSubnetParsed.overlaps(aSubnetParsed) {
			errorMsg := fmt.Sprintf("subnet %s/%s IPv4 range (%s) overlaps with that of subnet %s/%s (%s). IPv4 ranges "+
				"for subnets with the same VNI MUST be disjoint",
				thisSubnet.Namespace,
				thisSubnet.Name,
				thisSubnet.Spec.IPv4,
				aSubnet.Namespace,
				aSubnet.Name,
				aSubnet.Spec.IPv4)
			errors = append(errors, field.Forbidden(field.NewPath("spec", "ipv4"), errorMsg))
		}
	}

	return errors
}

func (s *subnetStrategy) allSubnetsWithVNI(vni uint32) ([]*network.Subnet, error) {
	allSubnets, err := s.subnetLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var allSubnetsWithSameVNI []*network.Subnet
	for _, aSubnet := range allSubnets {
		if aSubnet.Spec.VNI == vni {
			allSubnetsWithSameVNI = append(allSubnetsWithSameVNI, aSubnet)
		}
	}
	return allSubnetsWithSameVNI, nil
}

type parsedSubnet struct {
	baseU, lastU uint32
}

func parseSubnet(subnet *network.Subnet) (ps parsedSubnet) {
	_, ipNet, err := net.ParseCIDR(subnet.Spec.IPv4)
	if err != nil {
		return
	}
	ps.baseU = ipv4ToUint32(ipNet.IP)
	ones, bits := ipNet.Mask.Size()
	delta := uint32(uint64(1)<<uint(bits-ones) - 1)
	ps.lastU = ps.baseU + delta
	return
}

func ipv4ToUint32(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 + uint32(v4[1])<<16 + uint32(v4[2])<<8 + uint32(v4[3])
}

func (ps1 parsedSubnet) overlaps(ps2 parsedSubnet) bool {
	return ps1.baseU <= ps2.lastU && ps1.lastU >= ps2.baseU
}
