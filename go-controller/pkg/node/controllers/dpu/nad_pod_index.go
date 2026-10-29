// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
)

// nadPodIndex holds the pods that reference each NAD, by pod key. The zero
// value is ready to use.
type nadPodIndex struct {
	sync.Mutex
	nadToPods map[string]sets.Set[string]
	podToNADs map[string]sets.Set[string]
}

// set replaces the NADs recorded for podKey. The caller must not mutate nads
// afterwards.
func (i *nadPodIndex) set(podKey string, nads sets.Set[string]) {
	i.Lock()
	defer i.Unlock()

	for nad := range i.podToNADs[podKey] {
		if !nads.Has(nad) {
			i.untrack(nad, podKey)
		}
	}
	if nads.Len() == 0 {
		delete(i.podToNADs, podKey)
		return
	}

	if i.nadToPods == nil {
		i.nadToPods = map[string]sets.Set[string]{}
		i.podToNADs = map[string]sets.Set[string]{}
	}
	for nad := range nads {
		if i.nadToPods[nad] == nil {
			i.nadToPods[nad] = sets.New[string]()
		}
		i.nadToPods[nad].Insert(podKey)
	}
	i.podToNADs[podKey] = nads
}

// delete drops podKey from the index.
func (i *nadPodIndex) delete(podKey string) {
	i.Lock()
	defer i.Unlock()

	for nad := range i.podToNADs[podKey] {
		i.untrack(nad, podKey)
	}
	delete(i.podToNADs, podKey)
}

// pods returns the keys of the pods that reference nadName.
func (i *nadPodIndex) pods(nadName string) []string {
	i.Lock()
	defer i.Unlock()

	return i.nadToPods[nadName].UnsortedList()
}

// untrack removes podKey from the pods of nad, dropping the NAD once no pod
// references it. The lock must be held.
func (i *nadPodIndex) untrack(nad, podKey string) {
	pods := i.nadToPods[nad]
	pods.Delete(podKey)
	if pods.Len() == 0 {
		delete(i.nadToPods, nad)
	}
}
