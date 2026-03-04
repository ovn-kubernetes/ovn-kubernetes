package kube

import (
	"context"
	"encoding/json"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimssclientset "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpcloudnetworkclientset "github.com/openshift/client-go/cloudnetwork/clientset/versioned"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"

	adminpolicybasedrouteclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	egressfirewall "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned"
	egressqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned"
	egressserviceclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
)

// InterfaceOVN represents the exported methods for dealing with getting/setting
// kubernetes and OVN resources
type InterfaceOVN interface {
	Interface
	UpdateEgressFirewall(egressfirewall *egressfirewall.EgressFirewall) error
	UpdateEgressIP(eIP *egressipv1.EgressIP) error
	PatchEgressIP(name string, patchData []byte) error
	GetEgressIP(name string) (*egressipv1.EgressIP, error)
	GetEgressIPs() ([]*egressipv1.EgressIP, error)
	GetEgressFirewalls() ([]*egressfirewall.EgressFirewall, error)
	CreateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error)
	UpdateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error)
	DeleteCloudPrivateIPConfig(name string) error
	UpdateEgressServiceStatus(namespace, name, host string) error
	UpdateIPAMClaimIPs(updatedIPAMClaim *ipamclaimsapi.IPAMClaim) error
}

// Interface represents the exported methods for dealing with getting/setting
// kubernetes resources
type Interface interface {
	SetAnnotationsOnPod(namespace, podName string, annotations map[string]interface{}) error
	SetAnnotationsOnService(namespace, serviceName string, annotations map[string]interface{}) error
	SetAnnotationsOnNode(nodeName string, annotations map[string]interface{}) error
	SetAnnotationsOnNamespace(namespaceName string, annotations map[string]interface{}) error
	SetLabelsOnNode(nodeName string, labels map[string]interface{}) error
	PatchNode(old, new *corev1.Node) error
	UpdateNodeStatus(node *corev1.Node) error
	UpdatePodStatus(pod *corev1.Pod) error
	// GetPodsForDBChecker should only be used by legacy DB checker. Use watchFactory instead to get pods.
	GetPodsForDBChecker(namespace string, opts metav1.ListOptions) ([]*corev1.Pod, error)
	// GetNodeForWindows should only be used for windows hybrid overlay binary and never in linux code
	GetNodeForWindows(name string) (*corev1.Node, error)
	GetNodesForWindows() ([]*corev1.Node, error)
	Events() kv1core.EventInterface
}

// Kube works with kube client only
// Implements Interface
type Kube struct {
	KClient kubernetes.Interface
}

// KubeOVN works with all kube and ovn resources
// Implements InterfaceOVN
type KubeOVN struct {
	Kube
	ANPClient            anpclientset.Interface
	EIPClient            egressipclientset.Interface
	EgressFirewallClient egressfirewallclientset.Interface
	CloudNetworkClient   ocpcloudnetworkclientset.Interface
	EgressServiceClient  egressserviceclientset.Interface
	APBRouteClient       adminpolicybasedrouteclientset.Interface
	EgressQoSClient      egressqosclientset.Interface
	IPAMClaimsClient     ipamclaimssclientset.Interface
	NADClient            nadclientset.Interface
	NetworkQoSClient     networkqosclientset.Interface
}

// SetAnnotationsOnPod takes the pod object and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnPod(namespace, podName string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	podDesc := namespace + "/" + podName
	klog.Infof("Setting annotations %v on pod %s", annotations, podDesc)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on pod %s: %v", podDesc, err)
		return err
	}

	_, err = k.KClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on pod %s: %v", podDesc, err)
	}
	return err
}

// SetAnnotationsOnNode takes the node name and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnNode(nodeName string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	klog.Infof("Setting annotations %v on node %s", annotations, nodeName)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on node %s: %v", nodeName, err)
		return err
	}

	_, err = k.KClient.CoreV1().Nodes().PatchStatus(context.TODO(), nodeName, patchData)
	if err != nil {
		klog.Errorf("Error in setting annotation on node %s: %v", nodeName, err)
	}
	return err
}

