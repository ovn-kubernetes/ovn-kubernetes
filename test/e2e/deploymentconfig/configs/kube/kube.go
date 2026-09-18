// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package kube reads deployment configuration from a cluster the suite did not create.
package kube

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	"k8s.io/kubernetes/test/utils/image"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

const (
	ovnKubeNamespaceEnvVar = "OVN_TEST_OVNK_NAMESPACE"
	frrK8sNamespaceEnvVar  = "OVN_TEST_FRRK8S_NAMESPACE"
	externalBridgeEnvVar   = "OVN_TEST_EXTERNAL_BRIDGE"
	primaryInterfaceEnvVar = "OVN_TEST_PRIMARY_INTERFACE"
)

type kube struct{}

func New() api.DeploymentConfig {
	return kube{}
}

func (kube) OVNKubernetesNamespace() string {
	if namespace := os.Getenv(ovnKubeNamespaceEnvVar); namespace != "" {
		return namespace
	}
	namespace, err := ovnKubeNamespace()
	if err != nil {
		panic(fmt.Sprintf("failed to find where OVN-Kubernetes runs, set %s to name its namespace: %v",
			ovnKubeNamespaceEnvVar, err))
	}
	return namespace
}

func (kube) FRRK8sNamespace() string {
	if namespace := os.Getenv(frrK8sNamespaceEnvVar); namespace != "" {
		return namespace
	}
	// FRR-K8s is not on every cluster, so its absence keeps the upstream name and fails only
	// the specs that reach for it, rather than failing whatever asked.
	namespace, err := frrK8sNamespace()
	if err != nil {
		return "frr-k8s-system"
	}
	return namespace
}

func (kube) ExternalBridgeName() string {
	if bridge := os.Getenv(externalBridgeEnvVar); bridge != "" {
		return bridge
	}
	return gatewayOrSkip().bridge
}

func (kube) PrimaryInterfaceName() string {
	if uplink := os.Getenv(primaryInterfaceEnvVar); uplink != "" {
		return uplink
	}
	return gatewayOrSkip().uplink
}

func (kube) GetAgnHostContainerImage() string {
	return image.GetE2EImage(image.Agnhost)
}

// IsConfigurationEnabled reports false for every flag: the flags select which inputs a spec builds,
// and false picks the input any cluster accepts.
func (kube) IsConfigurationEnabled(api.Config) bool {
	return false
}

var (
	ovnKubeNamespace = cached(func() (string, error) { return namespaceOf("app=ovnkube-node") })
	frrK8sNamespace  = cached(func() (string, error) { return namespaceOf("app=frr-k8s") })
	gatewayBridge    = cached(discoverGateway)
)

// cached keeps an answer the cluster gave once, and keeps nothing when it failed, so that a
// lookup which timed out on one spec does not answer for the whole run.
func cached[T any](resolve func() (T, error)) func() (T, error) {
	var (
		mu     sync.Mutex
		answer T
		known  bool
	)
	return func() (T, error) {
		mu.Lock()
		defer mu.Unlock()
		if known {
			return answer, nil
		}
		resolved, err := resolve()
		if err != nil {
			return answer, err
		}
		answer, known = resolved, true
		return answer, nil
	}
}

func namespaceOf(label string) (string, error) {
	pod, err := runningPod("", label)
	if err != nil {
		return "", fmt.Errorf("find pod labelled %s: %w", label, err)
	}
	return pod.Namespace, nil
}

func runningPod(namespace, label string) (*corev1.Pod, error) {
	client, err := framework.LoadClientset()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: label,
		FieldSelector: "status.phase=Running",
		Limit:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods labelled %s: %w", label, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no running pod labelled %s in %s", label, cmp.Or(namespace, "any namespace"))
	}
	return &pods.Items[0], nil
}

type gateway struct {
	bridge string
	uplink string
}

// gatewayOrSkip skips rather than fails, because a cluster that keeps its uplink off OVS metadata
// is one the spec cannot run on, not one OVN-Kubernetes is broken on.
func gatewayOrSkip() gateway {
	discovered, err := gatewayBridge()
	if err != nil {
		ginkgo.Skip(fmt.Sprintf("this cluster does not name a gateway bridge and uplink; set %s and %s to run this spec: %v",
			externalBridgeEnvVar, primaryInterfaceEnvVar, err), 2)
	}
	return discovered
}

// discoverGateway reads the uplink OVN-Kubernetes recorded on the bridge it put it on, rather
// than looking for a physical interface, which on a node running pods is every pod's veth as
// well. One node is asked, since the suite keeps a single answer for the whole cluster.
func discoverGateway() (gateway, error) {
	pod, err := runningPod(kube{}.OVNKubernetesNamespace(), "app=ovnkube-node")
	if err != nil {
		return gateway{}, err
	}
	bridges, err := ovsVsctl(pod, "list-br")
	if err != nil {
		return gateway{}, fmt.Errorf("list OVS bridges on %s: %w", pod.Spec.NodeName, err)
	}

	var found []gateway
	for _, bridge := range strings.Fields(bridges) {
		uplink, err := ovsVsctl(pod, "--if-exists", "get", "bridge", bridge, "external_ids:bridge-uplink")
		if err != nil {
			return gateway{}, fmt.Errorf("read bridge-uplink on %s: %w", bridge, err)
		}
		if uplink = strings.Trim(uplink, "\"\n "); uplink != "" {
			found = append(found, gateway{bridge: bridge, uplink: uplink})
		}
	}
	if len(found) != 1 {
		return gateway{}, fmt.Errorf("%d of the OVS bridges on node %s name an uplink %v, and the specs mean exactly one",
			len(found), pod.Spec.NodeName, found)
	}
	return found[0], nil
}

func ovsVsctl(pod *corev1.Pod, args ...string) (string, error) {
	return e2ekubectl.RunKubectl(pod.Namespace,
		append([]string{"exec", pod.Name, "-c", "ovnkube-controller", "--", "ovs-vsctl"}, args...)...)
}
