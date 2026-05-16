// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gateway Init EXGW", func() {
	var (
		fexec *ovntest.FakeExec
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		fexec = ovntest.NewFakeExec()
		Expect(util.SetExec(fexec)).To(Succeed())
	})

	Context("interfaceForEXGW", func() {
		It("returns intfName if intfName is a bridge", func() {
			intfName := "br-ex"
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 br-exists " + intfName,
				Output: "",
			})

			result := interfaceForEXGW(intfName)
			Expect(result).To(Equal(intfName))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns bridgeName if bridge exists and port is correctly attached", func() {
			intfName := "eth1"
			bridgeName := "breth1"
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 br-exists " + intfName,
				Err: fmt.Errorf("br-exists failed"),
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 br-exists " + bridgeName,
				Output: "",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 port-to-br " + intfName,
				Output: bridgeName,
			})

			result := interfaceForEXGW(intfName)
			Expect(result).To(Equal(bridgeName))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns intfName if bridge exists but port is NOT attached (Issue #6111)", func() {
			intfName := "eth1"
			bridgeName := "breth1"
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 br-exists " + intfName,
				Err: fmt.Errorf("br-exists failed"),
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 br-exists " + bridgeName,
				Output: "",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 port-to-br " + intfName,
				Err: fmt.Errorf("ovs-vsctl: no port named eth1"),
			})

			result := interfaceForEXGW(intfName)
			Expect(result).To(Equal(intfName))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns intfName if bridge exists but port is attached to DIFFERENT bridge", func() {
			intfName := "eth1"
			bridgeName := "breth1"
			otherBridge := "br-other"
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 br-exists " + intfName,
				Err: fmt.Errorf("br-exists failed"),
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 br-exists " + bridgeName,
				Output: "",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 port-to-br " + intfName,
				Output: otherBridge,
			})

			result := interfaceForEXGW(intfName)
			Expect(result).To(Equal(intfName))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})

		It("returns intfName if neither intf nor bridge exists", func() {
			intfName := "eth1"
			bridgeName := "breth1"
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 br-exists " + intfName,
				Err: fmt.Errorf("br-exists failed"),
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 br-exists " + bridgeName,
				Err: fmt.Errorf("br-exists failed"),
			})

			result := interfaceForEXGW(intfName)
			Expect(result).To(Equal(intfName))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})
	})
})
