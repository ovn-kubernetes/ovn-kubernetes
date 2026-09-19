// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package mgmtportdevice

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	testNodeName = "node-a"
	testNetwork  = "red"

	redVF2        = `{"red":{"DeviceId":"0000:03:00.2","PfId":0,"FuncId":2}}`
	redVF2Rewrite = `{"red":{"FuncId":2,"PfId":0,"DeviceId":"0000:03:00.2"}}`
	redVF3        = `{"red":{"DeviceId":"0000:03:00.3","PfId":0,"FuncId":3}}`
	redAndBlue    = `{"blue":{"DeviceId":"0000:03:00.4","PfId":0,"FuncId":4},"red":{"DeviceId":"0000:03:00.2","PfId":0,"FuncId":2}}`
)

func nodeWithDevices(name, devices string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if devices != "" {
		node.Annotations = map[string]string{util.OvnNodeManagementPort: devices}
	}
	return node
}

func TestNeedsUpdate(t *testing.T) {
	tests := []struct {
		name    string
		oldNode *corev1.Node
		newNode *corev1.Node
		want    bool
	}{
		{
			name:    "an add is taken, the device may have moved before the watch started",
			newNode: nodeWithDevices(testNodeName, redVF2),
			want:    true,
		},
		{
			name:    "another node is not ours",
			newNode: nodeWithDevices("node-b", redVF3),
		},
		{
			name:    "a deletion is left to the port teardown",
			oldNode: nodeWithDevices(testNodeName, redVF2),
		},
		{
			name:    "an unchanged annotation is not a device change",
			oldNode: nodeWithDevices(testNodeName, redVF2),
			newNode: nodeWithDevices(testNodeName, redVF2),
		},
		{
			name:    "the same device re-serialized is not a device change",
			oldNode: nodeWithDevices(testNodeName, redVF2),
			newNode: nodeWithDevices(testNodeName, redVF2Rewrite),
		},
		{
			name:    "another network being allocated does not wake this one",
			oldNode: nodeWithDevices(testNodeName, redVF2),
			newNode: nodeWithDevices(testNodeName, redAndBlue),
		},
		{
			name:    "the device moving wakes this one",
			oldNode: nodeWithDevices(testNodeName, redVF2),
			newNode: nodeWithDevices(testNodeName, redVF3),
			want:    true,
		},
		{
			name:    "a device published where there was none wakes this one",
			oldNode: nodeWithDevices(testNodeName, ""),
			newNode: nodeWithDevices(testNodeName, redVF2),
			want:    true,
		},
		{
			name:    "a device withdrawn wakes this one",
			oldNode: nodeWithDevices(testNodeName, redVF2),
			newNode: nodeWithDevices(testNodeName, ""),
			want:    true,
		},
		{
			name:    "a malformed annotation reads as nothing published",
			oldNode: nodeWithDevices(testNodeName, "not json"),
			newNode: nodeWithDevices(testNodeName, "still not json"),
		},
	}

	c := &Controller{nodeName: testNodeName, network: testNetwork}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.needsUpdate(tt.oldNode, tt.newNode); got != tt.want {
				t.Errorf("needsUpdate() = %v, want %v", got, tt.want)
			}
		})
	}
}
