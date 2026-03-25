package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestExternalIDsForLoadBalancer(t *testing.T) {
	name := "svc-ab23"
	namespace := "ns"
	defaultNetInfo := util.DefaultNetInfo{}
	config.IPv4Mode = true
	defer func() {
		config.IPv4Mode = false
	}()
	UDNNetInfo, err := getSampleUDNNetInfo(namespace, "layer3")
	require.NoError(t, err)
	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, &defaultNetInfo),
	)

	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			// also handle no TypeMeta, which can happen.
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, &defaultNetInfo),
	)

	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
			types.NetworkExternalID:           UDNNetInfo.GetNetworkName(),
			types.NetworkRoleExternalID:       types.NetworkRolePrimary,
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, UDNNetInfo),
	)

	// ETP=Local on default network should set the etp external-id
	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
			types.LoadBalancerETPExternalID:   "local",
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			},
		}, &defaultNetInfo),
	)

	// ETP=Cluster on default network should NOT set the etp external-id
	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			},
		}, &defaultNetInfo),
	)

	// ETP=Local on UDN should set both etp and network external-ids
	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
			types.LoadBalancerETPExternalID:   "local",
			types.NetworkExternalID:           UDNNetInfo.GetNetworkName(),
			types.NetworkRoleExternalID:       types.NetworkRolePrimary,
		},
		getExternalIDsForLoadBalancer(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			},
		}, UDNNetInfo),
	)

}
