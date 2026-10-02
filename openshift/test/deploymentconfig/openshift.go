// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deploymentconfig

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

// FedoraKubevirtContainerDiskImage matches Origin's approved live-migration test image.
const FedoraKubevirtContainerDiskImage = "quay.io/kubevirt/fedora-with-test-tooling-container-disk:v1.8.2"

var (
	imageIDMapping map[api.ImageID]imageutils.ImageID = map[api.ImageID]imageutils.ImageID{
		api.Agnhost:                     imageutils.Agnhost,
		api.FedoraKubevirtContainerDisk: imageutils.None,
	}
	imageConfigMap map[api.ImageID]api.ImageConfig
)

func init() {
	// Add images that are needed by the test suite.
	imageConfigMap = map[api.ImageID]api.ImageConfig{
		api.Agnhost: {
			ImageID:  api.Agnhost,
			PullSpec: imageutils.GetE2EImage(imageutils.Agnhost),
		},
		api.FedoraKubevirtContainerDisk: {
			ImageID:  api.FedoraKubevirtContainerDisk,
			PullSpec: FedoraKubevirtContainerDiskImage,
		},
	}
}

func IsOpenShift(config *rest.Config) (bool, error) {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	// Check for OpenShift-specific API groups
	groups, err := kubeClient.Discovery().ServerGroups()
	if err != nil {
		return false, fmt.Errorf("failed to get server groups: %w", err)
	}
	for _, group := range groups.Groups {
		if strings.HasSuffix(group.Name, ".openshift.io") {
			return true, nil
		}
	}
	return false, nil
}

type openshift struct {
	kubeConfig     *rest.Config
	requiredImages map[api.ImageID]struct{}
}

func New(config *rest.Config) api.DeploymentConfig {
	return &openshift{kubeConfig: config,
		requiredImages: make(map[api.ImageID]struct{})}
}

func (m *openshift) OVNKubernetesNamespace() string {
	return "openshift-ovn-kubernetes"
}

func (m *openshift) FRRK8sNamespace() string {
	return "openshift-frr-k8s"
}

func (m *openshift) ExternalBridgeName() string {
	return "br-ex"
}

func (m *openshift) PrimaryInterfaceName() string {
	if m.kubeConfig == nil {
		return ""
	}
	kubeClient, err := kubernetes.NewForConfig(m.kubeConfig)
	if err != nil {
		panic(fmt.Sprintf("failed to create kubernetes client: %v", err))
	}
	nodes, err := kubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{Limit: 1})
	if err != nil {
		panic(fmt.Sprintf("failed to list nodes: %v", err))
	}
	if len(nodes.Items) == 0 {
		panic("no nodes found in cluster")
	}
	iface, err := m.findPrimaryInterface(kubeClient, nodes.Items[0].Name)
	if err != nil {
		panic(fmt.Sprintf("failed to find primary interface: %v", err))
	}
	return iface
}

func (m *openshift) IsConfigurationEnabled(config api.Config) bool {
	return false
}

func (m *openshift) NBDBContainerName() string {
	return "nbdb"
}

func (m *openshift) GetImage(imageID api.ImageID) api.ImageConfig {
	if imageID == api.FedoraKubevirtContainerDisk && os.Getenv("KUBE_TEST_REPO") != "" {
		pullSpec, err := imageutils.ReplaceRegistryInImageURL(imageConfigMap[imageID].PullSpec)
		if err != nil {
			panic(err)
		}
		return api.ImageConfig{ImageID: imageID, PullSpec: pullSpec}
	}
	return imageConfigMap[imageID]
}

func (m *openshift) AddRequiredImage(imageID ...api.ImageID) {
	for _, imgID := range imageID {
		m.requiredImages[imgID] = struct{}{}
	}
}

func (m *openshift) GetRequiredImages() []api.ImageConfig {
	imageConfigs := []api.ImageConfig{}
	for imageID := range m.requiredImages {
		newID, ok := imageIDMapping[imageID]
		if !ok {
			newID = imageutils.None
		}
		imageConfigs = append(imageConfigs, api.ImageConfig{
			ImageID:  api.ImageID(newID),
			PullSpec: imageConfigMap[imageID].PullSpec,
		})
	}
	return imageConfigs
}

func (m *openshift) GetNodeMachineNetworks() (map[string][]string, error) {
	if m.kubeConfig == nil {
		return make(map[string][]string), nil
	}
	kubeClient, err := kubernetes.NewForConfig(m.kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	result := make(map[string][]string, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		// On cloud platforms, CNCC sets the cloud egress IP annotation
		// with the actual routable subnet. Use it when present, fall
		// back to node-primary-ifaddr for baremetal.
		parsed, err := util.ParseCloudEgressIPConfig(node)
		if err != nil {
			parsed, err = util.ParseNodePrimaryIfAddr(node)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse node %s primary address: %w", node.Name, err)
		}
		var subnets []string
		if parsed.V4.Net != nil {
			subnets = append(subnets, parsed.V4.Net.String())
		}
		if parsed.V6.Net != nil {
			subnets = append(subnets, parsed.V6.Net.String())
		}
		result[node.Name] = subnets
	}
	return result, nil
}

// findPrimaryInterface queries ovs-vsctl on an ovnkube-node pod to find the
// physical uplink port on br-ex. The physical NIC is the OVS port whose
// interface type is "system".
func (m *openshift) findPrimaryInterface(kubeClient *kubernetes.Clientset, nodeName string) (string, error) {
	ovnNamespace := m.OVNKubernetesNamespace()
	ovnkubeNodePods, err := kubeClient.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return "", err
	}
	if len(ovnkubeNodePods.Items) != 1 {
		return "", fmt.Errorf("failed to find ovnkube-node pod for node instance %s", nodeName)
	}
	ovnKubeNodePodName := ovnkubeNodePods.Items[0].Name
	ports, err := e2epodoutput.RunHostCmd(ovnNamespace, ovnKubeNodePodName, "ovs-vsctl list-ports br-ex")
	if err != nil {
		return "", err
	}
	for _, port := range strings.Split(strings.TrimSpace(ports), "\n") {
		port = strings.TrimSpace(port)
		if port == "" {
			continue
		}
		out, err := e2epodoutput.RunHostCmd(ovnNamespace, ovnKubeNodePodName, fmt.Sprintf("ovs-vsctl get Port %s Interfaces", port))
		if err != nil {
			return "", err
		}
		// remove brackets on list of interfaces
		ifaces := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(out), "]"), "[")
		for _, iface := range strings.Split(ifaces, ",") {
			out, err := e2epodoutput.RunHostCmd(ovnNamespace, ovnKubeNodePodName, fmt.Sprintf("ovs-vsctl get Interface %s Type", strings.TrimSpace(iface)))
			if err != nil {
				return "", err
			}
			// If system Type we know this is the OVS port is the NIC
			if strings.TrimSpace(out) == "system" {
				return port, nil
			}
		}
	}
	return "", fmt.Errorf("failed to find network interface from ovnkube-node pod %s", ovnKubeNodePodName)
}
