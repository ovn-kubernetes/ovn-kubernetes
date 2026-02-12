package util

import (
	"context"
	"fmt"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// EnsureDefaultNetworkNAD ensures that a well-known NAD exists for the
// default network in ovn-k namespace. This will allow the users to customize
// the primary UDN attachments with static IPs, and/or MAC address requests, by
// using the multus-cni `default network` feature.
func EnsureDefaultNetworkNAD(nadLister nadlisters.NetworkAttachmentDefinitionLister, nadClient nadclientset.Interface) (*nadtypes.NetworkAttachmentDefinition, error) {
	nad, err := nadLister.NetworkAttachmentDefinitions(config.Kubernetes.OVNConfigNamespace).Get(types.DefaultNetworkName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if nad != nil {
		return nad, nil
	}
	return nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(config.Kubernetes.OVNConfigNamespace).Create(
		context.Background(),
		&nadtypes.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      types.DefaultNetworkName,
				Namespace: config.Kubernetes.OVNConfigNamespace,
			},
			Spec: nadtypes.NetworkAttachmentDefinitionSpec{
				Config: fmt.Sprintf("{\"cniVersion\": \"0.4.0\", \"name\": \"ovn-kubernetes\", \"type\": \"%s\"}", config.CNI.Plugin),
			},
		},
		// note we don't set ourselves as field manager for this create as we
		// want to process the resulting event that would otherwise be filtered
		// out in nadNeedsUpdate
		metav1.CreateOptions{},
	)
}
