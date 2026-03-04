package udn

import (
	"fmt"
	"strings"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadv1Listers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	nadutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// wait on a certain pod annotation related condition. in case of non-nil error,
// return error to abort the retry attempt
type podAnnotWaitCond = func(*corev1.Pod, string) (*util.PodAnnotation, bool, error)

const resourceNameAnnot = "k8s.v1.cni.cncf.io/resourceName"

type UserDefinedPrimaryNetwork struct {
	networkManager networkmanager.Interface
	nadLister      nadv1Listers.NetworkAttachmentDefinitionLister
	annotation     *util.PodAnnotation

	// the pod's UDN primary network, and its associated NAD name and the resourceName defined in the NAD
	activeNetwork util.NetInfo
	nadName       string
	resourceName  string

	// the pod's UDN primary interface's device information (when UDN's resourceName is defined)
	// deviceInfo is extra device information stored by SRIOV device plugin in /var/run/k8s.cni.cncf.io/devinfo/dp
	deviceInfo *nadapi.DeviceInfo
	// deviceID is either a PCI address or an Auxiliary address (format <driver>.<type>.<id>, i.e. mlx5_core.sf.2) of the VF assigned to the pod
	deviceID string
	// netdevName is the linux network device name (such as enps0) when the VF device is bound to a kernel driver
	netdevName string
	// isVFIO indicates if the VF device is bound to the VFIO driver, exposing direct device access to userspace applications or virtual machines
	isVFIO bool
}

func NewPrimaryNetwork(networkManager networkmanager.Interface, nadLister nadv1Listers.NetworkAttachmentDefinitionLister) *UserDefinedPrimaryNetwork {
	return &UserDefinedPrimaryNetwork{
		networkManager: networkManager,
		nadLister:      nadLister,
	}
}

func (p *UserDefinedPrimaryNetwork) InterfaceName() string {
	return "ovn-udn1"
}

func (p *UserDefinedPrimaryNetwork) NetworkDevice() string {
	return p.netdevName
}

func (p *UserDefinedPrimaryNetwork) NetworkDeviceInfo() (string, *nadapi.DeviceInfo, bool) {
	return p.deviceID, p.deviceInfo, p.isVFIO
}

func (p *UserDefinedPrimaryNetwork) Annotation() *util.PodAnnotation {
	return p.annotation
}

func (p *UserDefinedPrimaryNetwork) NetworkName() string {
	if p.activeNetwork == nil {
		return ""
	}
	return p.activeNetwork.GetNetworkName()
}

func (p *UserDefinedPrimaryNetwork) NADName() string {
	return p.nadName
}

func (p *UserDefinedPrimaryNetwork) MTU() int {
	if p.activeNetwork == nil {
		return 0
	}
	return p.activeNetwork.MTU()
}

func (p *UserDefinedPrimaryNetwork) Found() bool {
	// if the primary UDN interface is not VF backed, its deviceInfo is empty but not nil.
	return p.annotation != nil && p.activeNetwork != nil && p.deviceInfo != nil
}

func (p *UserDefinedPrimaryNetwork) WaitForPrimaryAnnotationFn(annotCondFn podAnnotWaitCond) podAnnotWaitCond {
	return func(pod *corev1.Pod, nadName string) (*util.PodAnnotation, bool, error) {
		annotation, isReady, err := annotCondFn(pod, nadName)
		if err != nil || !isReady {
			return annotation, isReady, err
		}
		isReady, err = p.ensure(pod, nadName, annotation)
		if err == nil && isReady {
			return annotation, isReady, err
		}
		return nil, false, err
	}
}

func (p *UserDefinedPrimaryNetwork) ensure(pod *corev1.Pod, nadName string, annotation *util.PodAnnotation) (bool, error) {
	// non default network is not related to primary UDNs
	if nadName != types.DefaultNetworkName {
		return true, nil
	}

	if annotation == nil {
		var err error
		annotation, err = util.UnmarshalPodAnnotation(pod.Annotations, nadName)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				return false, nil
			}
			return false, err
		}
	}

	// If these are pods created before primary UDN functionality the
	// default network without role is the primary network
	if annotation.Role == "" {
		annotation.Role = types.NetworkRolePrimary
	}

	// If default network is the primary there is nothing else to do
	if annotation.Role == types.NetworkRolePrimary {
		return true, nil
	}

	ready, err := p.ensureAnnotation(pod)
	if !ready || err != nil {
		return ready, err
	}
	err = p.ensureActiveNetwork(pod.Namespace)
	if err != nil {
		return false, err
	}
	err = p.ensureNetworkDevice(pod)
	return err == nil, err
}

