// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
)

// cudnMetricKey holds the label values for a single CUDN in the count gauge.
type cudnMetricKey struct {
	role, topology, transport string
}

// seedCUDNCountMetrics populates the metric tracker from existing CUDNs at
// startup, then sets the gauge once.
//
// Only finalized CUDNs are seeded. This preserves the tracker invariant: every
// entry in cudnMetricTracker has a finalizer that guarantees cudnMetricUncounted
// will run during deletion. Unfinalized CUDNs are safe to skip — they will be
// counted by cudnMetricCounted when the reconcile loop adds the finalizer.
//
// Without this guard a tracker leak is possible: if an unfinalized CUDN is
// deleted before the controller reconciles it, Kubernetes garbage-collects it
// immediately (no finalizer), reconcileCUDN sees NotFound, syncClusterUDN(nil)
// short-circuits, and cudnMetricUncounted is never called — leaving a phantom
// entry in the tracker.
func (c *Controller) seedCUDNCountMetrics(cudnNADs cudnToNADs) {
	counts := map[cudnMetricKey]float64{}
	for _, entry := range cudnNADs {
		if !controllerutil.ContainsFinalizer(entry.cudn, template.FinalizerUserDefinedNetwork) {
			continue
		}
		role, topology, transport := cudnMetricLabels(&entry.cudn.Spec.Network)
		key := cudnMetricKey{role, topology, transport}
		c.cudnMetricTracker[entry.cudn.Name] = key
		counts[key]++
	}
	for k, count := range counts {
		metrics.SetCUDNCount(k.role, k.topology, k.transport, count)
	}
}

// cudnMetricCounted records that a CUDN with the given spec is now counted in
// the gauge metric. The caller must have already persisted the finalizer — this
// is what guarantees a matching cudnMetricUncounted call during deletion.
func (c *Controller) cudnMetricCounted(name string, spec *userdefinednetworkv1.NetworkSpec) {
	role, topology, transport := cudnMetricLabels(spec)
	key := cudnMetricKey{role, topology, transport}
	c.cudnMetricTracker[name] = key
	metrics.SetCUDNCount(key.role, key.topology, key.transport, c.countCUDNMetricKey(key))
}

// cudnMetricUncounted records that a CUDN is no longer counted in the gauge
// metric. The caller must have already removed the finalizer.
func (c *Controller) cudnMetricUncounted(name string) {
	key, existed := c.cudnMetricTracker[name]
	if !existed {
		return
	}
	delete(c.cudnMetricTracker, name)
	remaining := c.countCUDNMetricKey(key)
	if remaining == 0 {
		metrics.DeleteCUDNCount(key.role, key.topology, key.transport)
	} else {
		metrics.SetCUDNCount(key.role, key.topology, key.transport, remaining)
	}
}

// countCUDNMetricKey counts how many CUDNs in the tracker share the given label combination.
func (c *Controller) countCUDNMetricKey(target cudnMetricKey) float64 {
	var count float64
	for _, k := range c.cudnMetricTracker {
		if k == target {
			count++
		}
	}
	return count
}

// cudnMetricLabels extracts the role, topology, and transport label values from
// a CUDN network spec for use in Prometheus metrics. An empty transport (the
// default OVN overlay) is mapped to "Geneve" for overlay topologies. Localnet
// topology uses direct provider-network attachment (no overlay encapsulation),
// so its transport is labeled "Localnet".
func cudnMetricLabels(spec *userdefinednetworkv1.NetworkSpec) (role, topology, transport string) {
	if spec.Layer2 != nil {
		role = string(spec.Layer2.Role)
	} else if spec.Layer3 != nil {
		role = string(spec.Layer3.Role)
	} else if spec.Localnet != nil {
		role = string(spec.Localnet.Role)
	}
	topology = string(spec.Topology)
	transport = string(spec.Transport)
	if transport == "" {
		if spec.Topology == userdefinednetworkv1.NetworkTopologyLocalnet {
			transport = "Localnet"
		} else {
			transport = "Geneve"
		}
	}
	return
}
