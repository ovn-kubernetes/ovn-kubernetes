// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dependency_test

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/dependency"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Test the DAG (Directed Acyclic Graph) functionality
var _ = Describe("DAG", func() {
	var dag *dependency.DAG

	// BeforeEach runs before EVERY test in this Describe block
	// This ensures each test starts with a fresh DAG instance
	BeforeEach(func() {
		dag = dependency.NewDAG()
	})

	// Test DAG creation and basic functionality
	Context("when creating a new DAG", func() {
		It("should create an empty DAG", func() {
			// Test that a new DAG returns empty upgrade order
			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())
			Expect(order).To(BeEmpty())
		})
	})

	// Test adding components without dependencies
	Context("when adding components without dependencies", func() {
		It("should handle a single component", func() {
			dag.AddComponent("ovn-master", []string{})

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())
			Expect(order).To(Equal([]string{"ovn-master"}))
		})

		It("should handle multiple independent components", func() {
			dag.AddComponent("component-c", []string{})
			dag.AddComponent("component-a", []string{})
			dag.AddComponent("component-b", []string{})

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())

			// Independent components should be sorted alphabetically for consistency
			Expect(order).To(Equal([]string{"component-a", "component-b", "component-c"}))
		})
	})

	// Test simple dependency chains
	Context("when components have simple dependencies", func() {
		It("should handle a linear dependency chain", func() {
			// ovn-node depends on ovn-master
			dag.AddComponent("ovn-master", []string{})
			dag.AddComponent("ovn-node", []string{"ovn-master"})

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())
			Expect(order).To(Equal([]string{"ovn-master", "ovn-node"}))
		})

		It("should handle multiple components depending on the same component", func() {
			// Both ovn-node and ovn-controller depend on ovn-master
			dag.AddComponent("ovn-master", []string{})
			dag.AddComponent("ovn-node", []string{"ovn-master"})
			dag.AddComponent("ovn-controller", []string{"ovn-master"})

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())

			// ovn-master should be first, others can be in any order but alphabetically sorted
			Expect(order[0]).To(Equal("ovn-master"))
			Expect(order[1:]).To(ConsistOf("ovn-controller", "ovn-node"))
		})
	})

	// Test complex dependency scenarios
	Context("when components have complex dependencies", func() {
		It("should handle a diamond dependency pattern", func() {
			// Diamond: A -> B,C -> D
			//   A
			//  / \
			// B   C
			//  \ /
			//   D
			dag.AddComponent("a", []string{})
			dag.AddComponent("b", []string{"a"})
			dag.AddComponent("c", []string{"a"})
			dag.AddComponent("d", []string{"b", "c"})

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())

			// Verify topological ordering
			Expect(order[0]).To(Equal("a")) // A must be first
			Expect(order[3]).To(Equal("d")) // D must be last
			// B and C can be in any order but both before D
			Expect(order[1:3]).To(ConsistOf("b", "c"))
		})

		It("should handle multiple dependency levels", func() {
			// Create a realistic OVN scenario:
			// global -> ovn-master -> ovn-node -> ovn-controller
			dag.AddComponent("global", []string{})
			dag.AddComponent("ovn-master", []string{"global"})
			dag.AddComponent("ovn-node", []string{"ovn-master"})
			dag.AddComponent("ovn-controller", []string{"ovn-node"})

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())
			Expect(order).To(Equal([]string{"global", "ovn-master", "ovn-node", "ovn-controller"}))
		})
	})

	// Test cycle detection - this is critical for DAG correctness
	Context("when there are dependency cycles", func() {
		It("should detect a simple two-component cycle", func() {
			// A depends on B, B depends on A
			dag.AddComponent("a", []string{"b"})
			dag.AddComponent("b", []string{"a"})

			_, err := dag.GetUpgradeOrder()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cycle detected"))
		})

		It("should detect a three-component cycle", func() {
			// A -> B -> C -> A
			dag.AddComponent("a", []string{"c"})
			dag.AddComponent("b", []string{"a"})
			dag.AddComponent("c", []string{"b"})

			_, err := dag.GetUpgradeOrder()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cycle detected"))
		})

		It("should detect cycles even with independent components", func() {
			// Independent component + cycle
			dag.AddComponent("independent", []string{})
			dag.AddComponent("a", []string{"b"})
			dag.AddComponent("b", []string{"a"})

			_, err := dag.GetUpgradeOrder()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cycle detected"))
		})
	})

	// Test edge cases and error conditions
	Context("when handling edge cases", func() {
		It("should detect disconnected components (dependencies that don't exist)", func() {
			// Component depends on something that doesn't exist
			dag.AddComponent("ovn-node", []string{"non-existent-master"})

			_, err := dag.GetUpgradeOrder()
			Expect(err).To(HaveOccurred())
			// The DAG should detect this as a disconnected graph
			Expect(err.Error()).To(ContainSubstring("graph has cycles or disconnected components"))
		})

		It("should handle adding the same component multiple times", func() {
			// Adding same component twice should overwrite
			dag.AddComponent("ovn-master", []string{})
			dag.AddComponent("ovn-master", []string{}) // No dependencies this time

			order, err := dag.GetUpgradeOrder()
			Expect(err).ToNot(HaveOccurred())
			// Should only appear once in the result
			Expect(order).To(Equal([]string{"ovn-master"}))
		})

		It("should maintain consistent ordering across multiple calls", func() {
			// Add components in different order
			dag.AddComponent("z-component", []string{})
			dag.AddComponent("a-component", []string{})
			dag.AddComponent("m-component", []string{})

			order1, err1 := dag.GetUpgradeOrder()
			order2, err2 := dag.GetUpgradeOrder()

			Expect(err1).ToNot(HaveOccurred())
			Expect(err2).ToNot(HaveOccurred())
			Expect(order1).To(Equal(order2))
			// Should be alphabetically sorted for consistency
			Expect(order1).To(Equal([]string{"a-component", "m-component", "z-component"}))
		})
	})
})

// Test the Component struct separately
var _ = Describe("Component", func() {
	It("should handle components properly through DAG operations", func() {
		// This tests the Component struct creation indirectly
		// since Component is not exported, we test it through DAG
		dag := dependency.NewDAG()
		dag.AddComponent("test-component", []string{}) // No dependencies to avoid disconnected graph

		// The component should be properly stored and retrievable via GetUpgradeOrder
		order, err := dag.GetUpgradeOrder()
		Expect(err).ToNot(HaveOccurred())
		Expect(order).To(ContainElement("test-component"))
	})
})
