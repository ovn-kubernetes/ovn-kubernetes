package ovn

import (
	gotesting "testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestLayer2LocalNodeSyncs(t *gotesting.T) {
	const node = "worker1"
	all := nodeSyncs{syncMgmtPort: true, syncGw: true, syncReroute: true, syncClusterRouterPort: true}
	for _, tc := range []struct {
		name          string
		add           bool
		advertised    bool
		gatewayMarked bool
		want          nodeSyncs
	}{
		{name: "an add syncs everything", add: true, want: all},
		{name: "an add syncs everything whatever is marked", add: true, gatewayMarked: true, want: all},
		{name: "an update with nothing changed syncs nothing"},
		{name: "an update with the gateway marked syncs it", gatewayMarked: true, want: nodeSyncs{syncGw: true}},
	} {
		t.Run(tc.name, func(t *gotesting.T) {
			oc := newLayer2NodeSyncsTestController(t, node, tc.advertised)
			if tc.gatewayMarked {
				oc.gatewaysFailed.Store(node, true)
			}
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}
			var old *corev1.Node
			if !tc.add {
				old = n
			}
			if got := *oc.localNodeSyncs(old, n, nil, nil); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func newLayer2NodeSyncsTestController(t *gotesting.T, node string, advertised bool) *Layer2UserDefinedNetworkController {
	t.Helper()
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	nad := ovntest.GenerateNADWithConfig("rednad", "greenamespace", `
{
        "cniVersion": "1.1.0",
        "name": "bluenet",
        "type": "ovn-k8s-cni-overlay",
        "topology": "layer2",
        "subnets": "100.128.0.0/16",
        "mtu": 1300,
        "netAttachDefName": "greenamespace/rednad",
        "role": "primary"
}
`)
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}
	mutable := util.NewMutableNetInfo(netInfo)
	if advertised {
		mutable.SetPodNetworkAdvertisedVRFs(map[string][]string{node: {types.DefaultNetworkName}})
	}
	return &Layer2UserDefinedNetworkController{
		BaseLayer2UserDefinedNetworkController: BaseLayer2UserDefinedNetworkController{
			BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
				BaseNetworkController: BaseNetworkController{
					ReconcilableNetInfo: util.NewReconcilableNetInfo(mutable),
				},
			},
		},
	}
}
