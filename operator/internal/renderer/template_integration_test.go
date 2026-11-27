// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// These tests render the shipped templates through the same renderer the
// controller uses and check that each CR setting the templates read lands in
// the right field of the rendered objects: the namespace, network and global
// settings, every feature gate and its options, the identity webhook and its
// CA bundle, and each component's node placement, image, custom variable and
// replica overrides. A second CR with no optional settings checks that the
// feature gates and the identity webhook stay off. Expected values come from
// the test inputs, so the tests change only when the CR-to-manifest mapping
// does.

package renderer_test

import (
	"fmt"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigyaml "sigs.k8s.io/yaml"

	k8sv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/operator/api/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/defaults"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/kinds"
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/renderer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// maximalCR sets every global setting and feature gate option to a non-default
// value, and enables the identity webhook.
const maximalCR = `
apiVersion: k8s.ovn.org/v1alpha1
kind: OVNK8sOperator
metadata:
  name: default
  namespace: ovnk8s-operator-system
spec:
  global:
    k8sAPIServer: https://10.0.0.10:6443
    podNetwork:
      - cidr: 10.128.0.0/14
        hostPrefix: 23
      - cidr: fd01::/48
        hostPrefix: 64
    serviceNetwork:
      - 172.30.0.0/16
      - fd02::/112
    mtu: 1400
    deploymentMode: standard
    gatewayMode: local
    namespace: ovnk-test
    v4JoinSubnet: 100.65.0.0/16
    v6JoinSubnet: fd99::/64
    v4MasqueradeSubnet: 169.254.64.0/18
    v6MasqueradeSubnet: fd70::/112
    emptyLbEvents: true
    enableCoredump: true
    metrics:
      ip: 10.0.0.1
    routingTableIDStart: 1002
    imagePullSecrets:
      - name: pull-secret
  featureGates:
    userDefinedNetworks:
      preconfiguredAddresses: {}
      advertisedIsolationMode: loose
      noOverlay:
        snat: true
        managedRouting:
          asNumber: 65001
          topology: full-mesh
          frrNamespace: frr-system
      dynamicUDNAllocation:
        deletionGracePeriod: 60
      networkConnect: {}
    routing:
      routeAdvertisements:
        advertiseDefaultNetwork: true
      evpn: {}
    egress:
      ip:
        healthCheckPort: 9200
      firewall: {}
      qos: {}
      service: {}
    networkPolicy:
      admin: {}
      icmp: {}
    interconnect:
      v4TransitSwitchSubnet: 100.89.0.0/16
      v6TransitSwitchSubnet: fd96::/64
    hybridOverlay: {}
    multiExternalGateway: {}
    uplink: {}
    persistentIPs: {}
    dnsNameResolver: {}
    networkQoS: {}
    lflowCache:
      cacheLimit: 1000
      cacheLimitKb: 2000
    multicast: {}
    serviceTemplateSupport: {}
  componentOverrides:
    - resourceName: ovnkubeIdentity
      enabled: true
`

// minimalCR sets only the required settings.
const minimalCR = `
apiVersion: k8s.ovn.org/v1alpha1
kind: OVNK8sOperator
metadata:
  name: default
  namespace: ovnk8s-operator-system
spec:
  global:
    k8sAPIServer: https://10.0.0.10:6443
    podNetwork:
      - cidr: 10.128.0.0/14
        hostPrefix: 23
    serviceNetwork:
      - 172.30.0.0/16
    deploymentMode: standard
    namespace: ovnk-test
`

const (
	testNamespace        = "ovnk-test"
	controlPlaneWorkload = "ovnkube-control-plane"
	nodeWorkload         = "ovnkube-node"
	identityWorkload     = "ovnkube-identity"
	ovnkubeLogLevelEnv   = "OVNKUBE_LOGLEVEL"
	enabledValue         = "true"
	testNodeLabel        = "example.com/ovn"
	testCABundle         = "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"
)

var (
	// gateEnv maps each env var a feature gate turns on to its gate.
	gateEnv = map[string]string{
		"OVN_HYBRID_OVERLAY_ENABLE":         "hybridOverlay",
		"OVN_ADMIN_NETWORK_POLICY_ENABLE":   "networkPolicy.admin",
		"OVN_ALLOW_ICMP_NETPOL":             "networkPolicy.icmp",
		"OVN_EGRESSIP_ENABLE":               "egress.ip",
		"OVN_EGRESSSERVICE_ENABLE":          "egress.service",
		"OVN_EGRESSFIREWALL_ENABLE":         "egress.firewall",
		"OVN_EGRESSQOS_ENABLE":              "egress.qos",
		"OVN_MULTI_NETWORK_ENABLE":          "userDefinedNetworks",
		"OVN_NETWORK_SEGMENTATION_ENABLE":   "userDefinedNetworks",
		"OVN_NETWORK_CONNECT_ENABLE":        "userDefinedNetworks.networkConnect",
		"OVN_PRE_CONF_UDN_ADDR_ENABLE":      "userDefinedNetworks.preconfiguredAddresses",
		"OVN_DYNAMIC_UDN_ALLOCATION":        "userDefinedNetworks.dynamicUDNAllocation",
		"OVN_NO_OVERLAY_ENABLE":             "userDefinedNetworks.noOverlay",
		"OVN_ROUTE_ADVERTISEMENTS_ENABLE":   "routing.routeAdvertisements",
		"OVN_EVPN_ENABLE":                   "routing.evpn",
		"OVN_UPLINK_ENABLE":                 "uplink",
		"OVN_MULTICAST_ENABLE":              "multicast",
		"OVN_ENABLE_MULTI_EXTERNAL_GATEWAY": "multiExternalGateway",
		"OVN_NETWORK_QOS_ENABLE":            "networkQoS",
		"OVN_ENABLE_PERSISTENT_IPS":         "persistentIPs",
		"OVN_ENABLE_DNSNAMERESOLVER":        "dnsNameResolver",
		"OVN_ENABLE_LFLOW_CACHE":            "lflowCache",
		"OVN_ENABLE_SVC_TEMPLATE_SUPPORT":   "serviceTemplateSupport",
	}

	// settingEnv maps env vars to the value maximalCR sets for them.
	settingEnv = map[string]string{
		"OVN_GATEWAY_MODE":                  "local",
		"OVN_V4_JOIN_SUBNET":                "100.65.0.0/16",
		"OVN_V6_JOIN_SUBNET":                "fd99::/64",
		"OVN_V4_MASQUERADE_SUBNET":          "169.254.64.0/18",
		"OVN_V6_MASQUERADE_SUBNET":          "fd70::/112",
		"OVN_EMPTY_LB_EVENTS":               enabledValue,
		"GOTRACEBACK":                       "crash",
		"METRICS_IP":                        "10.0.0.1",
		"OVN_V4_TRANSIT_SUBNET":             "100.89.0.0/16",
		"OVN_V6_TRANSIT_SUBNET":             "fd96::/64",
		"OVN_EGRESSIP_HEALTHCHECK_PORT":     "9200",
		"OVN_LFLOW_CACHE_LIMIT":             "1000",
		"OVN_LFLOW_CACHE_LIMIT_KB":          "2000",
		"OVN_DYNAMIC_UDN_GRACE_PERIOD":      "60",
		"OVN_ADVERTISED_UDN_ISOLATION_MODE": "loose",
		"OVN_ENABLE_OVNKUBE_IDENTITY":       enabledValue,
	}

	// ovnkubeConfLines are the ovnkube.conf lines maximalCR produces.
	ovnkubeConfLines = []string{
		"transport = no-overlay",
		"outbound-snat = enabled",
		"routing = managed",
		"as-number = 65001",
		"topology = full-mesh",
		"frr-namespace = frr-system",
		"routing-table-id-start = 1002",
	}

	testPlacement = &k8sv1alpha1.NodePlacement{
		NodeSelector: map[string]string{testNodeLabel: "enabled"},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: testNodeLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"enabled"},
					}},
				}},
			},
		}},
		Tolerations: []corev1.Toleration{{
			Key: testNodeLabel, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		}},
	}

	testImage = k8sv1alpha1.ContainerImage{
		Name:       defaults.DefaultImageName,
		Repository: "example.com/ovnkube",
		Tag:        "test",
		SHA256:     strings.Repeat("ab", 32),
	}

	// customVariableEnv maps component -> container -> custom variable -> the
	// env var the templates feed it into.
	customVariableEnv = map[string]map[string]map[string]string{
		k8sv1alpha1.ComponentOvnkubeControlPlane: {
			"ovnkube-cluster-manager": {
				"logLevel":          ovnkubeLogLevelEnv,
				"logFileMaxSize":    "OVNKUBE_LOGFILE_MAXSIZE",
				"logFileMaxBackups": "OVNKUBE_LOGFILE_MAXBACKUPS",
				"logFileMaxAge":     "OVNKUBE_LOGFILE_MAXAGE",
			},
		},
		k8sv1alpha1.ComponentOvnkubeNode: {
			"nb-ovsdb":   {"nbLogLevel": "OVN_LOGLEVEL_NB"},
			"sb-ovsdb":   {"sbLogLevel": "OVN_LOGLEVEL_SB"},
			"ovn-northd": {"northdLogLevel": "OVN_LOGLEVEL_NORTHD"},
			"ovnkube-controller": {
				"ovnkubeNodeLogLevel":   ovnkubeLogLevelEnv,
				"logFileMaxSize":        "OVNKUBE_LOGFILE_MAXSIZE",
				"logFileMaxBackups":     "OVNKUBE_LOGFILE_MAXBACKUPS",
				"logFileMaxAge":         "OVNKUBE_LOGFILE_MAXAGE",
				"libovsdbClientLogFile": "OVNKUBE_LIBOVSDB_CLIENT_LOGFILE",
			},
			"ovn-controller": {"ovnControllerLogLevel": "OVN_LOGLEVEL_CONTROLLER"},
		},
		k8sv1alpha1.ComponentOvnkubeIdentity: {
			identityWorkload: {"logLevel": ovnkubeLogLevelEnv},
		},
	}
)

