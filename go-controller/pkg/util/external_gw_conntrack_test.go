// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestDeletePodConntrackEntriesForGW(t *testing.T) {
	podIP := "10.128.0.2"
	allowedMAC := [][]byte{{0x0a, 0x0b, 0x0c}}
	staleLabel := []byte{0xff, 0xee, 0xdd}

	matchingFlow := &netlink.ConntrackFlow{
		FamilyType: uint8(netlink.FAMILY_V4),
		Forward: netlink.IPTuple{
			SrcIP:    net.ParseIP(podIP),
			DstIP:    net.ParseIP("1.2.3.4"),
			SrcPort:  12345,
			DstPort:  80,
			Protocol: 6,
		},
		Reverse: netlink.IPTuple{
			SrcIP:    net.ParseIP("1.2.3.4"),
			DstIP:    net.ParseIP(podIP),
			SrcPort:  80,
			DstPort:  12345,
			Protocol: 6,
		},
		Labels: staleLabel,
	}
	otherPodFlow := *matchingFlow
	otherPodFlow.Forward.SrcIP = net.ParseIP("10.128.0.99")
	otherPodFlow.Reverse.DstIP = net.ParseIP("10.128.0.99")

	podAsServerFlow := *matchingFlow
	podAsServerFlow.Forward.SrcIP = matchingFlow.Forward.DstIP
	podAsServerFlow.Forward.DstIP = net.ParseIP(podIP)
	podAsServerFlow.Reverse.SrcIP = net.ParseIP(podIP)
	podAsServerFlow.Reverse.DstIP = matchingFlow.Forward.DstIP

	unlabeledCopy := func(flow netlink.ConntrackFlow) *netlink.ConntrackFlow {
		flow.Labels = nil
		return &flow
	}

	expectLabeledThenUnlabeledPodDeletes := func(t *testing.T, mockNetLinkOps *mocks.NetLinkOps, labeledPodFlow *netlink.ConntrackFlow) {
		t.Helper()
		unlabeledPodFlow := unlabeledCopy(*labeledPodFlow)
		remainingGWLabeled := *labeledPodFlow
		remainingGWLabeled.Labels = allowedMAC[0]

		mockNetLinkOps.On("ConntrackDeleteFilters",
			netlink.ConntrackTableType(netlink.ConntrackTable),
			mock.AnythingOfType("netlink.InetFamily"),
			mock.AnythingOfType("*netlink.ConntrackFilter"),
		).Run(func(args mock.Arguments) {
			filter := args.Get(2).(*netlink.ConntrackFilter)
			require.True(t, filter.MatchConntrackFlow(labeledPodFlow), "first delete (UnmatchLabels) must match the pod's stale labeled flow")
			require.False(t, filter.MatchConntrackFlow(unlabeledPodFlow), "first delete must not match the unlabeled copy")
			require.False(t, filter.MatchConntrackFlow(&remainingGWLabeled), "first delete must not match a remaining-gateway MAC label")
			require.False(t, filter.MatchConntrackFlow(&otherPodFlow), "first delete must not match another pod")
		}).Return(uint(1), nil).Once()

		mockNetLinkOps.On("ConntrackDeleteFilters",
			netlink.ConntrackTableType(netlink.ConntrackTable),
			mock.AnythingOfType("netlink.InetFamily"),
			mock.AnythingOfType("*netlink.ConntrackFilter"),
		).Run(func(args mock.Arguments) {
			filter := args.Get(2).(*netlink.ConntrackFilter)
			require.True(t, filter.MatchConntrackFlow(unlabeledPodFlow), "second delete (no MAC labels) must match the unlabeled copy of the same 5-tuple")
			require.True(t, filter.MatchConntrackFlow(labeledPodFlow), "second delete still matches the same pod IP 5-tuple")
			require.False(t, filter.MatchConntrackFlow(&otherPodFlow), "second delete must not match another pod")
		}).Return(uint(1), nil).Once()
	}

	t.Run("orig-src: removes podIP flows with unmatched MAC labels then unlabeled copies", func(t *testing.T) {
		mockNetLinkOps := new(mocks.NetLinkOps)
		origNetLinkOps := netLinkOps
		netLinkOps = mockNetLinkOps
		t.Cleanup(func() { netLinkOps = origNetLinkOps })

		expectLabeledThenUnlabeledPodDeletes(t, mockNetLinkOps, matchingFlow)
		errs := deletePodConntrackEntriesForGW(podIP, allowedMAC, []*netlink.ConntrackFlow{matchingFlow, &otherPodFlow}, netlink.ConntrackOrigSrcIP)
		require.Empty(t, errs)
		mockNetLinkOps.AssertExpectations(t)
		mockNetLinkOps.AssertNotCalled(t, "ConntrackTableList", mock.Anything, mock.Anything)
	})

	t.Run("orig-dst: removes podIP flows with unmatched MAC labels then unlabeled copies", func(t *testing.T) {
		mockNetLinkOps := new(mocks.NetLinkOps)
		origNetLinkOps := netLinkOps
		netLinkOps = mockNetLinkOps
		t.Cleanup(func() { netLinkOps = origNetLinkOps })

		expectLabeledThenUnlabeledPodDeletes(t, mockNetLinkOps, &podAsServerFlow)
		errs := deletePodConntrackEntriesForGW(podIP, allowedMAC, []*netlink.ConntrackFlow{&podAsServerFlow, &otherPodFlow}, netlink.ConntrackOrigDstIP)
		require.Empty(t, errs)
		mockNetLinkOps.AssertExpectations(t)
		mockNetLinkOps.AssertNotCalled(t, "ConntrackTableList", mock.Anything, mock.Anything)
	})

	t.Run("skips orig-dst delete when the listed flow only matches orig-src", func(t *testing.T) {
		mockNetLinkOps := new(mocks.NetLinkOps)
		origNetLinkOps := netLinkOps
		netLinkOps = mockNetLinkOps
		t.Cleanup(func() { netLinkOps = origNetLinkOps })

		errs := deletePodConntrackEntriesForGW(podIP, allowedMAC, []*netlink.ConntrackFlow{matchingFlow}, netlink.ConntrackOrigDstIP)
		require.Empty(t, errs)
		mockNetLinkOps.AssertNotCalled(t, "ConntrackDeleteFilters")
	})

	t.Run("skips delete when no listed flows match the pod IP on orig-src", func(t *testing.T) {
		mockNetLinkOps := new(mocks.NetLinkOps)
		origNetLinkOps := netLinkOps
		netLinkOps = mockNetLinkOps
		t.Cleanup(func() { netLinkOps = origNetLinkOps })

		errs := deletePodConntrackEntriesForGW(podIP, allowedMAC, []*netlink.ConntrackFlow{&otherPodFlow}, netlink.ConntrackOrigSrcIP)
		require.Empty(t, errs)
		mockNetLinkOps.AssertNotCalled(t, "ConntrackDeleteFilters")
	})

	t.Run("collects labeled and unlabeled delete errors", func(t *testing.T) {
		mockNetLinkOps := new(mocks.NetLinkOps)
		origNetLinkOps := netLinkOps
		netLinkOps = mockNetLinkOps
		t.Cleanup(func() { netLinkOps = origNetLinkOps })

		deleteFilterErrCall := ovntest.TestifyMockHelper{
			OnCallMethodName:    "ConntrackDeleteFilters",
			OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"},
			RetArgList:          []interface{}{uint(0), fmt.Errorf("delete failed")},
			CallTimes:           2,
		}
		ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, []ovntest.TestifyMockHelper{deleteFilterErrCall})

		errs := deletePodConntrackEntriesForGW(podIP, allowedMAC, []*netlink.ConntrackFlow{matchingFlow}, netlink.ConntrackOrigSrcIP)
		require.Len(t, errs, 2)
		mockNetLinkOps.AssertExpectations(t)
	})
}
