package bridgeconfig

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

func TestDefaultBridgeConfig() *BridgeConfiguration {
	defaultNetConfig := &BridgeUDNConfiguration{
		OfPortPatch: "patch-breth0_ov",
	}
	return &BridgeConfiguration{
		NetConfig: map[string]*BridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
	}
}

func TestBridgeConfig(brName string) *BridgeConfiguration {
	return &BridgeConfiguration{
		BridgeName: brName,
	}
}
