package userdefinednetwork

import (
	"encoding/json"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseEVPNVIDs", func() {
	It("should extract VIDs from canonical NetConf JSON", func() {
		fullConfig := ovncnitypes.NetConf{
			Transport: ovntypes.TransportEVPN,
			EVPNConfig: &ovncnitypes.EVPNConfig{
				VTEP: "test-vtep",
				MACVRF: &ovncnitypes.VRFConfig{
					VNI:         10100,
					RouteTarget: "64512:10100",
					VID:         42,
				},
				IPVRF: &ovncnitypes.VRFConfig{
					VNI:         10101,
					RouteTarget: "64512:10101",
					VID:         43,
				},
			},
		}

		jsonBytes, err := json.Marshal(fullConfig)
		Expect(err).NotTo(HaveOccurred())

		macVID, ipVID, err := parseEVPNVIDs(string(jsonBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(macVID).To(Equal(42))
		Expect(ipVID).To(Equal(43))
	})

	It("should return zeros for non-EVPN canonical NetConf", func() {
		nonEVPNConfig := ovncnitypes.NetConf{
			Transport: "",
			Topology:  "layer3",
			Subnets:   "10.0.0.0/16",
		}

		jsonBytes, err := json.Marshal(nonEVPNConfig)
		Expect(err).NotTo(HaveOccurred())

		macVID, ipVID, err := parseEVPNVIDs(string(jsonBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(macVID).To(Equal(0))
		Expect(ipVID).To(Equal(0))
	})

	DescribeTable("should handle various configs",
		func(config string, wantMACVID, wantIPVID int, wantErr bool) {
			macVID, ipVID, err := parseEVPNVIDs(config)

			if wantErr {
				Expect(err).To(HaveOccurred())
				return
			}

			Expect(err).NotTo(HaveOccurred())
			Expect(macVID).To(Equal(wantMACVID))
			Expect(ipVID).To(Equal(wantIPVID))
		},
		Entry("empty config", "", 0, 0, false),
		Entry("non-EVPN config", `{"topology":"layer3","subnets":"10.0.0.0/16"}`, 0, 0, false),
		Entry("EVPN with both VRFs", `{"transport":"evpn","evpnConfig":{"macVRF":{"vid":10},"ipVRF":{"vid":20}}}`, 10, 20, false),
		Entry("EVPN with MAC-VRF only", `{"transport":"evpn","evpnConfig":{"macVRF":{"vid":15}}}`, 15, 0, false),
		Entry("EVPN with IP-VRF only", `{"transport":"evpn","evpnConfig":{"ipVRF":{"vid":25}}}`, 0, 25, false),
		Entry("EVPN with spaced JSON", `{"transport": "evpn", "evpnConfig": {"macVRF": {"vid": 30}}}`, 30, 0, false),
		Entry("EVPN with no VIDs", `{"transport":"evpn","evpnConfig":{"vtep":"test"}}`, 0, 0, false),
		Entry("evpn in name but not transport", `{"name":"my-evpn-network","topology":"layer3"}`, 0, 0, false),
		Entry("malformed JSON", `{"transport":"evpn", invalid`, 0, 0, true),
	)
})
