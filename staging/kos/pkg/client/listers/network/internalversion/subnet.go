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

// Code generated by lister-gen. DO NOT EDIT.

package internalversion

import (
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	network "k8s.io/examples/staging/kos/pkg/apis/network"
)

// SubnetLister helps list Subnets.
type SubnetLister interface {
	// List lists all Subnets in the indexer.
	List(selector labels.Selector) (ret []*network.Subnet, err error)
	// Subnets returns an object that can list and get Subnets.
	Subnets(namespace string) SubnetNamespaceLister
	SubnetListerExpansion
}

// subnetLister implements the SubnetLister interface.
type subnetLister struct {
	indexer cache.Indexer
}

// NewSubnetLister returns a new SubnetLister.
func NewSubnetLister(indexer cache.Indexer) SubnetLister {
	return &subnetLister{indexer: indexer}
}

// List lists all Subnets in the indexer.
func (s *subnetLister) List(selector labels.Selector) (ret []*network.Subnet, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*network.Subnet))
	})
	return ret, err
}

// Subnets returns an object that can list and get Subnets.
func (s *subnetLister) Subnets(namespace string) SubnetNamespaceLister {
	return subnetNamespaceLister{indexer: s.indexer, namespace: namespace}
}

// SubnetNamespaceLister helps list and get Subnets.
type SubnetNamespaceLister interface {
	// List lists all Subnets in the indexer for a given namespace.
	List(selector labels.Selector) (ret []*network.Subnet, err error)
	// Get retrieves the Subnet from the indexer for a given namespace and name.
	Get(name string) (*network.Subnet, error)
	SubnetNamespaceListerExpansion
}

// subnetNamespaceLister implements the SubnetNamespaceLister
// interface.
type subnetNamespaceLister struct {
	indexer   cache.Indexer
	namespace string
}

// List lists all Subnets in the indexer for a given namespace.
func (s subnetNamespaceLister) List(selector labels.Selector) (ret []*network.Subnet, err error) {
	err = cache.ListAllByNamespace(s.indexer, s.namespace, selector, func(m interface{}) {
		ret = append(ret, m.(*network.Subnet))
	})
	return ret, err
}

// Get retrieves the Subnet from the indexer for a given namespace and name.
func (s subnetNamespaceLister) Get(name string) (*network.Subnet, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(network.Resource("subnet"), name)
	}
	return obj.(*network.Subnet), nil
}