// decodeCR decodes a CR strictly, so a typo fails instead of silently
// dropping a setting.
func decodeCR(content string) *k8sv1alpha1.OVNK8sOperator {
	GinkgoHelper()
	cr := &k8sv1alpha1.OVNK8sOperator{}
	Expect(sigyaml.UnmarshalStrict([]byte(content), cr)).To(Succeed())
	return cr
}

// renderRealTemplates renders cr with the shipped templates/ directory.
func renderRealTemplates(cr *k8sv1alpha1.OVNK8sOperator, opts renderer.RenderOptions) []*unstructured.Unstructured {
	GinkgoHelper()
	config, err := defaults.LoadEmbeddedConfig()
	Expect(err).ToNot(HaveOccurred())
	manifests, err := renderer.NewTemplateRenderer("../../templates", config).RenderAllTemplates(cr, opts)
	Expect(err).ToNot(HaveOccurred())

	var objects []*unstructured.Unstructured
	for _, componentManifests := range manifests {
		for _, manifest := range componentManifests {
			obj := &unstructured.Unstructured{}
			Expect(utilyaml.Unmarshal([]byte(manifest.Content), &obj.Object)).To(Succeed())
			objects = append(objects, obj)
		}
	}
	return objects
}

// decodeKind returns the rendered objects of one kind, by name.
func decodeKind[T any](objects []*unstructured.Unstructured, kind string) map[string]T {
	GinkgoHelper()
	result := map[string]T{}
	for _, obj := range objects {
		if obj.GetKind() != kind {
			continue
		}
		var typed T
		Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &typed)).To(Succeed())
		result[obj.GetName()] = typed
	}
	return result
}

