// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	networkQoSCRDName = "networkqoses.k8s.ovn.org"
	portRuleMessage   = "protocol is required when port is set"
	burstRuleMessage  = "rate is required when burst is set"
	// The API server applies a changed CRD schema shortly after the update.
	schemaChangeTimeout = 30 * time.Second
)

// Objects are built from YAML: the typed client always sends a protocol, even an empty one.
func newNetworkQoS(spec string) *unstructured.Unstructured {
	manifest := "apiVersion: k8s.ovn.org/v1alpha1\nkind: NetworkQoS\nmetadata:\n  generateName: nqos-\n  namespace: default\nspec:\n" + spec
	obj := &unstructured.Unstructured{}
	Expect(utilyaml.Unmarshal([]byte(manifest), &obj.Object)).To(Succeed())
	return obj
}

func networkQoSWithPorts(ports string) *unstructured.Unstructured {
	return newNetworkQoS(fmt.Sprintf(`  priority: 50
  egress:
  - dscp: 10
    classifier:
      ports: %s
`, ports))
}

func networkQoSWithBandwidth(bandwidth string) *unstructured.Unstructured {
	return newNetworkQoS(fmt.Sprintf(`  priority: 50
  egress:
  - dscp: 10
    bandwidth: %s
`, bandwidth))
}

func networkQoSWithNADSelector(name string) *unstructured.Unstructured {
	return newNetworkQoS(fmt.Sprintf(`  networkSelectors:
  - networkSelectionType: NetworkAttachmentDefinitions
    networkAttachmentDefinitionSelector:
      namespaceSelector: {}
      networkSelector:
        matchLabels:
          name: %s
  priority: 50
  egress:
  - dscp: 10
`, name))
}

func createNetworkQoS(obj *unstructured.Unstructured) error {
	err := k8sClient.Create(ctx, obj)
	if err == nil {
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	}
	return err
}

func getNetworkQoSCRD() *unstructured.Unstructured {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: networkQoSCRDName}, crd)).To(Succeed())
	return crd
}

// Serve the schema of the CRDs that preceded the port and bandwidth rules: no CEL rules, and a
// protocol pattern that admits any string starting with TCP.
func serveLegacySchema() {
	crd := getNetworkQoSCRD()
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	Expect(err).NotTo(HaveOccurred())
	Expect(versions).To(HaveLen(1))
	version := versions[0].(map[string]interface{})
	rulePath := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "egress", "items", "properties"}
	portPath := append(append([]string{}, rulePath...), "classifier", "properties", "ports", "items")
	port, found, err := unstructured.NestedMap(version, portPath...)
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	delete(port, "x-kubernetes-validations")
	protocol := port["properties"].(map[string]interface{})["protocol"].(map[string]interface{})
	delete(protocol, "enum")
	protocol["pattern"] = "^TCP|UDP|SCTP$"
	Expect(unstructured.SetNestedMap(version, port, portPath...)).To(Succeed())
	bandwidthPath := append(append([]string{}, rulePath...), "bandwidth")
	bandwidth, found, err := unstructured.NestedMap(version, bandwidthPath...)
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	delete(bandwidth, "x-kubernetes-validations")
	Expect(unstructured.SetNestedMap(version, bandwidth, bandwidthPath...)).To(Succeed())
	Expect(unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")).To(Succeed())
	Expect(k8sClient.Update(ctx, crd)).To(Succeed())
}

func serveCRDSpec(spec map[string]interface{}) {
	crd := getNetworkQoSCRD()
	crd.Object["spec"] = spec
	Expect(k8sClient.Update(ctx, crd)).To(Succeed())
}

func portRuleError() error {
	probe := networkQoSWithPorts("[{port: 80}]")
	err := k8sClient.Create(ctx, probe)
	if err == nil {
		Expect(k8sClient.Delete(ctx, probe)).To(Succeed())
	}
	return err
}

