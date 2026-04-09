package config

import (
	"encoding/json"

	"github.com/onsi/gomega"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
)

var _ = Describe("ParseNetConf", func() {
	makeSingle := func(name, cniVersion, pluginType string, extraFields map[string]interface{}) []byte {
		conf := map[string]interface{}{
			"name":       name,
			"cniVersion": cniVersion,
			"type":       pluginType,
		}
		for k, v := range extraFields {
			conf[k] = v
		}
		b, err := json.Marshal(conf)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return b
	}

	makeConfList := func(name, cniVersion, pluginType string, extraFields map[string]interface{}) []byte {
		confList := map[string]interface{}{
			"name":       name,
			"cniVersion": cniVersion,
			"plugins":    []json.RawMessage{makeSingle("", cniVersion, pluginType, extraFields)},
		}
		b, err := json.Marshal(confList)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return b
	}

	DescribeTable("parses a valid default-network config",
		func(makeInput func(string, string, string, map[string]interface{}) []byte) {
			netconf, err := ParseNetConf(makeInput("ovn-kubernetes", "1.0.0", "ovn-k8s-cni-overlay", nil))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(netconf).NotTo(gomega.BeNil())
			gomega.Expect(netconf.Name).To(gomega.Equal(ovntypes.DefaultNetworkName))
			gomega.Expect(netconf.MTU).To(gomega.Equal(Default.MTU))
		},
		Entry("single", makeSingle),
		Entry("conflist", makeConfList),
	)

	DescribeTable("parses a valid secondary-network config",
		func(makeInput func(string, string, string, map[string]interface{}) []byte) {
			extra := map[string]interface{}{
				"topology":         "layer2",
				"netAttachDefName": "default/mynet",
			}
			netconf, err := ParseNetConf(makeInput("mynet", "1.0.0", "ovn-k8s-cni-overlay", extra))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(netconf.Topology).To(gomega.Equal("layer2"))
			gomega.Expect(netconf.Name).To(gomega.Equal("mynet"))
		},
		Entry("single", makeSingle),
		Entry("conflist", makeConfList),
	)

	DescribeTable("returns ErrorAttachDefNotOvnManaged for a non-OVN config",
		func(makeInput func(string, string, string, map[string]interface{}) []byte) {
			netconf, err := ParseNetConf(makeInput("bridge-net", "1.0.0", "bridge", nil))
			gomega.Expect(err).To(gomega.MatchError(ErrorAttachDefNotOvnManaged))
			gomega.Expect(netconf).To(gomega.BeNil())
		},
		Entry("single", makeSingle),
		Entry("conflist", makeConfList),
	)

	It("returns an error for invalid JSON", func() {
		netconf, err := ParseNetConf([]byte(`{invalid`))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(netconf).To(gomega.BeNil())
	})

	It("returns an error for an empty conflist plugins array", func() {
		input := []byte(`{"name":"empty","cniVersion":"1.0.0","plugins":[]}`)
		netconf, err := ParseNetConf(input)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(netconf).To(gomega.BeNil())
	})
})
