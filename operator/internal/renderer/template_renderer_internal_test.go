// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package renderer

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
)

// testEmbeddedConfigWithIdentity returns an embedded config that includes
// ovnkubeIdentity as an optional component under the default mode.
func testEmbeddedConfigWithIdentity() *defaults.EmbeddedConfig {
	return &defaults.EmbeddedConfig{
		Version:           "1.5.0",
		MinUpgradeVersion: "1.0.0",
		Modes: map[string]defaults.EmbeddedMode{
			k8sv1alpha1.DeploymentModeStandard: {
				Components:         []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode},
				OptionalComponents: []string{k8sv1alpha1.ComponentOvnkubeIdentity},
			},
		},
		Components: []defaults.EmbeddedComponent{
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
				ResourceType: "Deployment",
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: "default", Repository: "example.io/ovnkube", Tag: "v1.0", SHA256: "abc123"},
				},
				ContainerImageMapping: map[string][]string{
					"default": {"ovnkubeClusterManager"},
				},
			},
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeNode,
				ResourceType: "DaemonSet",
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: "default", Repository: "example.io/ovnkube", Tag: "v1.0", SHA256: "abc123"},
				},
				ContainerImageMapping: map[string][]string{
					"default": {"ovnkubeController"},
				},
			},
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity,
				ResourceType: "DaemonSet",
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: "default", Repository: "example.io/ovnkube-identity", Tag: "v1.0", SHA256: "def456"},
				},
				ContainerImageMapping: map[string][]string{
					"default": {"ovnkubeIdentity"},
				},
			},
		},
	}
}

// testEmbeddedConfigWithoutIdentity returns an embedded config that does NOT
// include ovnkubeIdentity — identity is absent from the mode entirely.
func testEmbeddedConfigWithoutIdentity() *defaults.EmbeddedConfig {
	return &defaults.EmbeddedConfig{
		Version:           "1.5.0",
		MinUpgradeVersion: "1.0.0",
		Modes: map[string]defaults.EmbeddedMode{
			k8sv1alpha1.DeploymentModeStandard: {
				Components: []string{k8sv1alpha1.ComponentOvnkubeControlPlane, k8sv1alpha1.ComponentOvnkubeNode},
				// OptionalComponents intentionally omits ovnkubeIdentity
			},
		},
		Components: []defaults.EmbeddedComponent{
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeControlPlane,
				ResourceType: "Deployment",
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: "default", Repository: "example.io/ovnkube", Tag: "v1.0", SHA256: "abc123"},
				},
				ContainerImageMapping: map[string][]string{
					"default": {"ovnkubeClusterManager"},
				},
			},
			{
				ResourceName: k8sv1alpha1.ComponentOvnkubeNode,
				ResourceType: "DaemonSet",
				ContainerImages: []defaults.EmbeddedContainerImage{
					{Name: "default", Repository: "example.io/ovnkube", Tag: "v1.0", SHA256: "abc123"},
				},
				ContainerImageMapping: map[string][]string{
					"default": {"ovnkubeController"},
				},
			},
		},
	}
}

func TestBuildTemplateData_DoesNotInjectWebhookCABundleWhenIdentityDisabled(t *testing.T) {
	r := NewTemplateRenderer("", testEmbeddedConfigWithoutIdentity())
	cr := &k8sv1alpha1.OVNK8sOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator"},
		Spec: k8sv1alpha1.OVNK8sOperatorSpec{
			Global: k8sv1alpha1.GlobalConfig{
				K8sAPIServer:   "https://10.0.0.1:6443",
				PodNetwork:     []k8sv1alpha1.PodNetworkEntry{{CIDR: "10.192.0.0/16", HostPrefix: 26}},
				ServiceNetwork: []string{"10.96.0.0/12"},
				MTU:            1440,
				DeploymentMode: k8sv1alpha1.DeploymentModeStandard,
				Namespace:      "ovn-kubernetes",
			},
			// No ComponentOverrides — identity is not in the mode at all
		},
	}

	// Pass a non-empty WebhookCABundle; it must be silently discarded because
	// the identity component is not in the resolved set.
	data, err := r.buildTemplateData(cr, RenderOptions{WebhookCABundle: "PEM-CA-DATA"})
	if err != nil {
		t.Fatalf("buildTemplateData: %v", err)
	}
	if _, ok := data["ovnkubeIdentity"].(map[string]any); ok {
		t.Fatal("ovnkubeIdentity key must be absent when identity is not resolved, but it was present")
	}
}

