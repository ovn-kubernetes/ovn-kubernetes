package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/applyconfiguration/clusternetworkconnect/v1"
	networkconnectclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
)

const (
	ovnNetworkConnectSubnetAnnotation          = "k8s.ovn.org/network-connect-subnet"
	networkConnectSubnetAnnotationFieldManager = "ovn-kubernetes-network-connect-controller-subnet-annotation"
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