// SetAnnotationsOnNamespace takes the namespace name and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnNamespace(namespaceName string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	klog.Infof("Setting annotations %v on namespace %s", annotations, namespaceName)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on namespace %s: %v", namespaceName, err)
		return err
	}

	_, err = k.KClient.CoreV1().Namespaces().Patch(context.TODO(), namespaceName, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on namespace %s: %v", namespaceName, err)
	}
	return err
}

// SetAnnotationsOnService takes a service namespace and name and a map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnService(namespace, name string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	serviceDesc := namespace + "/" + name
	klog.Infof("Setting annotations %v on service %s", annotations, serviceDesc)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on service %s: %v", serviceDesc, err)
		return err
	}

	_, err = k.KClient.CoreV1().Services(namespace).Patch(context.TODO(), name, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on service %s: %v", serviceDesc, err)
	}
	return err
}

// SetLabelsOnNode takes the node name and map of key/value string pairs to set as labels
func (k *Kube) SetLabelsOnNode(nodeName string, labels map[string]interface{}) error {
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"labels": labels,
		},
	}

	klog.V(4).Infof("Setting labels %v on node %s", labels, nodeName)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting labels on node %s: %v", nodeName, err)
		return err
	}

	_, err = k.KClient.CoreV1().Nodes().PatchStatus(context.TODO(), nodeName, patchData)
	return err
}

// PatchNode patches the old node object with the changes provided in the new node object.
func (k *Kube) PatchNode(old, new *corev1.Node) error {
	oldNodeObjectJson, err := json.Marshal(old)
	if err != nil {
		klog.Errorf("Unable to marshal node %s: %v", old.Name, err)
		return err
	}

	newNodeObjectJson, err := json.Marshal(new)
	if err != nil {
		klog.Errorf("Unable to marshal node %s: %v", new.Name, err)
		return err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNodeObjectJson, newNodeObjectJson, corev1.Node{})
	if err != nil {
		klog.Errorf("Unable to patch node %s: %v", old.Name, err)
		return err
	}

	if _, err = k.KClient.CoreV1().Nodes().Patch(context.TODO(), old.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		klog.Errorf("Unable to patch node %s: %v", old.Name, err)
		return err
	}

	return nil
}

// UpdateNodeStatus takes the node object and sets the provided update status
func (k *Kube) UpdateNodeStatus(node *corev1.Node) error {
	klog.Infof("Updating status on node %s", node.Name)
	_, err := k.KClient.CoreV1().Nodes().UpdateStatus(context.TODO(), node, metav1.UpdateOptions{})
	return err
}

// UpdatePodStatus update pod with provided pod data, limited to .Status and .ObjectMeta fields
func (k *Kube) UpdatePodStatus(pod *corev1.Pod) error {
	klog.Infof("Updating pod %s/%s", pod.Namespace, pod.Name)
	_, err := k.KClient.CoreV1().Pods(pod.Namespace).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
	return err
}

// GetPodsForDBChecker returns the list of all Pod objects in a namespace matching the options. Only used by the legacy db checker.
func (k *Kube) GetPodsForDBChecker(namespace string, opts metav1.ListOptions) ([]*corev1.Pod, error) {
	list := []*corev1.Pod{}
	opts.ResourceVersion = "0"
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.KClient.CoreV1().Pods(namespace).List(ctx, opts)
	}).EachListItem(context.TODO(), opts, func(obj runtime.Object) error {
		list = append(list, obj.(*corev1.Pod))
		return nil
	})
	return list, err
}

// GetNodesForWindows returns the list of all Node objects from kubernetes. Only used by windows binary.
func (k *Kube) GetNodesForWindows() ([]*corev1.Node, error) {
	list := []*corev1.Node{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.KClient.CoreV1().Nodes().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*corev1.Node))
		return nil
	})
	return list, err
}

// GetNodeForWindows returns the Node resource from kubernetes apiserver, given its name. Only used by windows binary.
func (k *Kube) GetNodeForWindows(name string) (*corev1.Node, error) {
	return k.KClient.CoreV1().Nodes().Get(context.TODO(), name, metav1.GetOptions{})
}

// Events returns events to use when creating an EventSinkImpl
func (k *Kube) Events() kv1core.EventInterface {
	return k.KClient.CoreV1().Events("")
}

