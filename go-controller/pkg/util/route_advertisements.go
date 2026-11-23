// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"slices"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// RouteAdvertisementsIntent is what an applied RouteAdvertisements asks for on
// a network whose pod network it advertises. Cluster manager records it on the
// network's NADs, keyed by RouteAdvertisements name, in the
// route-advertisements-intent annotation:
//
//	{"example-ra": {"uid": "ra-uid", "generation": 2, "targetVRF": "auto"}}
//
// The entry stays until cluster manager applies a different state, so nodes
// keep the last applied intent while a RouteAdvertisements is edited, rejected
// or being deleted.
type RouteAdvertisementsIntent struct {
	// UID and Generation identify the applied spec when a network's NADs
	// receive updates at different times. Generation restarts on recreation.
	UID        ktypes.UID `json:"uid,omitempty"`
	Generation int64      `json:"generation,omitempty"`
	// TargetVRF is the target VRF as specified in the RouteAdvertisements.
	TargetVRF string `json:"targetVRF"`
	// EgressIP is set if EgressIPs are advertised as well.
	EgressIP bool `json:"egressIP,omitempty"`
}

// NewRouteAdvertisementsIntent returns the intent of a RouteAdvertisements, or
// nil if it does not advertise the pod network.
func NewRouteAdvertisementsIntent(ra *ratypes.RouteAdvertisements) *RouteAdvertisementsIntent {
	if ra == nil || !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) {
		return nil
	}
	return &RouteAdvertisementsIntent{
		UID:        ra.UID,
		Generation: ra.Generation,
		TargetVRF:  ra.Spec.TargetVRF,
		EgressIP:   slices.Contains(ra.Spec.Advertisements, ratypes.EgressIP),
	}
}

// ResolvedVRF returns the VRF the network's routes are generated in; an empty
// string means the default VRF.
func (i RouteAdvertisementsIntent) ResolvedVRF(network NetInfo) string {
	if i.TargetVRF != "auto" {
		return i.TargetVRF
	}
	if network.IsDefault() {
		return ""
	}
	return GetNetworkVRFName(network)
}

// GetRouteAdvertisementsPodPrefixes returns the current pod CIDRs required on
// this node: node host subnets for layer 3, network subnets for layer 2.
func GetRouteAdvertisementsPodPrefixes(network NetInfo, node *corev1.Node) ([]string, error) {
	var prefixes []string
	if network.TopologyType() == types.Layer2Topology {
		for _, subnet := range network.Subnets() {
			prefixes = append(prefixes, subnet.CIDR.String())
		}
	} else {
		subnets, err := ParseNodeHostSubnetAnnotation(node, network.GetNetworkName())
		if err != nil {
			return nil, fmt.Errorf("failed to get pod prefixes for network %q on node %q: %w", network.GetNetworkName(), node.Name, err)
		}
		prefixes = StringSlice(subnets)
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("no pod prefixes for network %q on node %q", network.GetNetworkName(), node.Name)
	}
	return prefixes, nil
}

// RouteAdvertisementsConfigurationExportsPrefix reports whether a router in
// the intended VRF originates the prefix and permits a peer to export it.
func RouteAdvertisementsConfigurationExportsPrefix(cfg *frrtypes.FRRConfiguration, vrf, prefix string) bool {
	for _, router := range cfg.Spec.BGP.Routers {
		if router.VRF != vrf || !slices.Contains(router.Prefixes, prefix) {
			continue
		}
		for _, peer := range router.Neighbors {
			allowed := peer.ToAdvertise.Allowed
			if allowed.Mode == frrtypes.AllowAll || slices.Contains(allowed.Prefixes, prefix) {
				return true
			}
		}
	}
	return false
}

// RouteAdvertisementsNodeLabel returns a label value identifying a node.
// The underscore keeps hashed names distinct from all valid DNS node names.
func RouteAdvertisementsNodeLabel(nodeName string) string {
	if len(nodeName) <= validation.LabelValueMaxLength {
		return nodeName
	}
	digest := sha256.Sum256([]byte(nodeName))
	return "node_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
}
