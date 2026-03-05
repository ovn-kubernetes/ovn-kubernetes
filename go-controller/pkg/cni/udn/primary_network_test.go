package udn

import (
	"fmt"
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	v1nadmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	types "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/gomega"
)

func TestWaitForPrimaryAnnotationFn(t *testing.T) {

	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	namespace := "ns1"
	nadName := "nad1"
	wrongNadName := "nad2"
	pod := &corev1.Pod{}
	pod.Namespace = namespace
	pod.Annotations = map[string]string{
		"k8s.ovn.org/pod-networks": `{"ns1/nad1": {
			"role": "primary",
			"mac_address": "0a:58:fd:98:00:01"
		}}`,
	}

	tests := []struct {
		description           string
		nadName               string
		annotationFromFn      *util.PodAnnotation
		isReadyFromFn         bool
		pod                   *corev1.Pod
		nads                  []*nadapi.NetworkAttachmentDefinition
		getActiveNetworkError error
		expectedIsReady       bool
		expectedFound         bool
		expectedAnnotation    *util.PodAnnotation
		expectedNADName       string
		expectedNetworkName   string
		expectedMTU           int
		expectedError         error
	}{
		{
			description: "With non default nad should be ready",
			nadName:     "red",
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleSecondary,
			},
			isReadyFromFn:   true,
			pod:             &corev1.Pod{},
			expectedIsReady: true,
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRoleSecondary,
			},
		},
		{
			description:        "With no ovn annotation should force return not ready",
			nadName:            types.DefaultNetworkName,
			annotationFromFn:   nil,
			isReadyFromFn:      true,
			pod:                &corev1.Pod{},
			expectedAnnotation: nil,
			expectedIsReady:    false,
		},
		{
			description: "With primary default should be ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRolePrimary,
			},
			isReadyFromFn: true,
			pod:           &corev1.Pod{},
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRolePrimary,
			},
			expectedIsReady: true,
		},
		{
			description: "With default network without role should be ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: "",
			},
			isReadyFromFn: true,
			pod:           &corev1.Pod{},
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRolePrimary,
			},
			expectedIsReady: true,
		},

		{
			description: "With missing primary annotation and active network should return not ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn:   true,
			pod:             &corev1.Pod{},
			expectedIsReady: false,
		},
		{
			description: "With primary network annotation and missing active network should return not ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNAD("blue", wrongNadName, namespace,
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			isReadyFromFn:   true,
			pod:             pod,
			expectedIsReady: false,
			expectedError:   fmt.Errorf("failed to get primary UDN's network-attachment-definition %s/%s: not found", namespace, nadName),
		},
		{
			description: "With missing primary network annotation and active network should return not ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			pod:           &corev1.Pod{},
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNAD("blue", nadName, namespace,
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			expectedIsReady: false,
		},
		{
			description: "With primary network annotation and active network should return ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			pod:           pod,
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNAD("blue", nadName, namespace,
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			expectedIsReady:     true,
			expectedFound:       true,
			expectedNetworkName: "blue",
			expectedNADName:     util.GetNADName(namespace, nadName),
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			expectedMTU: 1300,
		},
		{
			description: "With primary network annotation and active network and no MTU should return ready with default MTU",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			pod:           pod,
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNADWithoutMTU("blue", nadName, namespace,
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			expectedIsReady:     true,
			expectedFound:       true,
			expectedNetworkName: "blue",
			expectedNADName:     util.GetNADName(namespace, nadName),
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			expectedMTU: 1400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			g := NewWithT(t)
			// needs to be set so the primary user defined networks can use ipfamilies supported by the underlying cluster
			config.IPv4Mode = true
			config.IPv6Mode = true
			nadLister := v1nadmocks.NetworkAttachmentDefinitionLister{}
			nadNamespaceLister := v1nadmocks.NetworkAttachmentDefinitionNamespaceLister{}
			if tt.nads != nil {
				nadLister.On("NetworkAttachmentDefinitions", tt.pod.Namespace).Return(&nadNamespaceLister)
				if tt.expectedNADName != "" {
					nadNamespaceLister.On("Get", nadName).Return(tt.nads[0], nil)
				} else {
					nadNamespaceLister.On("Get", nadName).Return(nil, fmt.Errorf("not found"))
				}
			}
			waitCond := func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
				return tt.annotationFromFn, tt.isReadyFromFn, nil
			}

			fakeNetworkManager := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{},
			}
			for _, nad := range tt.nads {
				nadNetwork, _ := util.ParseNADInfo(nad)
				mutableNetInfo := util.NewMutableNetInfo(nadNetwork)
				mutableNetInfo.SetNADs(util.GetNADName(nad.Namespace, nad.Name))
				nadNetwork = mutableNetInfo
				if nadNetwork.IsPrimaryNetwork() {
					if _, loaded := fakeNetworkManager.PrimaryNetworks[nad.Namespace]; !loaded {
						fakeNetworkManager.PrimaryNetworks[nad.Namespace] = nadNetwork
					}
				}
			}

			userDefinedPrimaryNetwork := NewPrimaryNetwork(fakeNetworkManager, &nadLister)
			obtainedAnnotation, obtainedIsReady, err := userDefinedPrimaryNetwork.WaitForPrimaryAnnotationFn(
				waitCond)(tt.pod, tt.nadName)
			obtainedFound := userDefinedPrimaryNetwork.Found()
			obtainedNetworkName := userDefinedPrimaryNetwork.NetworkName()
			obtainedNADName := userDefinedPrimaryNetwork.NADName()
			obtainedMTU := userDefinedPrimaryNetwork.MTU()
			if tt.expectedError == nil {
				g.Expect(err).ToNot(HaveOccurred(), "should not return error")
			} else {
				g.Expect(err).To(MatchError(tt.expectedError.Error()), "should return expected error")
			}
			g.Expect(obtainedIsReady).To(Equal(tt.expectedIsReady), "should return expected readiness")
			g.Expect(obtainedFound).To(Equal(tt.expectedFound), "should return expected found flag")
			g.Expect(obtainedNetworkName).To(Equal(tt.expectedNetworkName), "should return expected network name")
			g.Expect(obtainedNADName).To(Equal(tt.expectedNADName), "should return expected nad name")
			g.Expect(obtainedAnnotation).To(Equal(tt.expectedAnnotation), "should return expected ovn pod annotation")
			g.Expect(obtainedMTU).To(Equal(tt.expectedMTU), "should return expected MTU")
		})
	}
}