func names(objects []*unstructured.Unstructured, kind string) []string {
	var result []string
	for _, obj := range objects {
		if obj.GetKind() == kind {
			result = append(result, obj.GetName())
		}
	}
	return result
}

// workloadPodSpecs returns the pod spec of each standard-mode workload.
func workloadPodSpecs(objects []*unstructured.Unstructured) map[string]corev1.PodSpec {
	GinkgoHelper()
	deployments := decodeKind[appsv1.Deployment](objects, kinds.Deployment)
	daemonSets := decodeKind[appsv1.DaemonSet](objects, kinds.DaemonSet)
	specs := map[string]corev1.PodSpec{
		k8sv1alpha1.ComponentOvnkubeControlPlane: deployments[controlPlaneWorkload].Spec.Template.Spec,
		k8sv1alpha1.ComponentOvnkubeNode:         daemonSets[nodeWorkload].Spec.Template.Spec,
		k8sv1alpha1.ComponentOvnkubeIdentity:     daemonSets[identityWorkload].Spec.Template.Spec,
	}
	for component, spec := range specs {
		Expect(spec.Containers).ToNot(BeEmpty(), "%s workload was not rendered", component)
	}
	return specs
}

// workloadEnv returns the distinct values of each env var across the
// containers of every rendered Deployment and DaemonSet.
func workloadEnv(objects []*unstructured.Unstructured) map[string][]string {
	GinkgoHelper()
	deployments := decodeKind[appsv1.Deployment](objects, kinds.Deployment)
	daemonSets := decodeKind[appsv1.DaemonSet](objects, kinds.DaemonSet)
	specs := make([]corev1.PodSpec, 0, len(deployments)+len(daemonSets))
	for _, deployment := range deployments {
		specs = append(specs, deployment.Spec.Template.Spec)
	}
	for _, daemonSet := range daemonSets {
		specs = append(specs, daemonSet.Spec.Template.Spec)
	}
	env := map[string][]string{}
	for _, spec := range specs {
		for _, container := range slices.Concat(spec.InitContainers, spec.Containers) {
			for _, variable := range container.Env {
				if variable.ValueFrom == nil && !slices.Contains(env[variable.Name], variable.Value) {
					env[variable.Name] = append(env[variable.Name], variable.Value)
				}
			}
		}
	}
	return env
}

