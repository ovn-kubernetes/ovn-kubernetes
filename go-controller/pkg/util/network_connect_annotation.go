package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/applyconfiguration/clusternetworkconnect/v1"
	networkconnectclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
)

const (
	ovnNetworkConnectSubnetAnnotation          = "k8s.ovn.org/network-connect-subnet"
	OvnConnectRouterTunnelKeyAnnotation        = "k8s.ovn.org/connect-router-tunnel-key"
	networkConnectSubnetAnnotationFieldManager = "ovn-kubernetes-network-connect-controller-subnet-annotation"
	networkConnectRouterTunnelKeyFieldManager  = "ovn-kubernetes-network-connect-controller-tunnel-key-annotation"
)

type NetworkConnectSubnetAnnotation struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// UpdateNetworkConnectSubnetAnnotation patches the subnet annotation for the given CNC and given allocated subnets.
// It uses the Apply method to patch the annotation and has its own manager field to avoid conflicts with other annotation patches
// like the router tunnel key annotation patch below.
func UpdateNetworkConnectSubnetAnnotation(cnc *networkconnectv1.ClusterNetworkConnect, cncClient networkconnectclientset.Interface, allocatedSubnets map[string][]*net.IPNet) error {
	// Build annotation directly from allocatedSubnets (always the full list)
	subnetsMap := make(map[string]NetworkConnectSubnetAnnotation)
	for networkName, subnets := range allocatedSubnets {
		annotation := NetworkConnectSubnetAnnotation{}
		for _, subnet := range subnets {
			if subnet.IP.To4() != nil {
				annotation.IPv4 = subnet.String()
			} else {
				annotation.IPv6 = subnet.String()
			}
		}
		subnetsMap[networkName] = annotation
	}

	bytes, err := json.Marshal(subnetsMap)
	if err != nil {
		return fmt.Errorf("failed to marshal network connect subnet annotation: %v", err)
	}

	// Apply only the annotation this controller manages, leaving other annotations untouched
	applyObj := networkconnectapply.ClusterNetworkConnect(cnc.Name)
	applyObj.Annotations = map[string]string{
		ovnNetworkConnectSubnetAnnotation: string(bytes),
	}
	_, err = cncClient.K8sV1().ClusterNetworkConnects().Apply(context.TODO(), applyObj,
		metav1.ApplyOptions{FieldManager: networkConnectSubnetAnnotationFieldManager, Force: true})
	if err != nil {
		return fmt.Errorf("failed to apply network connect subnet annotation: %v", err)
	}
	klog.V(5).Infof("Updated network connect subnet annotation for CNC %s with %d subnets", cnc.Name, len(allocatedSubnets))
	return nil
}

// ParseNetworkConnectSubnetAnnotation parses the subnet annotation from the given CNC.
// Returns a map of owner (e.g., "layer3_1", "layer2_2") to allocated subnets.
// Returns empty map if annotation is missing or empty.
func ParseNetworkConnectSubnetAnnotation(cnc *networkconnectv1.ClusterNetworkConnect) (map[string][]*net.IPNet, error) {
	result := make(map[string][]*net.IPNet)

	if cnc == nil || cnc.Annotations == nil {
		return result, nil
	}

	annotationValue, exists := cnc.Annotations[ovnNetworkConnectSubnetAnnotation]
	if !exists || annotationValue == "" || annotationValue == "{}" {
		return result, nil
	}

	var subnetsMap map[string]NetworkConnectSubnetAnnotation
	if err := json.Unmarshal([]byte(annotationValue), &subnetsMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal network connect subnet annotation: %v", err)
	}

	for owner, annotation := range subnetsMap {
		var subnets []*net.IPNet
		if annotation.IPv4 != "" {
			_, ipnet, err := net.ParseCIDR(annotation.IPv4)
			if err != nil {
				return nil, fmt.Errorf("failed to parse IPv4 subnet %s for owner %s: %v", annotation.IPv4, owner, err)
			}
			subnets = append(subnets, ipnet)
		}
		if annotation.IPv6 != "" {
			_, ipnet, err := net.ParseCIDR(annotation.IPv6)
			if err != nil {
				return nil, fmt.Errorf("failed to parse IPv6 subnet %s for owner %s: %v", annotation.IPv6, owner, err)
			}
			subnets = append(subnets, ipnet)
		}
		if len(subnets) > 0 {
			result[owner] = subnets
		}
	}

	return result, nil
}

// UpdateNetworkConnectRouterTunnelKeyAnnotation updates the router tunnel key annotation for the given CNC and given tunnel ID.
// It uses the Apply method to patch the annotation and has its own manager field to avoid conflicts with other annotation patches
// like the subnet annotation patch above.
func UpdateNetworkConnectRouterTunnelKeyAnnotation(cncName string, cncClient networkconnectclientset.Interface, tunnelID int) error {
	applyObj := networkconnectapply.ClusterNetworkConnect(cncName).
		WithAnnotations(map[string]string{
			OvnConnectRouterTunnelKeyAnnotation: strconv.Itoa(tunnelID),
		})
	_, err := cncClient.K8sV1().ClusterNetworkConnects().Apply(
		context.TODO(),
		applyObj,
		metav1.ApplyOptions{FieldManager: networkConnectRouterTunnelKeyFieldManager, Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to apply network connect router tunnel key annotation: %v", err)
	}
	klog.V(5).Infof("Updated network connect router tunnel key annotation for CNC %s with tunnel ID %d", cncName, tunnelID)
	return nil
}

// ParseNetworkConnectTunnelKeyAnnotation parses the tunnel key annotation from the given CNC.
// Returns 0 if annotation is missing.
func ParseNetworkConnectTunnelKeyAnnotation(cnc *networkconnectv1.ClusterNetworkConnect) (int, error) {
	if cnc == nil || cnc.Annotations == nil {
		return 0, nil
	}

	annotationValue, exists := cnc.Annotations[OvnConnectRouterTunnelKeyAnnotation]
	if !exists || annotationValue == "" {
		return 0, nil
	}

	tunnelID, err := strconv.Atoi(annotationValue)
	if err != nil {
		return 0, fmt.Errorf("failed to parse tunnel key annotation: %v", err)
	}

	return tunnelID, nil
}
