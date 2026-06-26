// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics/mocks"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type fakeOVSAppctlClient struct {
	output clientOutput
	mutex  sync.Mutex
}

func NewFakeOVSAppctlClient(data clientOutput) fakeOVSAppctlClient {
	return fakeOVSAppctlClient{output: data}
}

func (c *fakeOVSAppctlClient) FakeCall(_ int, _ ...string) (string, string, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.output.stdout, c.output.stderr, c.output.err
}

func (c *fakeOVSAppctlClient) ChangeOutput(newOutput clientOutput) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.output = newOutput
}

// buildIPsecOutput generates IPsec tunnel status output in JSON format for the given tunnel names.
// For each tunnel name, it generates entries for both inbound (-in-1) and outbound (-out-1) tunnels.
// The output format matches 'ovs-appctl -t ovs-monitor-ipsec ipsec/status' command output.
func buildIPsecOutput(tunnelNames ...string) string {
	var output strings.Builder
	connectionID := 10

	output.WriteString("{")
	for i, tunnelName := range tunnelNames {
		if i > 0 {
			output.WriteString(", ")
		}

		connectionID++
		inConnID := connectionID
		connectionID++
		outConnID := connectionID

		// Format: {'tunnel-name': {'tunnel-name-in-1': 'esp info', 'tunnel-name-out-1': 'esp info'}}
		fmt.Fprintf(&output, "'%s': {'%s-in-1': '#%d: \"%s-in-1\" esp.e591db0c@192.168.111.21 esp.88eb5d94@192.168.111.24 Traffic: ESPin=5MB ESPout=0B ESPmax=2^63B ', '%s-out-1': '#%d: \"%s-out-1\" esp.c16822ce@192.168.111.21 esp.e2b67192@192.168.111.24 Traffic: ESPin=0B ESPout=8MB ESPmax=2^63B '}",
			tunnelName, tunnelName, inConnID, tunnelName, tunnelName, outConnID, tunnelName)
	}
	output.WriteString("}")

	return output.String()
}

// buildOVSTestData creates the OVS DB test data structure with geneve tunnel interfaces.
func buildOVSTestData(tunnelNames ...string) []libovsdbtest.TestData {
	var data []libovsdbtest.TestData
	var portUUIDs []string

	// Create interfaces and ports for each tunnel
	for _, tunnelName := range tunnelNames {
		intfUUID := buildUUID()
		portUUID := buildUUID()

		data = append(data,
			&vswitchd.Interface{
				UUID: intfUUID,
				Name: tunnelName,
				Type: "geneve",
			},
			&vswitchd.Port{
				UUID:       portUUID,
				Name:       tunnelName,
				Interfaces: []string{intfUUID},
			},
		)
		portUUIDs = append(portUUIDs, portUUID)
	}

	// Create br-int bridge with all ports
	brUUID := buildUUID()
	data = append(data, &vswitchd.Bridge{
		UUID:  brUUID,
		Name:  "br-int",
		Ports: portUUIDs,
	})

	// Create OpenvSwitch object
	data = append(data, &vswitchd.OpenvSwitch{
		UUID:    buildUUID(),
		Bridges: []string{brUUID},
	})

	return data
}