func customValue(component, variable string) string {
	return component + "-" + variable
}

// integrationCR is maximalCR plus every component override on each
// standard-mode component.
func integrationCR() *k8sv1alpha1.OVNK8sOperator {
	GinkgoHelper()
	cr := decodeCR(maximalCR)
	cr.Spec.ComponentOverrides = nil
	for _, component := range []string{
		k8sv1alpha1.ComponentOvnkubeControlPlane,
		k8sv1alpha1.ComponentOvnkubeNode,
		k8sv1alpha1.ComponentOvnkubeIdentity,
	} {
		override := k8sv1alpha1.ComponentOverride{
			ResourceName:    component,
			NodePlacement:   testPlacement,
			ContainerImages: []k8sv1alpha1.ContainerImage{testImage},
		}
		for _, variables := range customVariableEnv[component] {
			for variable := range variables {
				override.CustomVariables = append(override.CustomVariables,
					k8sv1alpha1.CustomVariable{Name: variable, Value: customValue(component, variable)})
			}
		}
		switch component {
		case k8sv1alpha1.ComponentOvnkubeControlPlane:
			override.Replicas = new(int32(3))
		case k8sv1alpha1.ComponentOvnkubeIdentity:
			override.Enabled = new(true)
		}
		cr.Spec.ComponentOverrides = append(cr.Spec.ComponentOverrides, override)
	}
	return cr
}