func TestBuildTemplateData_InjectsWebhookCABundleWhenIdentityEnabled(t *testing.T) {
	tru := true
	r := NewTemplateRenderer("", testEmbeddedConfigWithIdentity())
	cr := &k8sv1alpha1.OVNK8sOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator"},
		Spec: k8sv1alpha1.OVNK8sOperatorSpec{
			Global: k8sv1alpha1.GlobalConfig{
				K8sAPIServer:   "https://10.0.0.1:6443",
				PodNetwork:     []k8sv1alpha1.PodNetworkEntry{{CIDR: "10.192.0.0/16", HostPrefix: 26}},
				ServiceNetwork: []string{"10.96.0.0/12"},
				MTU:            1440,
				DeploymentMode: k8sv1alpha1.DeploymentModeStandard,
				Namespace:      "ovn-kubernetes",
			},
			ComponentOverrides: []k8sv1alpha1.ComponentOverride{
				{ResourceName: k8sv1alpha1.ComponentOvnkubeIdentity, Enabled: &tru},
			},
		},
	}

	data, err := r.buildTemplateData(cr, RenderOptions{WebhookCABundle: "PEM-CA-DATA"})
	if err != nil {
		t.Fatalf("buildTemplateData: %v", err)
	}
	comp, ok := data["ovnkubeIdentity"].(map[string]any)
	if !ok {
		t.Fatalf("ovnkubeIdentity component data missing")
	}
	if comp["webhookCABundle"] != "PEM-CA-DATA" {
		t.Fatalf("webhookCABundle = %v, want PEM-CA-DATA", comp["webhookCABundle"])
	}
}

