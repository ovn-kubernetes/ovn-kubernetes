package metrics

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
)

type fakeIPsecClient struct {
	output clientOutput
	mutex  sync.Mutex
}

func NewFakeIPsecClient(data clientOutput) fakeIPsecClient {
	return fakeIPsecClient{output: data}
}

func (c *fakeIPsecClient) FakeCall(...string) (string, string, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.output.stdout, c.output.stderr, c.output.err
}

func (c *fakeIPsecClient) ChangeOutput(newOutput clientOutput) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.output = newOutput
}

var _ = ginkgo.Describe("IPsec metrics", func() {
	var (
		localIP                              = "10.89.0.2"
		geneveTunnelName1                    = "ovn-8ebfff-0"
		remoteIP1                            = "10.89.0.3"
		geneveTunnelName2                    = "ovn-e9845d-0"
		remoteIP2                            = "10.89.0.4"
		stopChan                             chan struct{}
		wg                                   *sync.WaitGroup
		mockIPsecTunnelIKEChildSAStateMetric *mocks.GaugeMock
		ovsVsctlClient                       fakeOVSClient
		ipsecClient                          fakeIPsecClient
	)

	ginkgo.BeforeEach(func() {
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
		mockIPsecTunnelIKEChildSAStateMetric = mocks.NewGaugeMock()
		metricIPsecTunnelIKEChildSAState = mockIPsecTunnelIKEChildSAStateMetric
		ovsVsctlClient = NewFakeOVSClientWithSameOutput(clientOutput{})
		ipsecClient = NewFakeIPsecClient(clientOutput{})
		err := MonitorIPsecTunnelsState(stopChan, wg, ovsVsctlClient.FakeCall, ipsecClient.FakeCall)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "monitor ipsec tunnel state should not fail")
	})

	ginkgo.AfterEach(func() {
		ginkgo.By("clean up resources for the test")
		close(stopChan)
		wg.Wait()
		// Clean up ip xfrm state entry after every test.
		err := netlink.XfrmStateDel(getBaseState())
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error for ip xfrm state entry cleanup")
	})

	ginkgo.Context("Tunnel state", func() {
		ovntest.OnSupportedPlatformsIt("when geneve tunnels are present with IKE Child SAs established", func() {
			ginkgo.By("Emulate IKE Child SAs establishment for existing Geneve tunnels")
			ovsVsCtlCmdOutput := clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","up",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctlClient.ChangeOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput := clientOutput{
				stdout: `000 #13: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23543s; REPLACE in 24315s; newest; eroute owner; IKE SA #16; idle;
                             000 #16: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23975s; REPLACE in 24736s; newest; idle;
                             000 #11: "ovn-8ebfff-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23942s; REPLACE in 24212s; newest; eroute owner; IKE SA #16; idle;
                             000 #14: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23575s; REPLACE in 24525s; newest; idle;
                             000 #15: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23873s; REPLACE in 24623s; newest; eroute owner; IKE SA #14; idle;
                             000 #12: "ovn-e9845d-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 24034s; REPLACE in 24304s; newest; eroute owner; IKE SA #14; idle;`,
				stderr: "",
				err:    nil,
			}
			ipsecClient.ChangeOutput(ipsecCmdOutput)
			ginkgo.By("Trigger ip xfrm state event")
			err := netlink.XfrmStateAdd(getBaseState())
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is in established state")
			gomega.Eventually(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(20 * time.Second).Should(gomega.Equal(1))
		})
		ovntest.OnSupportedPlatformsIt("when geneve tunnels are present with IKE Child SAs not established for a tunnel", func() {
			ginkgo.By("Emulate IKE Child SA establishment failure for one of the existing Geneve tunnels")
			ovsVsCtlCmdOutput := clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","up",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctlClient.ChangeOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput := clientOutput{
				stdout: `000 #13: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23543s; REPLACE in 24315s; newest; eroute owner; IKE SA #16; idle;
                             000 #16: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23975s; REPLACE in 24736s; newest; idle;
                             000 #14: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23575s; REPLACE in 24525s; newest; idle;
                             000 #15: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23873s; REPLACE in 24623s; newest; eroute owner; IKE SA #14; idle;
                             000 #12: "ovn-e9845d-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 24034s; REPLACE in 24304s; newest; eroute owner; IKE SA #14; idle;`,
				stderr: "",
				err:    nil,
			}
			ipsecClient.ChangeOutput(ipsecCmdOutput)
			ginkgo.By("Trigger ip xfrm state event")
			err := netlink.XfrmStateAdd(getBaseState())
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is not in established state")
			gomega.Eventually(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(20 * time.Second).Should(gomega.Equal(0))
		})

		ovntest.OnSupportedPlatformsIt("check IKE Child SA establishment when geneve tunnel interface flapping scenario", func() {
			// Test no IPsec tunnel metrics when both Geneve tunnels are down.
			ginkgo.By("Emulate all the Geneve tunnels are down state")
			ovsVsCtlCmdOutput := clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","down",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","down",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctlClient.ChangeOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput := clientOutput{stdout: "", stderr: "", err: nil}
			ipsecClient.ChangeOutput(ipsecCmdOutput)
			ginkgo.By("Trigger ip xfrm state event")
			state := getBaseState()
			err := netlink.XfrmStateAdd(state)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is not in established state")
			gomega.Consistently(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))
			// Test correspoding IPsec tunnel metrics when one of the Geneve tunnels is down.
			ginkgo.By("Emulate atleast one of the Geneve tunnels is in down state")
			ovsVsCtlCmdOutput = clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","down",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctlClient.ChangeOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput = clientOutput{
				stdout: `000 #13: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23543s; REPLACE in 24315s; newest; eroute owner; IKE SA #16; idle;
                             000 #16: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23975s; REPLACE in 24736s; newest; idle;
                             000 #11: "ovn-8ebfff-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23942s; REPLACE in 24212s; newest; eroute owner; IKE SA #16; idle;`,
				stderr: "",
				err:    nil,
			}
			ipsecClient.ChangeOutput(ipsecCmdOutput)
			ginkgo.By("Trigger ip xfrm state event")
			err = netlink.XfrmStateDel(state)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry delete operation")
			ginkgo.By("Check IKE Child SA metric is not in established state")
			gomega.Consistently(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(5 * time.Second).Should(gomega.Equal(0))
			// Test correspoding IPsec tunnel metrics when Geneve tunnels are up.
			ginkgo.By("Emulate all of the Geneve tunnels are in up state")
			ovsVsCtlCmdOutput = clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","up",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctlClient.ChangeOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput = clientOutput{
				stdout: `000 #13: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23543s; REPLACE in 24315s; newest; eroute owner; IKE SA #16; idle;
                             000 #16: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23975s; REPLACE in 24736s; newest; idle;
                             000 #11: "ovn-8ebfff-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23942s; REPLACE in 24212s; newest; eroute owner; IKE SA #16; idle;
                             000 #14: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23575s; REPLACE in 24525s; newest; idle;
                             000 #15: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23873s; REPLACE in 24623s; newest; eroute owner; IKE SA #14; idle;
                             000 #12: "ovn-e9845d-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 24034s; REPLACE in 24304s; newest; eroute owner; IKE SA #14; idle;`,
				stderr: "",
				err:    nil,
			}
			ipsecClient.ChangeOutput(ipsecCmdOutput)
			ginkgo.By("Trigger ip xfrm state event")
			err = netlink.XfrmStateAdd(state)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error ip xfrm state entry add operation")
			ginkgo.By("Check IKE Child SA metric is in established state")
			gomega.Eventually(func() int {
				return int(mockIPsecTunnelIKEChildSAStateMetric.GetValue())
			}).WithTimeout(20 * time.Second).Should(gomega.Equal(1))
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