// UpdateEgressFirewall updates the EgressFirewall with the provided EgressFirewall data
func (k *KubeOVN) UpdateEgressFirewall(egressfirewall *egressfirewall.EgressFirewall) error {
	klog.Infof("Updating status on EgressFirewall %s in namespace %s", egressfirewall.Name, egressfirewall.Namespace)
	_, err := k.EgressFirewallClient.K8sV1().EgressFirewalls(egressfirewall.Namespace).Update(context.TODO(), egressfirewall, metav1.UpdateOptions{})
	return err
}

// UpdateEgressIP updates the EgressIP with the provided EgressIP data
func (k *KubeOVN) UpdateEgressIP(eIP *egressipv1.EgressIP) error {
	klog.Infof("Updating status on EgressIP %s status %v", eIP.Name, eIP.Status)
	_, err := k.EIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIP, metav1.UpdateOptions{})
	return err
}

func (k *KubeOVN) PatchEgressIP(name string, patchData []byte) error {
	_, err := k.EIPClient.K8sV1().EgressIPs().Patch(context.TODO(), name, types.JSONPatchType, patchData, metav1.PatchOptions{})
	return err
}

// GetEgressIP returns the EgressIP object from kubernetes
func (k *KubeOVN) GetEgressIP(name string) (*egressipv1.EgressIP, error) {
	return k.EIPClient.K8sV1().EgressIPs().Get(context.TODO(), name, metav1.GetOptions{})
}

// GetEgressIPs returns the list of all EgressIP objects from kubernetes
func (k *KubeOVN) GetEgressIPs() ([]*egressipv1.EgressIP, error) {
	list := []*egressipv1.EgressIP{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.EIPClient.K8sV1().EgressIPs().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*egressipv1.EgressIP))
		return nil
	})
	return list, err
}

// GetEgressFirewalls returns the list of all EgressFirewall objects from kubernetes
func (k *KubeOVN) GetEgressFirewalls() ([]*egressfirewall.EgressFirewall, error) {
	list := []*egressfirewall.EgressFirewall{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.EgressFirewallClient.K8sV1().EgressFirewalls(metav1.NamespaceAll).List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*egressfirewall.EgressFirewall))
		return nil
	})
	return list, err
}

func (k *KubeOVN) CreateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error) {
	return k.CloudNetworkClient.CloudV1().CloudPrivateIPConfigs().Create(context.TODO(), cloudPrivateIPConfig, metav1.CreateOptions{})
}

func (k *KubeOVN) UpdateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error) {
	return k.CloudNetworkClient.CloudV1().CloudPrivateIPConfigs().Update(context.TODO(), cloudPrivateIPConfig, metav1.UpdateOptions{})
}

func (k *KubeOVN) DeleteCloudPrivateIPConfig(name string) error {
	return k.CloudNetworkClient.CloudV1().CloudPrivateIPConfigs().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (k *KubeOVN) UpdateEgressServiceStatus(namespace, name, host string) error {
	es, err := k.EgressServiceClient.K8sV1().EgressServices(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	es.Status.Host = host

	_, err = k.EgressServiceClient.K8sV1().EgressServices(es.Namespace).UpdateStatus(context.TODO(), es, metav1.UpdateOptions{})
	return err
}

func (k *KubeOVN) UpdateIPAMClaimIPs(updatedIPAMClaim *ipamclaimsapi.IPAMClaim) error {
	_, err := k.IPAMClaimsClient.K8sV1alpha1().IPAMClaims(updatedIPAMClaim.Namespace).UpdateStatus(context.TODO(), updatedIPAMClaim, metav1.UpdateOptions{})
	return err
}

// SetAnnotationsOnNAD takes a NAD namespace and name and a map of key/value string pairs to set as annotations
func (k *KubeOVN) SetAnnotationsOnNAD(namespace, name string, annotations map[string]string, fieldManager string) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	patchData, err = json.Marshal(&patch)
	if err != nil {
		return err
	}

	patchOptions := metav1.PatchOptions{}
	if fieldManager != "" {
		patchOptions.FieldManager = fieldManager
	}

	_, err = k.NADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Patch(context.Background(), name, types.MergePatchType, patchData, patchOptions)
	return err
}
