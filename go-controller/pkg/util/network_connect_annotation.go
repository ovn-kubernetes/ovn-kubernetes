package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
)

const (
	ovnNetworkConnectSubnetAnnotation = "k8s.ovn.org/network-connect-subnet"
)

type NetworkConnectSubnetAnnotation struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

func UpdateNetworkConnectSubnetAnnotation(cnc *networkconnectv1.ClusterNetworkConnect, cncClient networkconnectclientset.Interface, allocatedSubnets map[string][]*net.IPNet) error {
	// First get existing annotations
	annotations := cnc.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// Parse existing network connect subnet annotations
	subnetsMap, err := parseNetworkConnectSubnetAnnotation(annotations)
	if err != nil {
		if !IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse network connect subnet annotation: %v", err)
		}
		// annotation doesn't exist yet
		subnetsMap = make(map[string]NetworkConnectSubnetAnnotation)
	}

	// Update the map with new allocated subnets
	for networkName, subnets := range allocatedSubnets {
		if len(subnets) == 0 {
			// Delete this network's entry if no subnets
			delete(subnetsMap, networkName)
			continue
		}

		annotation := NetworkConnectSubnetAnnotation{}
		for _, subnet := range subnets {
			if subnet.IP.To4() != nil {
				// IPv4 subnet
				annotation.IPv4 = subnet.String()
			} else {
				// IPv6 subnet
				annotation.IPv6 = subnet.String()
			}
		}
		subnetsMap[networkName] = annotation
	}

	// If no subnets left, delete the annotation entirely
	if len(subnetsMap) == 0 {
		delete(annotations, ovnNetworkConnectSubnetAnnotation)
	} else {
		// Marshal back to JSON and set annotation
		bytes, err := json.Marshal(subnetsMap)
		if err != nil {
			return fmt.Errorf("failed to marshal network connect subnet annotation: %v", err)
		}
		annotations[ovnNetworkConnectSubnetAnnotation] = string(bytes)
	}

	// Update annotations on the CNC object
	cnc.SetAnnotations(annotations)

	// Update the object via client
	_, err = cncClient.K8sV1().ClusterNetworkConnects().Update(context.TODO(), cnc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update network connect subnet annotation: %v", err)
	}
	return nil
}

// parseNetworkConnectSubnetAnnotation parses the network connect subnet annotation
func parseNetworkConnectSubnetAnnotation(annotations map[string]string) (map[string]NetworkConnectSubnetAnnotation, error) {
	annotation, ok := annotations[ovnNetworkConnectSubnetAnnotation]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", ovnNetworkConnectSubnetAnnotation)
	}

	subnetsMap := make(map[string]NetworkConnectSubnetAnnotation)
	if err := json.Unmarshal([]byte(annotation), &subnetsMap); err != nil {
		return nil, fmt.Errorf("could not parse %q annotation %q: %v", ovnNetworkConnectSubnetAnnotation, annotation, err)
	}

	return subnetsMap, nil
}
