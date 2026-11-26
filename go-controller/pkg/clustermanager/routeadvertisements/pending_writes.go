// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"
	"k8s.io/client-go/tools/cache"
)

// A successful write can reach the API before our informer. If an EgressIP
// moves in that interval, its RA must see the old owner's configuration
// before it can withdraw the route and publish the replacement.
type pendingWrites struct {
	sync.Mutex
	store cache.Store
	byKey map[string]pendingWrite
	// Only a reconcile that already found the cache behind needs waking.
	held map[string]bool
}

type pendingWrite struct {
	ra      string
	version string
	// Delete returns no resource version. A different UID at the same name
	// proves the old object is gone even if the watch skipped the deletion.
	deletedUID ktypes.UID
}

func newPendingWrites(store cache.Store) *pendingWrites {
	return &pendingWrites{store: store, byKey: map[string]pendingWrite{}, held: map[string]bool{}}
}

func reached(cached, written string) bool {
	if cached == written {
		return true
	}
	order, err := resourceversion.CompareResourceVersion(cached, written)
	return err == nil && order >= 0
}

func (p *pendingWrites) seen(key string, write pendingWrite) bool {
	obj, found, err := p.store.GetByKey(key)
	if err != nil {
		return false
	}
	if found {
		cached := obj.(metav1.Object)
		if write.deletedUID != "" {
			return cached.GetUID() != write.deletedUID || !cached.GetDeletionTimestamp().IsZero()
		}
		return reached(cached.GetResourceVersion(), write.version)
	}
	if write.deletedUID != "" {
		return true
	}
	// A relist can miss both our create and a subsequent deletion. The
	// store's processed version then confirms that absence is up to date.
	return reached(p.store.LastStoreSyncResourceVersion(), write.version)
}

func (p *pendingWrites) record(key string, write pendingWrite) {
	if write.version == "" && write.deletedUID == "" {
		return
	}
	p.Lock()
	defer p.Unlock()
	// The watch may have delivered the event before the API call returned.
	if p.seen(key, write) {
		delete(p.byKey, key)
		return
	}
	p.byKey[key] = write
}

func (p *pendingWrites) written(ra string, obj metav1.Object) {
	p.record(cache.MetaObjectToName(obj).String(), pendingWrite{ra: ra, version: obj.GetResourceVersion()})
}

func (p *pendingWrites) deleted(ra string, obj metav1.Object) {
	p.record(cache.MetaObjectToName(obj).String(), pendingWrite{ra: ra, deletedUID: obj.GetUID()})
}

// A later version releases the wait too, but may contain someone else's edit.
// Only the exact version returned by our write is eligible for suppression.
func (p *pendingWrites) observed(obj metav1.Object) (ra string, held, own bool) {
	p.Lock()
	defer p.Unlock()
	key := cache.MetaObjectToName(obj).String()
	write, ok := p.byKey[key]
	if !ok || write.deletedUID != "" || !reached(obj.GetResourceVersion(), write.version) {
		return "", false, false
	}
	delete(p.byKey, key)
	return write.ra, p.held[write.ra], write.version == obj.GetResourceVersion()
}

func (p *pendingWrites) deletionObserved(key string) (ra string, held, own bool) {
	p.Lock()
	defer p.Unlock()
	write, ok := p.byKey[key]
	if !ok {
		return "", false, false
	}
	// The queued key can outlive this event. Recheck the store under the
	// same lock as record before clearing an expectation for a newer create.
	if !p.seen(key, write) {
		return "", false, false
	}
	delete(p.byKey, key)
	return write.ra, p.held[write.ra], write.deletedUID != ""
}

func (p *pendingWrites) waiting(ra string) bool {
	p.Lock()
	defer p.Unlock()
	delete(p.held, ra)
	for key, write := range p.byKey {
		if write.ra != ra {
			continue
		}
		if p.seen(key, write) {
			delete(p.byKey, key)
		} else {
			p.held[ra] = true
		}
	}
	return p.held[ra]
}