var _ = ginkgo.Describe("IPsec metrics", func() {
	var (
		geneveTunnelName1                    = "ovn-8ebfff-0"
		geneveTunnelName2                    = "ovn-e9845d-0"
		stopChan                             chan struct{}
		wg                                   *sync.WaitGroup
		mockIPsecTunnelIKEChildSAStateMetric *mocks.GaugeMock
		ovsClient                            libovsdbclient.Client
		libovsdbCtx                          *libovsdbtest.Context
		ovsAppctlClient                      fakeOVSAppctlClient
	)

	ginkgo.BeforeEach(func() {
		var err error
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
		prevMetric := metricIPsecTunnelIKEChildSAState
		ginkgo.DeferCleanup(func() {
			metricIPsecTunnelIKEChildSAState = prevMetric
		})
		mockIPsecTunnelIKEChildSAStateMetric = mocks.NewGaugeMock()
		metricIPsecTunnelIKEChildSAState = mockIPsecTunnelIKEChildSAStateMetric

		// Create libovsdb test harness with geneve tunnel interfaces
		ovsClient, libovsdbCtx, err = libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: buildOVSTestData(geneveTunnelName1, geneveTunnelName2),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create libovsdb test harness")
		ginkgo.DeferCleanup(libovsdbCtx.Cleanup)

		ovsAppctlClient = NewFakeOVSAppctlClient(clientOutput{})
		err = MonitorIPsecTunnelsState(stopChan, wg, ovsClient, ovsAppctlClient.FakeCall)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "monitor ipsec tunnel state should not fail")
	})

	ginkgo.AfterEach(func() {
		ginkgo.By("clean up resources for the test")
		close(stopChan)
		wg.Wait()
		// Clean up ip xfrm state entry after every test.
		err := netlink.XfrmStateDel(getBaseState())
		if err != nil && !errors.Is(err, syscall.ENOENT) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error for ip xfrm state entry cleanup")
		}
	})

	ginkgo.Context("Tunnel state", func() {
		ovntest.OnSupportedPlatformsIt("when geneve tunnels are present with IKE Child SAs established", func() {
			ginkgo.By("Emulate IKE Child SAs establishment for existing Geneve tunnels")
			ovsAppctlClient.ChangeOutput(clientOutput{
				stdout: buildIPsecOutput(geneveTunnelName1, geneveTunnelName2),
			})
			ginkgo.By("Trigger ip xfrm state event")
			err := netlink.XfrmStateAdd(getBaseState())
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is in established state")
			gomega.Eventually(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(10 * time.Second).Should(gomega.Equal(1))
		})
		ovntest.OnSupportedPlatformsIt("when geneve tunnels are present with IKE Child SAs not established for a tunnel", func() {
			ginkgo.By("Emulate IKE Child SA establishment failure for one of the existing Geneve tunnels")
			// Only tunnel2 is fully established; tunnel1 is missing all IPsec SAs
			ovsAppctlClient.ChangeOutput(clientOutput{
				stdout: buildIPsecOutput(geneveTunnelName2),
			})
			ginkgo.By("Trigger ip xfrm state event")
			err := netlink.XfrmStateAdd(getBaseState())
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is not in established state")
			gomega.Eventually(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(10 * time.Second).Should(gomega.Equal(0))
		})

		ovntest.OnSupportedPlatformsIt("check IKE Child SA establishment when IPsec tunnels change", func() {
			// Test no IPsec tunnel metrics when no IPsec tunnels are established.
			ginkgo.By("Emulate scenario with no IPsec tunnels established")
			ovsAppctlClient.ChangeOutput(clientOutput{}) // Empty output = no tunnels
			ginkgo.By("Trigger ip xfrm state event")
			state := getBaseState()
			err := netlink.XfrmStateAdd(state)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is not in established state")
			gomega.Consistently(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))
			// Test corresponding IPsec tunnel metrics when only one tunnel is established.
			ginkgo.By("Emulate IPsec tunnel established for only one Geneve tunnel")
			ovsAppctlClient.ChangeOutput(clientOutput{
				stdout: buildIPsecOutput(geneveTunnelName1),
			})
			ginkgo.By("Trigger ip xfrm state event")
			err = netlink.XfrmStateDel(state)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry delete operation")
			ginkgo.By("Check IKE Child SA metric is not in established state")
			gomega.Consistently(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))
			// Test corresponding IPsec tunnel metrics when all tunnels are established.
			ginkgo.By("Emulate IPsec tunnels established for all Geneve tunnels")
			ovsAppctlClient.ChangeOutput(clientOutput{
				stdout: buildIPsecOutput(geneveTunnelName1, geneveTunnelName2),
			})
			ginkgo.By("Trigger ip xfrm state event")
			err = netlink.XfrmStateAdd(state)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is in established state")
			gomega.Eventually(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(10 * time.Second).Should(gomega.Equal(1))
		})
	})
})

func getBaseState() *netlink.XfrmState {
	return &netlink.XfrmState{
		// Force 4 byte notation for the IPv4 addresses
		Src:   net.ParseIP("127.0.0.1").To4(),
		Dst:   net.ParseIP("127.0.0.2").To4(),
		Proto: netlink.XFRM_PROTO_ESP,
		Mode:  netlink.XFRM_MODE_TUNNEL,
		Spi:   1,
		Auth: &netlink.XfrmStateAlgo{
			Name: "hmac(sha256)",
			Key:  []byte("abcdefghijklmnopqrstuvwzyzABCDEF"),
		},
		Crypt: &netlink.XfrmStateAlgo{
			Name: "cbc(aes)",
			Key:  []byte("abcdefghijklmnopqrstuvwzyzABCDEF"),
		},
		Mark: &netlink.XfrmMark{
			Value: 0x12340000,
			Mask:  0xffff0000,
		},
	}
}