// TestBuildFeatureGateDataFullyPopulated is the canonical registry of dotted
// gate names: it fully populates FeatureGates and asserts the exact gate list
// and config map. A new field in FeatureGates (or a gateContainers change)
// must update this test.
func TestBuildFeatureGateDataFullyPopulated(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	b := func(v bool) *bool { return &v }
	u32 := func(v uint32) *uint32 { return &v }

	fg := &k8sv1alpha1.FeatureGates{
		UserDefinedNetworks: &k8sv1alpha1.UserDefinedNetworksConfig{
			PreconfiguredAddresses:  &k8sv1alpha1.EmptyConfig{},
			AdvertisedIsolationMode: "loose",
			NoOverlay: &k8sv1alpha1.NoOverlayConfig{
				Snat: b(true),
				ManagedRouting: &k8sv1alpha1.NoOverlayManagedRoutingConfig{
					ASNumber:     u32(65001),
					Topology:     "full-mesh",
					FrrNamespace: "frr-system",
				},
			},
			DynamicUDNAllocation: &k8sv1alpha1.DynamicUDNAllocationConfig{DeletionGracePeriod: i32(60)},
			NetworkConnect:       &k8sv1alpha1.EmptyConfig{},
		},
		Routing: &k8sv1alpha1.RoutingConfig{
			RouteAdvertisements: &k8sv1alpha1.RouteAdvertisementsConfig{
				AdvertiseDefaultNetwork: b(true),
			},
			EVPN: &k8sv1alpha1.EmptyConfig{},
		},
		Egress: &k8sv1alpha1.EgressConfig{
			IP:       &k8sv1alpha1.EgressIPConfig{HealthCheckPort: i32(9107)},
			Firewall: &k8sv1alpha1.EmptyConfig{},
			Qos:      &k8sv1alpha1.EmptyConfig{},
			Service:  &k8sv1alpha1.EmptyConfig{},
		},
		NetworkPolicy: &k8sv1alpha1.NetworkPolicyConfig{
			Admin: &k8sv1alpha1.EmptyConfig{},
			ICMP:  &k8sv1alpha1.EmptyConfig{},
		},
		Interconnect:           &k8sv1alpha1.InterconnectConfig{V4TransitSwitchSubnet: "100.88.0.0/16"},
		HybridOverlay:          &k8sv1alpha1.EmptyConfig{},
		MultiExternalGateway:   &k8sv1alpha1.EmptyConfig{},
		PersistentIPs:          &k8sv1alpha1.EmptyConfig{},
		DNSNameResolver:        &k8sv1alpha1.EmptyConfig{},
		NetworkQoS:             &k8sv1alpha1.EmptyConfig{},
		LFlowCache:             &k8sv1alpha1.LFlowCacheConfig{CacheLimit: i32(1000)},
		Multicast:              &k8sv1alpha1.EmptyConfig{},
		ServiceTemplateSupport: &k8sv1alpha1.EmptyConfig{},
		Uplink:                 &k8sv1alpha1.EmptyConfig{},
	}

	gates, config, err := buildFeatureGateData(fg)
	if err != nil {
		t.Fatal(err)
	}

	wantGates := []string{
		"dnsNameResolver",
		"egress",
		"egress.firewall",
		"egress.ip",
		"egress.qos",
		"egress.service",
		"hybridOverlay",
		"interconnect",
		"lflowCache",
		"multiExternalGateway",
		"multicast",
		"networkPolicy",
		"networkPolicy.admin",
		"networkPolicy.icmp",
		"networkQoS",
		"persistentIPs",
		"routing",
		"routing.evpn",
		"routing.routeAdvertisements",
		"serviceTemplateSupport",
		"uplink",
		"userDefinedNetworks",
		"userDefinedNetworks.dynamicUDNAllocation",
		"userDefinedNetworks.networkConnect",
		"userDefinedNetworks.noOverlay",
		"userDefinedNetworks.noOverlay.managedRouting",
		"userDefinedNetworks.preconfiguredAddresses",
	}
	if len(gates) != len(wantGates) {
		t.Fatalf("gate list mismatch:\n got  %v\n want %v", gates, wantGates)
	}
	for i := range wantGates {
		if gates[i] != wantGates[i] {
			t.Fatalf("gate list mismatch at %d:\n got  %v\n want %v", i, gates, wantGates)
		}
	}

	assertConfig := func(gate, field string, want any) {
		t.Helper()
		gc, ok := config[gate].(map[string]any)
		if !ok {
			t.Fatalf("no config for gate %q", gate)
		}
		if got := gc[field]; got != want {
			t.Errorf("config[%q][%q] = %v, want %v", gate, field, got, want)
		}
	}
	assertConfig("interconnect", "v4TransitSwitchSubnet", "100.88.0.0/16")
	assertConfig("egress.ip", "healthCheckPort", float64(9107))
	assertConfig("lflowCache", "cacheLimit", float64(1000))
	assertConfig("userDefinedNetworks", "advertisedIsolationMode", "loose")
	assertConfig("userDefinedNetworks.noOverlay", "snat", true)
	assertConfig("userDefinedNetworks.noOverlay.managedRouting", "asNumber", float64(65001))
	assertConfig("userDefinedNetworks.noOverlay.managedRouting", "frrNamespace", "frr-system")
	assertConfig("routing.routeAdvertisements", "advertiseDefaultNetwork", true)

	// Group containers with no scalar fields of their own must not have config.
	for _, gate := range []string{"routing", "egress", "networkPolicy"} {
		if _, ok := config[gate]; ok {
			t.Errorf("unexpected config for group container %q: %v", gate, config[gate])
		}
	}

	// Nil feature gates produce empty results.
	gates, config, err = buildFeatureGateData(nil)
	if err != nil || len(gates) != 0 || len(config) != 0 {
		t.Errorf("nil FeatureGates: got gates=%v config=%v err=%v", gates, config, err)
	}
}
