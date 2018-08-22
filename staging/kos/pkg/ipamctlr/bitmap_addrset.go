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

import (
	"fmt"
	"math/rand"
)

// BitmapAddressSet is an implementation of AddressSet.
// These are not safe for concurrent use.
type BitmapAddressSet struct {
	Count int
	Taken []bool
}

var _ AddressSet = &BitmapAddressSet{}

func NewBitmapAddressSet(size int) *BitmapAddressSet {
	return &BitmapAddressSet{0, make([]bool, size)}
}

func (as *BitmapAddressSet) NumTaken() int {
	return as.Count
}

func (as *BitmapAddressSet) Take(i int) bool {
	if as.Taken[i] {
		return false
	}
	as.Taken[i] = true
	as.Count++
	return true
}

// PickOne picks a free address and returns it,
// returns -1 if no address is available.
func (as *BitmapAddressSet) PickOne() int {
	if as.Count >= len(as.Taken) {
		return -1
	}
	i0 := rand.Intn(len(as.Taken))
	i := i0
	for {
		if !as.Taken[i] {
			as.Taken[i] = true
			as.Count++
			return i
		}
		i = (i + 1) % len(as.Taken)
		if i == i0 {
			fmt.Printf("Count was wrong in an BitmapAddressSet!\n")
			as.Count = len(as.Taken)
			return -1
		}
	}
}

func (as *BitmapAddressSet) Release(i int) bool {
	if as.Taken[i] {
		as.Taken[i] = false
		as.Count--
		return true
	}
	return false
}