var _ = Describe("Rendering the real templates", func() {
	Context("with every setting", func() {
		var objects []*unstructured.Unstructured

		BeforeEach(func() {
			objects = renderRealTemplates(integrationCR(), renderer.RenderOptions{WebhookCABundle: testCABundle})
		})

		It("places every object and service account in the configured namespace", func() {
			for _, obj := range objects {
				Expect(obj.GetNamespace()).To(BeElementOf("", testNamespace), "%s %s", obj.GetKind(), obj.GetName())
			}
			var subjects []rbacv1.Subject
			for _, binding := range decodeKind[rbacv1.ClusterRoleBinding](objects, kinds.ClusterRoleBinding) {
				subjects = append(subjects, binding.Subjects...)
			}
			for _, binding := range decodeKind[rbacv1.RoleBinding](objects, kinds.RoleBinding) {
				subjects = append(subjects, binding.Subjects...)
			}
			Expect(subjects).ToNot(BeEmpty())
			for _, subject := range subjects {
				if subject.Kind == rbacv1.ServiceAccountKind {
					Expect(subject.Namespace).To(Equal(testNamespace), subject.Name)
				}
			}
		})

		It("renders the network settings into ovn-config", func() {
			data := decodeKind[corev1.ConfigMap](objects, kinds.ConfigMap)["ovn-config"].Data
			Expect(data).To(HaveKeyWithValue("net_cidr", "10.128.0.0/14/23,fd01::/48/64"))
			Expect(data).To(HaveKeyWithValue("svc_cidr", "172.30.0.0/16,fd02::/112"))
			Expect(data).To(HaveKeyWithValue("k8s_apiserver", "https://10.0.0.10:6443"))
			Expect(data).To(HaveKeyWithValue("mtu", "1400"))
		})

		It("renders every feature gate and setting into the container env vars", func() {
			env := workloadEnv(objects)
			for name, gate := range gateEnv {
				Expect(env[name]).To(ConsistOf(enabledValue), "%s (gate %s)", name, gate)
			}
			for name, want := range settingEnv {
				Expect(env[name]).To(ConsistOf(want), name)
			}
		})

		It("renders the no-overlay and routing options into ovnkube.conf", func() {
			conf := decodeKind[corev1.ConfigMap](objects, kinds.ConfigMap)["ovnkube-config"].Data["ovnkube.conf"]
			for _, line := range ovnkubeConfLines {
				Expect(conf).To(ContainSubstring(line))
			}
		})

		It("renders the default-network RouteAdvertisements", func() {
			Expect(names(objects, kinds.RouteAdvertisements)).To(ConsistOf("default"))
		})

		It("attaches the image pull secrets to every workload", func() {
			for component, spec := range workloadPodSpecs(objects) {
				Expect(spec.ImagePullSecrets).To(Equal([]corev1.LocalObjectReference{{Name: "pull-secret"}}), component)
			}
		})

		It("injects the CA bundle into every identity webhook", func() {
			configs := decodeKind[admissionregistrationv1.ValidatingWebhookConfiguration](
				objects, kinds.ValidatingWebhookConfiguration)
			Expect(configs).To(HaveLen(3))
			for name, config := range configs {
				Expect(config.Webhooks).ToNot(BeEmpty(), name)
				for _, webhook := range config.Webhooks {
					Expect(string(webhook.ClientConfig.CABundle)).To(Equal(testCABundle), name)
				}
			}
		})

		It("renders an empty caBundle when no CA bundle is passed", func() {
			unset := renderRealTemplates(integrationCR(), renderer.RenderOptions{})
			configs := decodeKind[admissionregistrationv1.ValidatingWebhookConfiguration](
				unset, kinds.ValidatingWebhookConfiguration)
			Expect(configs).To(HaveLen(3))
			for name, config := range configs {
				for _, webhook := range config.Webhooks {
					Expect(webhook.ClientConfig.CABundle).To(BeEmpty(), name)
				}
			}
		})

		It("applies the node placement override to every workload", func() {
			for component, spec := range workloadPodSpecs(objects) {
				Expect(spec.NodeSelector).To(Equal(testPlacement.NodeSelector), component)
				Expect(spec.Affinity).To(Equal(testPlacement.Affinity), component)
				Expect(spec.Tolerations).To(Equal(testPlacement.Tolerations), component)
			}
		})

		It("applies the image override to every container", func() {
			want := fmt.Sprintf("%s:%s@sha256:%s", testImage.Repository, testImage.Tag, testImage.SHA256)
			for component, spec := range workloadPodSpecs(objects) {
				for _, container := range slices.Concat(spec.InitContainers, spec.Containers) {
					Expect(container.Image).To(Equal(want), "%s/%s", component, container.Name)
				}
			}
		})

		It("feeds custom variables into the container env vars", func() {
			specs := workloadPodSpecs(objects)
			for component, containers := range customVariableEnv {
				for containerName, variables := range containers {
					index := slices.IndexFunc(specs[component].Containers, func(c corev1.Container) bool {
						return c.Name == containerName
					})
					Expect(index).ToNot(Equal(-1), "%s has no container %q", component, containerName)
					env := specs[component].Containers[index].Env
					for variable, envName := range variables {
						Expect(env).To(ContainElement(corev1.EnvVar{Name: envName, Value: customValue(component, variable)}),
							"%s/%s: custom variable %q", component, containerName, variable)
					}
				}
			}
		})

		It("applies the control-plane replica override", func() {
			deployments := decodeKind[appsv1.Deployment](objects, kinds.Deployment)
			Expect(deployments[controlPlaneWorkload].Spec.Replicas).To(HaveValue(Equal(int32(3))))
		})
	})

	Context("with no optional settings", func() {
		var objects []*unstructured.Unstructured

		BeforeEach(func() {
			objects = renderRealTemplates(decodeCR(minimalCR), renderer.RenderOptions{})
		})

		It("leaves every feature gate off", func() {
			env := workloadEnv(objects)
			for name, gate := range gateEnv {
				Expect(env).To(HaveKey(name))
				Expect(env[name]).ToNot(ContainElement(enabledValue), "%s (gate %s)", name, gate)
			}
		})

		It("renders no identity webhook", func() {
			Expect(names(objects, kinds.ValidatingWebhookConfiguration)).To(BeEmpty())
			Expect(names(objects, kinds.DaemonSet)).To(ConsistOf(nodeWorkload))
			Expect(workloadEnv(objects)["OVN_ENABLE_OVNKUBE_IDENTITY"]).To(ConsistOf("false"))
		})
	})
})
