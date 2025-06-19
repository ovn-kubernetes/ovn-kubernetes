package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

type fakeIPsecClient struct {
	output clientOutput
	mutex  *sync.Mutex
}

func NewFakeIPsecClient(data clientOutput) fakeIPsecClient {
	return fakeIPsecClient{output: data, mutex: &sync.Mutex{}}
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
		localIP                    = "10.89.0.2"
		geneveTunnelName1          = "ovn-8ebfff-0"
		remoteIP1                  = "10.89.0.3"
		geneveTunnelName2          = "ovn-e9845d-0"
		remoteIP2                  = "10.89.0.4"
		stopChan                   chan struct{}
		wg                         *sync.WaitGroup
		nbClient                   client.Client
		mockIPsecTunnelStateMetric *mocks.GaugeVecMock
	)

	ginkgo.BeforeEach(func() {
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
		var err error
		nbClient, _, _, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{&nbdb.NBGlobal{Ipsec: true}}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		mockIPsecTunnelStateMetric = mocks.NewGaugeVecMock()
		metricIPsecTunnelState = mockIPsecTunnelStateMetric
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
		wg.Wait()
	})

	ginkgo.Context("Tunnel state", func() {
		ginkgo.It("when geneve tunnels are present with IKE Child SAs established", func() {
			ovsVsCtlCmdOutput := clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","up",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctl := NewFakeOVSClientWithSameOutput(ovsVsCtlCmdOutput)
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
			ipsec := NewFakeIPsecClient(ipsecCmdOutput)
			MontiorIPsecTunnelsState(stopChan, wg, nbClient, ovsVsctl.FakeCall, ipsec.FakeCall)
			gomega.Eventually(func() (bool, error) {
				state, err := mockIPsecTunnelStateMetric.GetValue(remoteIP1)
				if err != nil {
					return false, err
				}
				if state == 0 {
					return false, nil
				}
				state, err = mockIPsecTunnelStateMetric.GetValue(remoteIP2)
				if err != nil {
					return false, err
				}
				return state == 1, nil
			}).WithTimeout(10 * time.Second).Should(gomega.BeTrue())
		})
		ginkgo.It("when geneve tunnels are present with IKE Child SAs not established for a tunnel", func() {
			ovsVsCtlCmdOutput := clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","up",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctl := NewFakeOVSClientWithSameOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput := clientOutput{
				stdout: `000 #13: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23543s; REPLACE in 24315s; newest; eroute owner; IKE SA #16; idle;
                             000 #16: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23975s; REPLACE in 24736s; newest; idle;
                             000 #14: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23575s; REPLACE in 24525s; newest; idle;
                             000 #15: "ovn-e9845d-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23873s; REPLACE in 24623s; newest; eroute owner; IKE SA #14; idle;
                             000 #12: "ovn-e9845d-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 24034s; REPLACE in 24304s; newest; eroute owner; IKE SA #14; idle;`,
				stderr: "",
				err:    nil,
			}
			ipsec := NewFakeIPsecClient(ipsecCmdOutput)
			MontiorIPsecTunnelsState(stopChan, wg, nbClient, ovsVsctl.FakeCall, ipsec.FakeCall)
			gomega.Eventually(func() (bool, error) {
				state, err := mockIPsecTunnelStateMetric.GetValue(remoteIP1)
				if err != nil {
					return false, err
				}
				if state == 0 {
					return false, nil
				}
				state, err = mockIPsecTunnelStateMetric.GetValue(remoteIP2)
				if err != nil {
					return false, err
				}
				return state == 1, nil
			}).WithTimeout(20 * time.Second).Should(gomega.BeFalse())
		})

		ginkgo.It("check IKE Child SA establishment when geneve tunnel interface flapping scenario", func() {
			// Test no IPsec tunnel metrics when both Geneve tunnels are down.
			ovsVsCtlCmdOutput := clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","down",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","down",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctl := NewFakeOVSClientWithSameOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput := clientOutput{stdout: "", stderr: "", err: nil}
			ipsec := NewFakeIPsecClient(ipsecCmdOutput)
			MontiorIPsecTunnelsState(stopChan, wg, nbClient, ovsVsctl.FakeCall, ipsec.FakeCall)
			gomega.Consistently(func() error {
				_, err := mockIPsecTunnelStateMetric.GetValue(remoteIP1)
				if err != nil {
					return err
				}
				_, err = mockIPsecTunnelStateMetric.GetValue(remoteIP2)
				return err
			}).WithTimeout(20 * time.Second).Should(gomega.HaveOccurred())
			// Test correspoding IPsec tunnel metrics when one of the Geneve tunnels is down.
			ovsVsCtlCmdOutput = clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","down",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctl.ChangeOutput(ovsVsCtlCmdOutput)
			ipsecCmdOutput = clientOutput{
				stdout: `000 #13: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23543s; REPLACE in 24315s; newest; eroute owner; IKE SA #16; idle;
                             000 #16: "ovn-8ebfff-0-in-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 23975s; REPLACE in 24736s; newest; idle;
                             000 #11: "ovn-8ebfff-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 23942s; REPLACE in 24212s; newest; eroute owner; IKE SA #16; idle;`,
				stderr: "",
				err:    nil,
			}
			ipsec.ChangeOutput(ipsecCmdOutput)
			gomega.Eventually(func() (bool, error) {
				state, err := mockIPsecTunnelStateMetric.GetValue(remoteIP1)
				if err != nil {
					return false, err
				}
				return state == 1, nil
			}).WithTimeout(20 * time.Second).Should(gomega.BeTrue())
			gomega.Consistently(func() error {
				_, err := mockIPsecTunnelStateMetric.GetValue(remoteIP2)
				return err
			}).WithTimeout(20 * time.Second).Should(gomega.HaveOccurred())
			// Test correspoding IPsec tunnel metrics when Geneve tunnels are up.
			ovsVsCtlCmdOutput = clientOutput{
				stdout: fmt.Sprintf(`{"data":[["%[1]s","up","up",["map",[["csum","true"],["key","flow"],
				["local_ip","%[2]s"],["remote_ip","%[3]s"],["remote_name","8ebfffbe-3cac-4a67-8f42-c74b708f8cc6"]]]],
				["%[4]s","up","up",["map",[["csum","true"],["key","flow"],["local_ip","%[2]s"],["remote_ip","%[5]s"],
				["remote_name","e9845dc0-283f-4ea0-a9fa-4418bb6708c4"]]]]], "headings":["name","admin_state",
				"link_state","options"]}`, geneveTunnelName1, localIP, remoteIP1, geneveTunnelName2, remoteIP2),
				stderr: "",
				err:    nil,
			}
			ovsVsctl.ChangeOutput(ovsVsCtlCmdOutput)
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
			ipsec.ChangeOutput(ipsecCmdOutput)
			gomega.Eventually(func() (bool, error) {
				state, err := mockIPsecTunnelStateMetric.GetValue(remoteIP1)
				if err != nil {
					return false, err
				}
				if state == 0 {
					return false, nil
				}
				state, err = mockIPsecTunnelStateMetric.GetValue(remoteIP2)
				if err != nil {
					return false, err
				}
				return state == 1, nil
			}).WithTimeout(20 * time.Second).Should(gomega.BeTrue())
		})
	})
})
