// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"strings"
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BenchmarkPodNetworkMappingFanout exercises the public mapping path so the
// same benchmark can run on revisions with and without parsed-selection reuse.
// One operation maps an unchanged eight-attachment pod across all controllers.
func BenchmarkPodNetworkMappingFanout(b *testing.B) {
	const attachments = 8
	entries := make([]string, 0, attachments)
	networksByNAD := make(map[string]string, attachments)
	for i := range attachments {
		name := fmt.Sprintf("net%d", i)
		entries = append(entries, fmt.Sprintf(`{"name":%q,"namespace":"tenant","interface":%q}`, name, name))
		networksByNAD["tenant/"+name] = name
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant",
		Name:      "pod",
		Annotations: map[string]string{
			nadapi.NetworkAttachmentAnnot: "[" + strings.Join(entries, ",") + "]",
		},
	}}
	resolve := func(nadKey string) string { return networksByNAD[nadKey] }
	for _, fanout := range []int{1, 8, 16, 450} {
		b.Run(fmt.Sprintf("controllers=%d", fanout), func(b *testing.B) {
			networks := make([]*userDefinedNetInfo, fanout)
			for i := range networks {
				networks[i] = &userDefinedNetInfo{netName: fmt.Sprintf("net%d", i)}
			}
			// Model repeated callbacks for an unchanged annotation. Cache misses
			// and annotation churn require a separate benchmark.
			for i, network := range networks {
				matched, selections, err := GetUDNPodNADToNetworkMapping(pod, network, resolve)
				wantCount := 0
				if i < attachments {
					wantCount = 1
				}
				if err != nil || matched != (wantCount == 1) || len(selections) != wantCount {
					b.Fatalf("controller %d: matched=%t, selections=%d, err=%v", i, matched, len(selections), err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				for _, network := range networks {
					if _, _, err := GetUDNPodNADToNetworkMapping(pod, network, resolve); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
