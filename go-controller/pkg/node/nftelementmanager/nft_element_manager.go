// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package nftelementmanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
)

type element struct {
	elem   *knftables.Element
	delete bool
}

type Controller struct {
	mu        sync.Mutex
	elements  []element
	ownedMaps sets.Set[string]
}

func NewController() *Controller {
	return &Controller{
		elements:  make([]element, 0),
		ownedMaps: sets.New[string](),
	}
}

// Run periodically reconciles nftables elements, including owned-map cleanup
func (c *Controller) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	ticker := time.NewTicker(syncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			if err := c.fullReconcile(); err != nil {
				klog.Errorf("NFT element manager: failed to reconcile (retry in %s): %v", syncPeriod.String(), err)
			}
			c.mu.Unlock()
		}
	}
}

// Add ensures an nftables element is present and stays present across reconciliation cycles
func (c *Controller) Add(elem *knftables.Element) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, existing := range c.elements {
		if elementsEqual(existing.elem, elem) {
			c.elements[i].elem = elem
			c.elements[i].delete = false
			return c.reconcile()
		}
	}
	c.elements = append(c.elements, element{elem: elem})
	return c.reconcile()
}

// Delete removes an nftables element and ensures it stays removed
func (c *Controller) Delete(elem *knftables.Element) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, existing := range c.elements {
		if elementsEqual(existing.elem, elem) {
			c.elements[i].delete = true
			return c.reconcile()
		}
	}
	return nil
}

// OwnMaps declares ownership of the given maps, replaces the tracked
// elements for those maps, and runs a full reconcile to prune untracked
// elements from the owned maps. All elements must belong to one of the
// passed maps.
func (c *Controller) OwnMaps(ownedMaps []string, elems ...*knftables.Element) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	syncedMaps := sets.New(ownedMaps...)
	for _, elem := range elems {
		if !syncedMaps.Has(elem.Map) {
			return fmt.Errorf("element map %q is not in owned maps %v", elem.Map, ownedMaps)
		}
	}
	c.ownedMaps = c.ownedMaps.Union(syncedMaps)
	for i := range c.elements {
		if syncedMaps.Has(c.elements[i].elem.Map) {
			c.elements[i].delete = true
		}
	}
	for _, elem := range elems {
		found := false
		for i, existing := range c.elements {
			if elementsEqual(existing.elem, elem) {
				c.elements[i].elem = elem
				c.elements[i].delete = false
				found = true
				break
			}
		}
		if !found {
			c.elements = append(c.elements, element{elem: elem})
		}
	}
	return c.fullReconcile()
}

// reconcile applies tracked element adds/deletes to nftables
func (c *Controller) reconcile() error {
	return c.doReconcile(false)
}

// fullReconcile applies tracked elements and also cleans up unrecognized elements in owned maps
func (c *Controller) fullReconcile() error {
	return c.doReconcile(true)
}

func (c *Controller) doReconcile(pruneOwnedMaps bool) error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Reconciling NFT elements took %v", time.Since(start))
	}()

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()

	elementsToKeep := make([]element, 0, len(c.elements))
	for _, e := range c.elements {
		if e.delete {
			tx.Add(e.elem)
			tx.Delete(e.elem)
		} else {
			elementsToKeep = append(elementsToKeep, e)
			tx.Add(e.elem)
		}
	}

	if pruneOwnedMaps {
		for mapName := range c.ownedMaps {
			existing, err := nft.ListElements(context.TODO(), "map", mapName)
			if err != nil {
				if knftables.IsNotFound(err) {
					continue
				}
				return err
			}
			for _, existingElem := range existing {
				if !c.isTracked(existingElem) {
					klog.Infof("NFT element manager: deleting stale element in map %s: key=%v", mapName, existingElem.Key)
					tx.Add(existingElem)
					tx.Delete(existingElem)
				}
			}
		}
	}

	if err := nft.Run(context.TODO(), tx); err != nil {
		return err
	}
	c.elements = elementsToKeep
	return nil
}

func (c *Controller) isTracked(candidate *knftables.Element) bool {
	for _, e := range c.elements {
		if !e.delete && elementsEqual(e.elem, candidate) {
			return true
		}
	}
	return false
}

func elementsEqual(a, b *knftables.Element) bool {
	if a.Map != b.Map {
		return false
	}
	if len(a.Key) != len(b.Key) {
		return false
	}
	for i := range a.Key {
		if a.Key[i] != b.Key[i] {
			return false
		}
	}
	if len(a.Value) != len(b.Value) {
		return false
	}
	for i := range a.Value {
		if a.Value[i] != b.Value[i] {
			return false
		}
	}
	return true
}
