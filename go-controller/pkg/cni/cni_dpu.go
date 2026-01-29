package cni

import (
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// updatePodDPUConnDetailsWithRetry update the pod annotation with the given connection details for the NAD in
// the PodRequest. If the dpuConnDetails argument is nil, delete the NAD's DPU connection details annotation instead.
func (pr *PodRequest) updatePodDPUConnDetailsWithRetry(kube kube.Interface, podLister corev1listers.PodLister, dpuConnDetails *util.DPUConnectionDetails) error {
	pod, err := podLister.Pods(pr.PodNamespace).Get(pr.PodName)
	if err != nil {
		return err
	}
	err = util.UpdatePodDPUConnDetailsWithRetry(
		podLister,
		kube,
		pod,
		dpuConnDetails,
		pr.nadKey,
	)
	if util.IsAnnotationAlreadySetError(err) {
		return nil
	}

	return err
}

func (pr *PodRequest) addDPUConnectionDetailsAnnot(k kube.Interface, podLister corev1listers.PodLister, vfNetdevName string, dpuProvider util.DPUProvider) error {
	dpuConnDetails, err := dpuProvider.CreateConnectionDetails(pr.CNIConf.DeviceID, pr.SandboxID, vfNetdevName)
	if err != nil {
		return err
	}

	return pr.updatePodDPUConnDetailsWithRetry(k, podLister, dpuConnDetails)
}
