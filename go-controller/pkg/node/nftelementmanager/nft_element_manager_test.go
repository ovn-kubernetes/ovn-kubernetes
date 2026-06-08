// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftelementmanager

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"sigs.k8s.io/knftables"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
)

const (
	testMapV4 = "test-snat-v4"
	testMapV6 = "test-snat-v6"
)

func setupFakeWithMaps(maps ...string) *knftables.Fake {
	fake := nodenft.SetFakeNFTablesHelper()
	tx := fake.NewTransaction()
	for _, m := range maps {
		tx.Add(&knftables.Map{
			Name: m,
			Type: "ipv4_addr . ifname : ipv4_addr",
		})
	}
	err := fake.Run(context.TODO(), tx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return fake
}

func listElements(fake *knftables.Fake, mapName string) []*knftables.Element {
	elems, err := fake.ListElements(context.TODO(), "map", mapName)
	if knftables.IsNotFound(err) {
		return nil
	}
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return elems
}

func hasElement(elems []*knftables.Element, mapName string, key []string, value []string) bool {
	for _, e := range elems {
		if e.Map != mapName {
			continue
		}
		if len(e.Key) != len(key) {
			continue
		}
		match := true
		for i := range key {
			if e.Key[i] != key[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if len(e.Value) != len(value) {
			continue
		}
		for i := range value {
			if e.Value[i] != value[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

var _ = ginkgo.Describe("NFT Element Manager", func() {

	var (
		fake *knftables.Fake
		ctrl *Controller
	)

	ginkgo.BeforeEach(func() {
		fake = setupFakeWithMaps(testMapV4, testMapV6)
		ctrl = NewController()
	})

	ginkgo.Context("Add", func() {
		ginkgo.It("adds an element to nftables", func() {
			err := ctrl.Add(&knftables.Element{
				Map:   testMapV4,
				Key:   []string{"10.0.0.1", "eth0"},
				Value: []string{"192.168.1.1"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))
			gomega.Expect(hasElement(elems, testMapV4, []string{"10.0.0.1", "eth0"}, []string{"192.168.1.1"})).To(gomega.BeTrue())
		})

		ginkgo.It("is idempotent for the same element", func() {
			elem := &knftables.Element{
				Map:   testMapV4,
				Key:   []string{"10.0.0.1", "eth0"},
				Value: []string{"192.168.1.1"},
			}
			gomega.Expect(ctrl.Add(elem)).To(gomega.Succeed())
			gomega.Expect(ctrl.Add(elem)).To(gomega.Succeed())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))
		})

		ginkgo.It("updates value for the same key", func() {
			gomega.Expect(ctrl.Add(&knftables.Element{
				Map:   testMapV4,
				Key:   []string{"10.0.0.1", "eth0"},
				Value: []string{"192.168.1.1"},
			})).To(gomega.Succeed())

			gomega.Expect(ctrl.Add(&knftables.Element{
				Map:   testMapV4,
				Key:   []string{"10.0.0.1", "eth0"},
				Value: []string{"192.168.1.2"},
			})).To(gomega.Succeed())

			elems := listElements(fake, testMapV4)
			gomega.Expect(hasElement(elems, testMapV4, []string{"10.0.0.1", "eth0"}, []string{"192.168.1.2"})).To(gomega.BeTrue())
		})
	})

	ginkgo.Context("Delete", func() {
		ginkgo.It("removes a tracked element from nftables", func() {
			elem := &knftables.Element{
				Map:   testMapV4,
				Key:   []string{"10.0.0.1", "eth0"},
				Value: []string{"192.168.1.1"},
			}
			gomega.Expect(ctrl.Add(elem)).To(gomega.Succeed())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))

			gomega.Expect(ctrl.Delete(elem)).To(gomega.Succeed())

			elems = listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.BeEmpty())
		})

		ginkgo.It("is a no-op for untracked elements", func() {
			err := ctrl.Delete(&knftables.Element{
				Map:   testMapV4,
				Key:   []string{"10.0.0.1", "eth0"},
				Value: []string{"192.168.1.1"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("OwnMaps", func() {
		ginkgo.It("adds elements and owns the map", func() {
			err := ctrl.OwnMaps([]string{testMapV4},
				&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.1", "eth0"}, Value: []string{"192.168.1.1"}},
				&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.2", "eth0"}, Value: []string{"192.168.1.1"}},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(2))
		})

		ginkgo.It("prunes stale elements from owned maps", func() {
			// Seed a stale element directly in nftables
			tx := fake.NewTransaction()
			tx.Add(&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.99", "eth0"}, Value: []string{"192.168.1.99"}})
			gomega.Expect(fake.Run(context.TODO(), tx)).To(gomega.Succeed())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))

			// OwnMaps with only the legitimate element
			err := ctrl.OwnMaps([]string{testMapV4},
				&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.1", "eth0"}, Value: []string{"192.168.1.1"}},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			elems = listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))
			gomega.Expect(hasElement(elems, testMapV4, []string{"10.0.0.1", "eth0"}, []string{"192.168.1.1"})).To(gomega.BeTrue())
			gomega.Expect(hasElement(elems, testMapV4, []string{"10.0.0.99", "eth0"}, []string{"192.168.1.99"})).To(gomega.BeFalse())
		})

		ginkgo.It("does not prune elements from other maps", func() {
			// Add an element to mapV4 via Add
			gomega.Expect(ctrl.Add(&knftables.Element{
				Map: testMapV4, Key: []string{"10.0.0.1", "eth0"}, Value: []string{"192.168.1.1"},
			})).To(gomega.Succeed())

			// OwnMaps for mapV6 only — mapV4 elements should be untouched
			err := ctrl.OwnMaps([]string{testMapV6},
				&knftables.Element{Map: testMapV6, Key: []string{"fd00::1", "eth0"}, Value: []string{"fd00::100"}},
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			v4Elems := listElements(fake, testMapV4)
			gomega.Expect(v4Elems).To(gomega.HaveLen(1))
			gomega.Expect(hasElement(v4Elems, testMapV4, []string{"10.0.0.1", "eth0"}, []string{"192.168.1.1"})).To(gomega.BeTrue())

			v6Elems := listElements(fake, testMapV6)
			gomega.Expect(v6Elems).To(gomega.HaveLen(1))
		})

		ginkgo.It("rejects elements that do not belong to passed maps", func() {
			err := ctrl.OwnMaps([]string{testMapV4},
				&knftables.Element{Map: testMapV6, Key: []string{"fd00::1", "eth0"}, Value: []string{"fd00::100"}},
			)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("not in owned maps"))
		})
	})

	ginkgo.Context("fullReconcile", func() {
		ginkgo.It("prunes stale elements from owned maps", func() {
			// Own the map and add a legitimate element
			gomega.Expect(ctrl.OwnMaps([]string{testMapV4},
				&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.1", "eth0"}, Value: []string{"192.168.1.1"}},
			)).To(gomega.Succeed())

			// Inject a stale element directly into nftables
			tx := fake.NewTransaction()
			tx.Add(&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.99", "eth0"}, Value: []string{"192.168.1.99"}})
			gomega.Expect(fake.Run(context.TODO(), tx)).To(gomega.Succeed())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(2))

			// Trigger full reconcile
			ctrl.mu.Lock()
			err := ctrl.fullReconcile()
			ctrl.mu.Unlock()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			elems = listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))
			gomega.Expect(hasElement(elems, testMapV4, []string{"10.0.0.1", "eth0"}, []string{"192.168.1.1"})).To(gomega.BeTrue())
		})

		ginkgo.It("does not prune elements from non-owned maps", func() {
			// Seed an element in a map we don't own
			tx := fake.NewTransaction()
			tx.Add(&knftables.Element{Map: testMapV4, Key: []string{"10.0.0.1", "eth0"}, Value: []string{"192.168.1.1"}})
			gomega.Expect(fake.Run(context.TODO(), tx)).To(gomega.Succeed())

			// fullReconcile with no owned maps
			ctrl.mu.Lock()
			err := ctrl.fullReconcile()
			ctrl.mu.Unlock()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			elems := listElements(fake, testMapV4)
			gomega.Expect(elems).To(gomega.HaveLen(1))
		})
	})

	ginkgo.Context("elementsEqual", func() {
		ginkgo.It("returns true for identical elements", func() {
			a := &knftables.Element{Map: "m", Key: []string{"k1", "k2"}, Value: []string{"v1"}}
			b := &knftables.Element{Map: "m", Key: []string{"k1", "k2"}, Value: []string{"v1"}}
			gomega.Expect(elementsEqual(a, b)).To(gomega.BeTrue())
		})

		ginkgo.It("returns false for different maps", func() {
			a := &knftables.Element{Map: "m1", Key: []string{"k"}, Value: []string{"v"}}
			b := &knftables.Element{Map: "m2", Key: []string{"k"}, Value: []string{"v"}}
			gomega.Expect(elementsEqual(a, b)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false for different key lengths", func() {
			a := &knftables.Element{Map: "m", Key: []string{"k1"}, Value: []string{"v"}}
			b := &knftables.Element{Map: "m", Key: []string{"k1", "k2"}, Value: []string{"v"}}
			gomega.Expect(elementsEqual(a, b)).To(gomega.BeFalse())
		})

		ginkgo.It("returns false for different values", func() {
			a := &knftables.Element{Map: "m", Key: []string{"k"}, Value: []string{"v1"}}
			b := &knftables.Element{Map: "m", Key: []string{"k"}, Value: []string{"v2"}}
			gomega.Expect(elementsEqual(a, b)).To(gomega.BeFalse())
		})
	})
})
