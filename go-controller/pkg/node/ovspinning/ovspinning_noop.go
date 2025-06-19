//go:build !linux
// +build !linux

package ovspinning

import (
	"context"

	"k8s.io/klog/v2"
)

func Run(ctx context.Context, _ <-chan struct{}) {
	klog.Infof("OVS CPU pinning is supported on linux platform only")
}
