// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"fmt"

	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
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

func (pr *PodRequest) addDPUConnectionDetailsAnnot(k kube.Interface, podLister corev1listers.PodLister, vfNetdevName string) error {
	if pr.CNIConf.DeviceID == "" {
		return fmt.Errorf("DeviceID must be set for Pod request with DPU")
	}
	deviceID := pr.CNIConf.DeviceID

	details, err := util.GetDPUOps().ResolveDeviceDetails(deviceID)
	if err != nil {
		return fmt.Errorf("failed to resolve device details for %s: %v", deviceID, err)
	}

	dpuConnDetails := util.DPUConnectionDetails{
		PfId:         fmt.Sprint(details.PfId),
		VfId:         fmt.Sprint(details.FuncId),
		SandboxId:    pr.SandboxID,
		VfNetdevName: vfNetdevName,
	}

	return pr.updatePodDPUConnDetailsWithRetry(k, podLister, &dpuConnDetails)
}
