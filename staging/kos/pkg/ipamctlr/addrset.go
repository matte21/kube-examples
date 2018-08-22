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

package ipamctlr

// AddressSet is a set of addresses in an externally determined range.
// An address is represented as an integer offset from the start of the range.
type AddressSet interface {
	// Count returns the number of addresses in the set
	NumTaken() int
	
	// Take ensures the given address is in the set.
	// Result indicates whether this made a change.
	Take(int) bool

	// PickOne adds a member to the set and returns the new member or,
	// if the set is full, returns -1.
	PickOne() int

	// Release ensures the given address is not in the set.
	// Result indicates whether this made a change.
	Release(int) bool
}