var _ = Describe("NetworkQoS CRD", func() {
	Context("spec.egress[].classifier.ports", func() {
		DescribeTable("rejects a port that OVN cannot match",
			func(ports, message string) {
				Expect(createNetworkQoS(networkQoSWithPorts(ports))).To(MatchError(ContainSubstring(message)))
			},
			Entry("a port without a protocol", "[{port: 80}]", portRuleMessage),
			Entry("an unknown protocol that the earlier pattern admitted", "[{protocol: TCPX, port: 80}]", `Unsupported value: "TCPX"`),
			Entry("a lowercase protocol", "[{protocol: tcp}]", `Unsupported value: "tcp"`),
		)

		DescribeTable("accepts a protocol with or without a port",
			func(ports string) {
				Expect(createNetworkQoS(networkQoSWithPorts(ports))).To(Succeed())
			},
			Entry("a protocol without a port", "[{protocol: TCP}]"),
			Entry("a protocol with a port", "[{protocol: UDP, port: 53}]"),
			Entry("entries with and without a port", "[{protocol: SCTP}, {protocol: TCP, port: 8080}]"),
		)
	})

	Context("spec.egress[].bandwidth", func() {
		It("rejects a burst without a rate", func() {
			Expect(createNetworkQoS(networkQoSWithBandwidth("{burst: 100}"))).To(MatchError(ContainSubstring(burstRuleMessage)))
		})

		DescribeTable("accepts a rate with or without a burst",
			func(bandwidth string) {
				Expect(createNetworkQoS(networkQoSWithBandwidth(bandwidth))).To(Succeed())
			},
			Entry("a rate without a burst", "{rate: 10000}"),
			Entry("a rate with a burst", "{rate: 10000, burst: 100}"),
		)
	})

	Context("spec.networkSelectors", func() {
		It("rejects a change of the network selectors", func() {
			nqos := networkQoSWithNADSelector("blue")
			Expect(createNetworkQoS(nqos)).To(Succeed())
			nqos.Object["spec"] = networkQoSWithNADSelector("red").Object["spec"]
			Expect(k8sClient.Update(ctx, nqos)).To(MatchError(ContainSubstring("networkSelectors are immutable")))
		})
	})

	// The rules form an atomic list, so an update that changes any rule validates every rule.
	Context("objects stored before the port and bandwidth rules", func() {
		It("accepts updates that leave the rules unchanged, and rule changes once the rules are fixed", func() {
			current := getNetworkQoSCRD().Object["spec"].(map[string]interface{})
			serveLegacySchema()
			DeferCleanup(func() {
				serveCRDSpec(current)
				Eventually(portRuleError).WithTimeout(schemaChangeTimeout).Should(MatchError(ContainSubstring(portRuleMessage)))
			})
			legacy := newNetworkQoS(`  priority: 50
  egress:
  - dscp: 10
    classifier:
      ports: [{port: 8080}, {protocol: TCPX, port: 80}]
    bandwidth: {burst: 100}
`)
			Eventually(func() error { return k8sClient.Create(ctx, legacy) }).WithTimeout(schemaChangeTimeout).Should(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })
			serveCRDSpec(current)
			Eventually(portRuleError).WithTimeout(schemaChangeTimeout).Should(MatchError(ContainSubstring(portRuleMessage)))

			By("accepting a change outside the rules")
			Expect(unstructured.SetNestedField(legacy.Object, int64(60), "spec", "priority")).To(Succeed())
			Expect(k8sClient.Update(ctx, legacy)).To(Succeed())

			update := legacy.DeepCopy()
			changeRule := func(ports, bandwidth string) error {
				update.Object["spec"] = newNetworkQoS(fmt.Sprintf(`  priority: 60
  egress:
  - dscp: 20
    classifier:
      ports: %s
    bandwidth: %s
`, ports, bandwidth)).Object["spec"]
				return k8sClient.Update(ctx, update)
			}
			By("rejecting a change to a rule while a port has an unknown protocol")
			Expect(changeRule("[{port: 8080}, {protocol: TCPX, port: 80}]", "{burst: 100}")).To(
				MatchError(ContainSubstring(`Unsupported value: "TCPX"`)))

			By("rejecting it while a port has no protocol or a burst has no rate, which are checked once the schema errors are fixed")
			Expect(changeRule("[{port: 8080}, {protocol: TCP, port: 80}]", "{burst: 100}")).To(
				MatchError(SatisfyAll(ContainSubstring(portRuleMessage), ContainSubstring(burstRuleMessage))))

			By("accepting it once every port and the bandwidth are fixed")
			Expect(changeRule("[{protocol: TCP, port: 8080}, {protocol: TCP, port: 80}]", "{rate: 10000, burst: 100}")).To(Succeed())
		})
	})
})
