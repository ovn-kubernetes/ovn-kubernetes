// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package namespace

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NamespaceAnnotationCache stores parsed namespace annotation values keyed by
// namespace. Mirrors NodeAnnotationCache.
type NamespaceAnnotationCache struct {
	namespaces *syncmap.SyncMap[*nsAnnotationEntry]
}

// nsAnnotationEntry is the per-namespace cache entry. The raw string is kept so
// reparsing can be skipped when the annotation hasn't changed.
type nsAnnotationEntry struct {
	aclLoggingRaw    string
	aclLoggingParsed libovsdbutil.ACLLoggingLevels
	aclLoggingErr    error

	routingGWsRaw    string
	routingGWsParsed sets.Set[string]
	routingGWsErr    error
}

// NewNamespaceAnnotationCache creates an empty annotation parse cache.
func NewNamespaceAnnotationCache() *NamespaceAnnotationCache {
	return &NamespaceAnnotationCache{namespaces: syncmap.NewSyncMap[*nsAnnotationEntry]()}
}

// UpdateNamespaceAnnotationState builds a parse-once view of namespace
// annotations. When updateCache is true, parsed values refresh the cache;
// callers comparing an old snapshot against a newer one pass false for the old
// one so it cannot replace the latest cached value.
func (c *NamespaceAnnotationCache) UpdateNamespaceAnnotationState(ns *corev1.Namespace, updateCache bool) *NamespaceAnnotationState {
	if ns == nil {
		return nil
	}
	aclLogging, aclLoggingErr := c.parseACLLoggingCached(ns, updateCache)
	multicastEnabled := ns.Annotations[util.NsMulticastAnnotation] == "true"
	routingGWs, routingGWsErr := c.parseRoutingGWsCached(ns, updateCache)
	_, bfdEnabled := ns.Annotations[util.BfdAnnotation]
	return newNamespaceAnnotationState(ns.Name, aclLogging, aclLoggingErr, multicastEnabled, routingGWs, routingGWsErr, bfdEnabled)
}

// DeleteNamespace removes all cached annotation parse results for the namespace.
func (c *NamespaceAnnotationCache) DeleteNamespace(nsName string) {
	_ = c.namespaces.DoWithLock(nsName, func(key string) error {
		c.namespaces.Delete(key)
		return nil
	})
}

func (c *NamespaceAnnotationCache) parseACLLoggingCached(ns *corev1.Namespace, updateCache bool) (libovsdbutil.ACLLoggingLevels, error) {
	raw := ns.Annotations[util.AclLoggingAnnotation]
	if cached, ok := c.getACLLogging(ns.Name, raw); ok {
		return cached.parsed, cached.err
	}
	parsed, err := parseACLLogging(raw)
	if updateCache {
		c.setACLLogging(ns.Name, raw, parsed, err)
	}
	return parsed, err
}

func (c *NamespaceAnnotationCache) parseRoutingGWsCached(ns *corev1.Namespace, updateCache bool) (sets.Set[string], error) {
	raw := ns.Annotations[util.RoutingExternalGWsAnnotation]
	if cached, ok := c.getRoutingGWs(ns.Name, raw); ok {
		return cached.parsed, cached.err
	}
	parsed, err := util.ParseRoutingExternalGWAnnotation(raw)
	if updateCache {
		c.setRoutingGWs(ns.Name, raw, parsed, err)
	}
	return parsed, err
}

// parseACLLogging mirrors aclLoggingUpdateNsInfo's annotation handling but
// returns by value rather than mutating an external struct.
func parseACLLogging(annotation string) (libovsdbutil.ACLLoggingLevels, error) {
	if annotation == "" || annotation == "{}" {
		return libovsdbutil.ACLLoggingLevels{}, nil
	}
	var levels libovsdbutil.ACLLoggingLevels
	if err := json.Unmarshal([]byte(annotation), &levels); err != nil {
		return libovsdbutil.ACLLoggingLevels{}, fmt.Errorf("could not unmarshal namespace ACL annotation %q: %v", annotation, err)
	}
	return levels, nil
}

type aclLoggingCached struct {
	parsed libovsdbutil.ACLLoggingLevels
	err    error
}

type routingGWsCached struct {
	parsed sets.Set[string]
	err    error
}

func (c *NamespaceAnnotationCache) getACLLogging(nsName, raw string) (aclLoggingCached, bool) {
	var out aclLoggingCached
	var ok bool
	_ = c.namespaces.DoWithLock(nsName, func(key string) error {
		entry, loaded := c.namespaces.Load(key)
		if !loaded || entry == nil {
			return nil
		}
		if entry.aclLoggingRaw != raw {
			return nil
		}
		out = aclLoggingCached{parsed: entry.aclLoggingParsed, err: entry.aclLoggingErr}
		ok = true
		return nil
	})
	return out, ok
}

func (c *NamespaceAnnotationCache) setACLLogging(nsName, raw string, parsed libovsdbutil.ACLLoggingLevels, err error) {
	_ = c.namespaces.DoWithLock(nsName, func(key string) error {
		entry := c.ensureEntryLocked(key)
		entry.aclLoggingRaw = raw
		entry.aclLoggingParsed = parsed
		entry.aclLoggingErr = err
		return nil
	})
}

func (c *NamespaceAnnotationCache) getRoutingGWs(nsName, raw string) (routingGWsCached, bool) {
	var out routingGWsCached
	var ok bool
	_ = c.namespaces.DoWithLock(nsName, func(key string) error {
		entry, loaded := c.namespaces.Load(key)
		if !loaded || entry == nil {
			return nil
		}
		if entry.routingGWsRaw != raw {
			return nil
		}
		out = routingGWsCached{parsed: entry.routingGWsParsed, err: entry.routingGWsErr}
		ok = true
		return nil
	})
	return out, ok
}

func (c *NamespaceAnnotationCache) setRoutingGWs(nsName, raw string, parsed sets.Set[string], err error) {
	_ = c.namespaces.DoWithLock(nsName, func(key string) error {
		entry := c.ensureEntryLocked(key)
		entry.routingGWsRaw = raw
		entry.routingGWsParsed = parsed
		entry.routingGWsErr = err
		return nil
	})
}

// ensureEntryLocked returns the cache entry for nsName, creating one if
// needed. Caller must hold the per-namespace syncmap key lock.
func (c *NamespaceAnnotationCache) ensureEntryLocked(nsName string) *nsAnnotationEntry {
	entry, _ := c.namespaces.LoadOrStore(nsName, &nsAnnotationEntry{
		routingGWsParsed: sets.New[string](),
	})
	return entry
}
