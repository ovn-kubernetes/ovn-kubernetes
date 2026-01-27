//go:build !linux
// +build !linux

package ovspinning

import (
	"context"

	"k8s.io/klog/v2"

	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
)

func Run(_ context.Context, _ <-chan struct{}, _ podresourcesapi.PodResourcesListerClient) {
	klog.Infof("OVS CPU pinning is supported on linux platform only")
}
