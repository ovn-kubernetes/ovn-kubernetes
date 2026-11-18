// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux
// +build !linux

package ovspinning

import (
	"context"

	"k8s.io/klog/v2"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
)

func Run(_ context.Context, _ <-chan struct{}, _ podresourcesapi.PodResourcesListerClient, _ libovsdbclient.Client) {
	klog.Infof("OVS CPU pinning is supported on linux platform only")
}
