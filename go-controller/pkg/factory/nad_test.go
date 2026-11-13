// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/gomega"
)

// TestNADRouteAdvertisementsIndexInitialized verifies that each watch factory
// registers the RouteAdvertisements NAD index if and only if route
// advertisements are enabled, and that the index resolves a NAD under every
// advertisement named in its annotation and follows annotation updates.
//
// The index is what lets the network manager enqueue only the networks whose
// NADs reference a changed RouteAdvertisements, so it must be registered when
// the factory is built, before the informer starts and the RA and NAD
// controllers begin consuming it. When route advertisements are disabled
// nothing reads the index, so it must not be registered at all.
func TestNADRouteAdvertisementsIndexInitialized(t *testing.T) {
	constructors := []struct {
		name string
		new  func(*util.OVNClientset) (*WatchFactory, error)
	}{
		{"controller", func(client *util.OVNClientset) (*WatchFactory, error) {
			return NewOVNKubeControllerWatchFactory(client.GetOVNKubeControllerClientset(), "test-node")
		}},
		{"node", func(client *util.OVNClientset) (*WatchFactory, error) {
			return NewNodeWatchFactory(client.GetNodeClientset(), "test-node")
		}},
		{"cluster manager", func(client *util.OVNClientset) (*WatchFactory, error) {
			return NewClusterManagerWatchFactory(client.GetClusterManagerClientset())
		}},
	}
	for _, constructor := range constructors {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/RA=%t", constructor.name, enabled), func(t *testing.T) {
				g := NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(Succeed())
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.OVNKubernetesFeature.EnableRouteAdvertisements = enabled
				wf, err := constructor.new(util.GetOVNClientset())
				g.Expect(err).NotTo(HaveOccurred())
				t.Cleanup(wf.Shutdown)

				indexer := wf.NADInformer().Informer().GetIndexer()
				_, err = indexer.ByIndex(RouteAdvertisementsNADIndex, "ra1")
				if !enabled {
					// No consumer exists without RA, so no index is expected.
					g.Expect(err).To(HaveOccurred())
					return
				}
				g.Expect(err).NotTo(HaveOccurred())

				nad := &nadapi.NetworkAttachmentDefinition{ObjectMeta: metav1.ObjectMeta{
					Name: "nad", Namespace: "namespace",
					Annotations: map[string]string{types.OvnRouteAdvertisementsKey: `["ra1", "ra2"]`},
				}}
				g.Expect(indexer.Add(nad)).To(Succeed())
				for _, name := range []string{"ra1", "ra2"} {
					// A NAD must be indexed under every advertisement it references.
					objects, err := indexer.ByIndex(RouteAdvertisementsNADIndex, name)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(objects).To(ConsistOf(nad))
				}

				// The informer store maintains the index as annotations change.
				updated := nad.DeepCopy()
				updated.Annotations[types.OvnRouteAdvertisementsKey] = `["ra2"]`
				g.Expect(indexer.Update(updated)).To(Succeed())
				objects, err := indexer.ByIndex(RouteAdvertisementsNADIndex, "ra1")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(objects).To(BeEmpty())
				objects, err = indexer.ByIndex(RouteAdvertisementsNADIndex, "ra2")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(objects).To(ConsistOf(updated))
			})
		}
	}
}