func (p *UserDefinedPrimaryNetwork) ensureActiveNetwork(namespace string) error {
	if p.activeNetwork != nil {
		return nil
	}
	activeNetwork, err := p.networkManager.GetActiveNetworkForNamespace(namespace)
	if err != nil {
		return err
	}
	// CNI should always have an active network for a pod on our node
	if activeNetwork == nil {
		return fmt.Errorf("no active network found for namespace %s", namespace)
	}
	if activeNetwork.IsDefault() {
		return fmt.Errorf("missing primary user defined network NAD for namespace '%s'", namespace)
	}

	p.activeNetwork = activeNetwork
	return nil
}

func (p *UserDefinedPrimaryNetwork) ensureAnnotation(pod *corev1.Pod) (bool, error) {
	if p.annotation != nil {
		return true, nil
	}
	annotations := pod.GetAnnotations()
	podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return false, err
	}
	for nadFullName, podNetwork := range podNetworks {
		if podNetwork.Role != types.NetworkRolePrimary {
			continue
		}
		annotation, err := util.UnmarshalPodAnnotation(annotations, nadFullName)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				return false, nil
			}
			return false, err
		}
		// To support for non VFIO devices like SRIOV, get the primary UDN's resource name
		nadNamespace, nadName, err := cache.SplitMetaNamespaceKey(nadFullName)
		if err != nil {
			return false, fmt.Errorf("invalid NAD name %s", nadFullName)
		}
		nad, err := p.nadLister.NetworkAttachmentDefinitions(nadNamespace).Get(nadName)
		if err != nil {
			return false, fmt.Errorf("failed to get primary UDN's network-attachment-definition %s: %v", nadFullName, err)
		}
		p.annotation = annotation
		p.nadName = nadFullName
		p.resourceName = nad.Annotations[resourceNameAnnot]
		klog.Infof("Primary UDN for pod %s/%s NAD %s resource %s", pod.Namespace, pod.Name, p.nadName, p.resourceName)

		return true, nil
	}
	return false, nil
}

func (p *UserDefinedPrimaryNetwork) ensureNetworkDevice(pod *corev1.Pod) error {
	// everything has already been set, no need to retry
	if p.deviceInfo != nil && (p.resourceName == "" || p.isVFIO || p.netdevName != "") {
		return nil
	}
	// this UDN primary network interface does not request specific device
	if p.resourceName == "" {
		p.deviceInfo = &nadapi.DeviceInfo{}
		return nil
	}
	deviceID, err := GetPodPrimaryUDNDeviceID(pod, p.resourceName)
	if err != nil {
		return fmt.Errorf("failed to get primary UDN device ID for pod %s/%s resource %s: %v",
			pod.Namespace, pod.Name, p.resourceName, err)
	}
	if util.IsPCIDeviceName(deviceID) {
		// DeviceID is a PCI address
		p.isVFIO = util.GetSriovnetOps().IsVfPciVfioBound(deviceID)
	} else if util.IsAuxDeviceName(deviceID) {
		// DeviceID is an Auxiliary device name - <driver_name>.<kind_of_a_type>.<id>
		chunks := strings.Split(deviceID, ".")
		if chunks[1] != "sf" {
			return fmt.Errorf("primary UDN's device info for pod %s/%s resource %s deviceID %s: only SF auxiliary devices are supported",
				pod.Namespace, pod.Name, p.resourceName, deviceID)
		}
	} else {
		return fmt.Errorf("primary UDN's device info for pod %s/%s resource %s deviceID %s: unexpected PCI or Auxiliary device name",
			pod.Namespace, pod.Name, p.resourceName, deviceID)
	}
	p.deviceID = deviceID

	deviceInfo, err := nadutils.LoadDeviceInfoFromDP(p.resourceName, deviceID)
	if err != nil {
		return fmt.Errorf("failed to load primary UDN's device info for pod %s/%s resource %s deviceID %s: %w",
			pod.Namespace, pod.Name, p.resourceName, deviceID, err)
	}
	p.deviceInfo = deviceInfo

	if !p.isVFIO {
		netdevName, err := util.GetNetdevNameFromDeviceId(deviceID, *deviceInfo)
		if err != nil {
			return fmt.Errorf("failed to get primary UDN's netdev name for pod %s/%s resource %s deviceID %s: %w",
				pod.Namespace, pod.Name, p.resourceName, deviceID, err)
		}
		p.netdevName = netdevName
	}

	klog.Infof("Primary UDN %s for pod %s/%s NAD %s resource %s deviceID %s deviceInfo %v netdevName %v isVFIO %v",
		p.activeNetwork.GetNetworkName(), pod.Namespace, pod.Name, p.nadName, p.resourceName, deviceID, p.deviceInfo, p.netdevName, p.isVFIO)
	return nil
}
