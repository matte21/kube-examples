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
)

// NewStrategy creates and returns a subnetStrategy instance
func NewStrategy(typer runtime.ObjectTyper) subnetStrategy {
	return subnetStrategy{typer, names.SimpleNameGenerator}
}

// GetAttrs returns labels.Set, fields.Set, the presence of Initializers if any
// and error in case the given runtime.Object is not a Subnet
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, bool, error) {
	apiserver, ok := obj.(*network.Subnet)
	if !ok {
		return nil, nil, false, fmt.Errorf("given object is not a Subnet")
	}
	return labels.Set(apiserver.ObjectMeta.Labels), SelectableFields(apiserver), apiserver.Initializers != nil, nil
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
}

var _ rest.RESTCreateStrategy = subnetStrategy{}
var _ rest.RESTUpdateStrategy = subnetStrategy{}
var _ rest.RESTDeleteStrategy = subnetStrategy{}

func (subnetStrategy) NamespaceScoped() bool {
	return true
}

func (subnetStrategy) PrepareForCreate(ctx genericapirequest.Context, obj runtime.Object) {
	subnet := obj.(*network.Subnet)
	subnet.Status = network.SubnetStatus{}
}

func (subnetStrategy) PrepareForUpdate(ctx genericapirequest.Context, obj, old runtime.Object) {
}

func (subnetStrategy) Validate(ctx genericapirequest.Context, obj runtime.Object) field.ErrorList {
	subnet := obj.(*network.Subnet)
	errs := field.ErrorList{}
	if subnet.Spec.VNI < 1 || subnet.Spec.VNI > 2097151 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "vni"), subnet.Spec.VNI, "must be in the range [1,2097151]"))
	}
	_, _, err := net.ParseCIDR(subnet.Spec.IPv4)
	if err != nil {
		errs = append(errs, field.Invalid(field.NewPath("spec", "ipv4"), subnet.Spec.IPv4, fmt.Sprintf("CIDR parse error: %s", err.Error())))
	}
	return errs
}

func (subnetStrategy) AllowCreateOnUpdate() bool {
	return false
}

func (subnetStrategy) AllowUnconditionalUpdate() bool {
	return false
}

func (subnetStrategy) Canonicalize(obj runtime.Object) {
}

func (subnetStrategy) ValidateUpdate(ctx genericapirequest.Context, obj, old runtime.Object) field.ErrorList {
	return field.ErrorList{}
}
