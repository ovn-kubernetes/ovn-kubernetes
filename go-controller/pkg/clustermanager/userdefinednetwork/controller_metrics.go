// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
)

// cudnMetricKey holds the label values for a CUDN metric label combination.
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
	for _, entry := range cudnNADs {
		if !controllerutil.ContainsFinalizer(entry.cudn, template.FinalizerUserDefinedNetwork) {
			continue
		}
		c.trackCUDN(entry.cudn.Name, &entry.cudn.Spec.Network)
	}
	for key, names := range c.cudnMetricTracker {
		metrics.SetCUDNCount(key.role, key.topology, key.transport, float64(names.Len()))
	}
}

// cudnMetricCounted records that a CUDN with the given spec is now counted in
// the gauge metric. The caller must have already persisted the finalizer — this
// is what guarantees a matching cudnMetricUncounted call during deletion.
func (c *Controller) cudnMetricCounted(name string, spec *userdefinednetworkv1.NetworkSpec) {
	key, names := c.trackCUDN(name, spec)
	metrics.SetCUDNCount(key.role, key.topology, key.transport, float64(names.Len()))
}

// trackCUDN inserts a CUDN name into the metric tracker bucket for its label
// combination, initializing the set if needed. Returns the bucket key and set.
func (c *Controller) trackCUDN(name string, spec *userdefinednetworkv1.NetworkSpec) (cudnMetricKey, sets.Set[string]) {
	role, topology, transport := cudnMetricLabels(spec)
	key := cudnMetricKey{role, topology, transport}
	if c.cudnMetricTracker[key] == nil {
		c.cudnMetricTracker[key] = sets.New[string]()
	}
	c.cudnMetricTracker[key].Insert(name)
	return key, c.cudnMetricTracker[key]
}

// cudnMetricUncounted records that a CUDN is no longer counted in the gauge
// metric. The caller must have already removed the finalizer.
func (c *Controller) cudnMetricUncounted(name string, spec *userdefinednetworkv1.NetworkSpec) {
	role, topology, transport := cudnMetricLabels(spec)
	key := cudnMetricKey{role, topology, transport}
	names := c.cudnMetricTracker[key]
	if names == nil || !names.Has(name) {
		return
	}
	names.Delete(name)
	if names.Len() == 0 {
		delete(c.cudnMetricTracker, key)
		metrics.DeleteCUDNCount(key.role, key.topology, key.transport)
	} else {
		metrics.SetCUDNCount(key.role, key.topology, key.transport, float64(names.Len()))
	}
}

// cudnMetricLabels extracts the role, topology, and transport label values from
// a CUDN network spec for use in Prometheus metrics. When spec.Transport is
// empty (the user did not configure an explicit transport), the label is set to
// "Default" — meaning the standard OVN overlay (Geneve or VXLAN, depending on
// ovn-encap-type). This avoids assuming a specific encapsulation protocol.
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
		transport = "Default"
	}
	return
}
