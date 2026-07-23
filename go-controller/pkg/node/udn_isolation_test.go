// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/knftables"
	"sigs.k8s.io/yaml"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("nftPodElementsSet", func() {
	const setName = "test-set"
	var nft *knftables.Fake

	for _, composed := range []bool{false, true} {
		Context(fmt.Sprintf("composed=%v", composed), func() {
			composed := composed
			setType := "ipv4_addr"
			if composed {
				setType = "ipv4_addr . inet_proto . inet_service"
			}

			BeforeEach(func() {
				nft = nodenft.SetFakeNFTablesHelper()
				tx := nft.NewTransaction()
				tx.Add(&knftables.Set{
					Name: setName,
					Type: setType,
				})
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
			})

			getElem := func(ip string) string {
				if !composed {
					return ip
				}
				return fmt.Sprintf("%s . tcp . 8080", ip)
			}

			getElemKey := func(ip string) []string {
				if !composed {
					return []string{ip}
				}
				return []string{ip, "tcp", "8080"}
			}

			getExpectedDump := func(ips ...string) string {
				result := fmt.Sprintf(`add table inet ovn-kubernetes
add set inet ovn-kubernetes %s { type %s ; }
`, setName, setType)
				for _, ip := range ips {
					result += fmt.Sprintf("add element inet ovn-kubernetes %s { %s }\n", setName, getElem(ip))
				}
				return result
			}

			It("fullSync should update sets and build local cache", func() {
				tx := nft.NewTransaction()
				tx.Add(&knftables.Element{
					Set: setName,
					Key: getElemKey("1.1.1.1"),
				})
				tx.Add(&knftables.Element{
					Set: setName,
					Key: getElemKey("1.1.1.2"),
				})
				Expect(nft.Run(context.Background(), tx)).To(Succeed())

				s := newNFTPodElementsSet(setName, composed)
				newElems := map[string]sets.Set[string]{}
				newElems["ns1/pod1"] = sets.New(getElem("1.1.1.2"))
				newElems["ns1/pod2"] = sets.New(getElem("1.1.1.2"))
				newElems["ns2/pod1"] = sets.New(getElem("1.1.1.3"))
				Expect(s.fullSync(nft, newElems)).To(Succeed())
				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.2", "1.1.1.3"), nft.Dump())).To(Succeed())

				Expect(s.podElements).To(Equal(newElems))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.2"): sets.New("ns1/pod1", "ns1/pod2"),
					getElem("1.1.1.3"): sets.New("ns2/pod1"),
				}))
			})

			It("updatePodElements should update sets and local cache on pod add", func() {
				s := newNFTPodElementsSet(setName, false)
				tx := nft.NewTransaction()

				s.updatePodElementsTX("ns1/pod1", sets.New(getElem("1.1.1.1")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns1/pod1", sets.New(getElem("1.1.1.1")))

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1"),
				}))

				s.updatePodElementsTX("ns2/pod1", sets.New(getElem("1.1.1.1")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns2/pod1", sets.New(getElem("1.1.1.1")))

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
					"ns2/pod1": sets.New(getElem("1.1.1.1")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1", "ns2/pod1"),
				}))
			})

			It("updatePodElements should update sets and local cache on pod update", func() {
				s := newNFTPodElementsSet(setName, false)
				tx := nft.NewTransaction()
				//setup existing pod IPs
				newElems := map[string]sets.Set[string]{}
				newElems["ns1/pod1"] = sets.New(getElem("1.1.1.2"))
				newElems["ns1/pod2"] = sets.New(getElem("1.1.1.2"))
				newElems["ns2/pod1"] = sets.New(getElem("1.1.1.3"))
				Expect(s.fullSync(nft, newElems)).To(Succeed())

				s.updatePodElementsTX("ns1/pod1", sets.New(getElem("1.1.1.1")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns1/pod1", sets.New(getElem("1.1.1.1")))

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1", "1.1.1.2", "1.1.1.3"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
					"ns2/pod1": sets.New(getElem("1.1.1.3")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1"),
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
					getElem("1.1.1.3"): sets.New("ns2/pod1"),
				}))

				s.updatePodElementsTX("ns2/pod1", sets.New(getElem("1.1.1.4")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns2/pod1", sets.New(getElem("1.1.1.4")))
				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1", "1.1.1.2", "1.1.1.4"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
					"ns2/pod1": sets.New(getElem("1.1.1.4")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1"),
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
					getElem("1.1.1.4"): sets.New("ns2/pod1"),
				}))
			})

			It("updatePodElements should update sets and local cache on pod delete", func() {
				s := newNFTPodElementsSet(setName, false)
				tx := nft.NewTransaction()
				//setup existing pod IPs
				newElems := map[string]sets.Set[string]{}
				newElems["ns1/pod1"] = sets.New(getElem("1.1.1.2"))
				newElems["ns1/pod2"] = sets.New(getElem("1.1.1.2"))
				newElems["ns2/pod1"] = sets.New(getElem("1.1.1.3"))
				Expect(s.fullSync(nft, newElems)).To(Succeed())

				s.updatePodElementsTX("ns1/pod1", sets.New[string](), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns1/pod1", sets.New[string]())

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.2", "1.1.1.3"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
					"ns2/pod1": sets.New(getElem("1.1.1.3")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
					getElem("1.1.1.3"): sets.New("ns2/pod1"),
				}))

				s.updatePodElementsTX("ns2/pod1", sets.New[string](), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns2/pod1", sets.New[string]())
				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.2"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
				}))
			})
		})
	}
})

var _ = Describe("UDN Host isolation", func() {
	var (
		manager    *UDNHostIsolationManager
		wf         *factory.WatchFactory
		fakeClient *util.OVNNodeClientset
		nft        *knftables.Fake
	)

	const (
		nadNamespace     = "nad-namespace"
		defaultNamespace = "default-namespace"
	)

	getExpectedDump := func(v4ips, v6ips []string) string {
		result :=
			`add table inet ovn-kubernetes
add chain inet ovn-kubernetes udn-isolation { type filter hook output priority 0 ; comment "Host isolation for user defined networks" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv6)" ; }
add set inet ovn-kubernetes udn-open-ports-v4 { type ipv4_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-v6 { type ipv6_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv6)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks (IPv6)" ; }
add rule inet ovn-kubernetes udn-isolation ip daddr . meta l4proto . th dport @udn-open-ports-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-open-ports-icmp-v4 meta l4proto icmp accept
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 kubelet.slice/kubelet.service ip daddr @udn-pod-default-ips-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-pod-default-ips-v4 drop
add rule inet ovn-kubernetes udn-isolation ip6 daddr . meta l4proto . th dport @udn-open-ports-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-open-ports-icmp-v6 meta l4proto icmpv6 accept
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 kubelet.slice/kubelet.service ip6 daddr @udn-pod-default-ips-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-pod-default-ips-v6 drop
`
		for _, ip := range v4ips {
			result += fmt.Sprintf("add element inet ovn-kubernetes udn-pod-default-ips-v4 { %s }\n", ip)
		}
		for _, ip := range v6ips {
			result += fmt.Sprintf("add element inet ovn-kubernetes udn-pod-default-ips-v6 { %s }\n", ip)
		}

		return result
	}

	getExpectedDumpWithOpenPorts := func(v4ips, v6ips []string, openPorts map[string][]*util.OpenPort) string {
		result := getExpectedDump(v4ips, v6ips)
		for ip, openPorts := range openPorts {
			netIP := net.ParseIP(ip)
			for _, openPort := range openPorts {
				if openPort.Protocol == "icmp" {
					if netIP.To4() != nil {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-icmp-v4 { %s }\n", ip)
					} else {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-icmp-v6 { %s }\n", ip)
					}
				} else {
					if netIP.To4() != nil {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-v4 { %s . %s . %d }\n", ip, openPort.Protocol, *openPort.Port)
					} else {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-v6 { %s . %s . %d }\n", ip, openPort.Protocol, *openPort.Port)
					}
				}
			}
		}
		return result
	}

	start := func(objects ...runtime.Object) {
		fakeClient = util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())

		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), "node1", nil)

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Copy manager.Start() sequence, but using fake nft and without running systemd tracker
		manager.kubeletCgroupPath = "kubelet.slice/kubelet.service"
		nft = nodenft.SetFakeNFTablesHelper()
		manager.nft = nft
		err = manager.setupUDNIsolationFromHost()
		Expect(err).NotTo(HaveOccurred())
		err = controller.StartWithInitialSync(manager.podInitialSync, manager.podController)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.IPv4Mode = true
		config.IPv6Mode = true

		wf = nil
		manager = nil
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
		}
		if manager != nil {
			manager.Stop()
		}
	})

	It("correctly handles host-network and not ready pods on initial sync", func() {
		hostNetPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hostnet",
				UID:       ktypes.UID("hostnet"),
				Namespace: defaultNamespace,
			},
		}
		hostNetPod.Spec.HostNetwork = true
		notReadyPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "notready",
				UID:       ktypes.UID("notready"),
				Namespace: defaultNamespace,
			},
		}

		fakeClient = util.GetOVNClientset(hostNetPod, notReadyPod).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())
		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), "node1", nil)
		nft = nodenft.SetFakeNFTablesHelper()
		manager.nft = nft

		Expect(wf.Start()).To(Succeed())
		Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
		Expect(manager.podInitialSync()).To(Succeed())
	})

	It("correctly generates initial rules", func() {
		start()
		Expect(nft.Dump()).To(Equal(getExpectedDump(nil, nil)))
	})

	It("correctly handles not ready pods", func() {
		notReadyPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "notready",
				UID:       ktypes.UID("notready"),
				Namespace: defaultNamespace,
			},
		}
		fakeClient = util.GetOVNClientset(notReadyPod).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())
		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), "node1", nil)
		Expect(wf.Start()).To(Succeed())
		Expect(manager.reconcilePod(notReadyPod.Namespace + "/" + notReadyPod.Name)).To(Succeed())
	})

	Context("updates pod IPs", func() {
		It("on restart", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3"}))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
		})

		It("on pod add", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1"}, []string{"2014:100:200::1"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Create(context.TODO(),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())
			_, err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Create(context.TODO(),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3", "2014:100:200::3"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())
		})

		It("on pod delete", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.2"}))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Delete(context.TODO(), "pod3", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())

			err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Delete(context.TODO(), "pod2", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1"}, []string{"2014:100:200::1"}), nft.Dump())
			}).Should(Succeed())
		})
	})

	Context("updates open ports", func() {
		intRef := func(i int) *int {
			return &i
		}

		It("on restart", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}, util.OpenPort{Protocol: "tcp", Port: intRef(80)}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2"}, util.OpenPort{Protocol: "icmp"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3"}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
				"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
				"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				"1.1.1.2":         {{Protocol: "icmp"}},
			}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
		})

		It("on pod add", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}, util.OpenPort{Protocol: "tcp", Port: intRef(80)}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
				"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
				"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
			}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Create(context.TODO(),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}, util.OpenPort{Protocol: "icmp"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
					"1.1.1.2":         {{Protocol: "icmp"}},
					"2014:100:200::2": {{Protocol: "icmp"}},
				}), nft.Dump())
			}).Should(Succeed())
			_, err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Create(context.TODO(),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3", "2014:100:200::3"}, util.OpenPort{Protocol: "icmp"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
					"1.1.1.2":         {{Protocol: "icmp"}},
					"2014:100:200::2": {{Protocol: "icmp"}},
				}), nft.Dump())
			}).Should(Succeed())
		})

		It("on pod update", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, nil), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			pod, err := fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Get(context.TODO(),
				"pod1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pod.Annotations[util.UDNOpenPortsAnnotationName] = getOpenPortAnnotation([]util.OpenPort{{Protocol: "tcp", Port: intRef(80)}})[util.UDNOpenPortsAnnotationName]
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Update(context.TODO(),
				pod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				}), nft.Dump())
			}).Should(Succeed())
		})

		It("on pod delete", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}, util.OpenPort{Protocol: "tcp", Port: intRef(80)}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}, util.OpenPort{Protocol: "icmp"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.2"}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
				"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
				"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				"1.1.1.2":         {{Protocol: "icmp"}},
				"2014:100:200::2": {{Protocol: "icmp"}},
			}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Delete(context.TODO(), "pod3", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
					"1.1.1.2":         {{Protocol: "icmp"}},
					"2014:100:200::2": {{Protocol: "icmp"}},
				}), nft.Dump())
			}).Should(Succeed())

			err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Delete(context.TODO(), "pod2", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				}), nft.Dump())
			}).Should(Succeed())
		})
	})
})

func getOpenPortAnnotation(openPorts []util.OpenPort) map[string]string {
	res, err := yaml.Marshal(openPorts)
	Expect(err).NotTo(HaveOccurred())
	anno := make(map[string]string)
	if len(res) > 0 {
		anno[util.UDNOpenPortsAnnotationName] = string(res)
	}
	return anno
}

// newPodWithIPs creates a new pod with the given IPs, only filled for default network.
func newPodWithIPs(namespace, name string, primaryUDN bool, ips []string, openPorts ...util.OpenPort) *corev1.Pod {
	annoPodIPs := make([]string, len(ips))
	for i, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			annoPodIPs[i] = "\"" + ip + "/24\""
		} else {
			annoPodIPs[i] = "\"" + ip + "/64\""
		}
	}
	annotations := getOpenPortAnnotation(openPorts)
	role := types.NetworkRolePrimary
	if primaryUDN {
		role = types.NetworkRoleInfrastructure
	}
	annotations[types.OvnPodAnnotationName] = fmt.Sprintf(`{"default": {"role": "%s", "ip_addresses":[%s], "mac_address":"0a:58:0a:f4:02:03"}}`,
		role, strings.Join(annoPodIPs, ","))

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			UID:         ktypes.UID(name),
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

// nftRunFailer fails the first `failures` calls to Run, then delegates to the wrapped
// interface. It stands in for the kernel rejecting the socket cgroupv2 match.
type nftRunFailer struct {
	knftables.Interface
	failures int
}

func (f *nftRunFailer) Run(ctx context.Context, tx *knftables.Transaction) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("Could not process rule: No such file or directory")
	}
	return f.Interface.Run(ctx, tx)
}

var _ = Describe("UDN Host isolation setup", func() {
	const nodeName = "node1"

	var (
		manager  *UDNHostIsolationManager
		fakeNFT  *knftables.Fake
		recorder *record.FakeRecorder
	)

	expectedDump := func(kubeletCgroupPath string) string {
		result := `add table inet ovn-kubernetes
add chain inet ovn-kubernetes udn-isolation { type filter hook output priority 0 ; comment "Host isolation for user defined networks" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv6)" ; }
add set inet ovn-kubernetes udn-open-ports-v4 { type ipv4_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-v6 { type ipv6_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv6)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks (IPv6)" ; }
add rule inet ovn-kubernetes udn-isolation ip daddr . meta l4proto . th dport @udn-open-ports-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-open-ports-icmp-v4 meta l4proto icmp accept
`
		if kubeletCgroupPath != "" {
			result += fmt.Sprintf("add rule inet ovn-kubernetes udn-isolation socket cgroupv2 %s %s ip daddr @udn-pod-default-ips-v4 accept\n",
				cgroupv2Level(kubeletCgroupPath), kubeletCgroupPath)
		}
		result += `add rule inet ovn-kubernetes udn-isolation ip daddr @udn-pod-default-ips-v4 drop
add rule inet ovn-kubernetes udn-isolation ip6 daddr . meta l4proto . th dport @udn-open-ports-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-open-ports-icmp-v6 meta l4proto icmpv6 accept
`
		if kubeletCgroupPath != "" {
			result += fmt.Sprintf("add rule inet ovn-kubernetes udn-isolation socket cgroupv2 %s %s ip6 daddr @udn-pod-default-ips-v6 accept\n",
				cgroupv2Level(kubeletCgroupPath), kubeletCgroupPath)
		}
		result += "add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-pod-default-ips-v6 drop\n"
		return result
	}

	BeforeEach(func() {
		fakeNFT = nodenft.SetFakeNFTablesHelper()
		recorder = record.NewFakeRecorder(10)
		manager = &UDNHostIsolationManager{
			ipv4:     true,
			ipv6:     true,
			nodeName: nodeName,
			recorder: recorder,
			nft:      fakeNFT,
		}
	})

	Context("kubelet cgroup discovery", func() {
		It("finds the kubelet cgroup relative to the cgroup root", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "kubelet.slice", kubeletCgroupName), 0o755)).To(Succeed())

			Expect(manager.findKubeletCgroupPath(cgroupRoot)).To(Succeed())
			Expect(manager.kubeletCgroupPath).To(Equal(filepath.Join("kubelet.slice", kubeletCgroupName)))
		})

		It("fails when kubelet does not run under a systemd cgroup", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "podruntime", "kubelet"), 0o755)).To(Succeed())

			Expect(manager.findKubeletCgroupPath(cgroupRoot)).To(MatchError(ContainSubstring(kubeletCgroupName)))
			Expect(manager.kubeletCgroupPath).To(BeEmpty())
		})
	})

	Context("generic kubelet cgroup detection from /proc", func() {
		// writeProcEntry creates a fake /proc/<pid> with the given comm, exe target and
		// cgroup file, mirroring what findKubeletPID reads from a real procfs.
		writeProcEntry := func(procRoot, pid, comm, exe, cgroup string) {
			dir := filepath.Join(procRoot, pid)
			Expect(os.MkdirAll(dir, 0o755)).To(Succeed(), "create proc entry for pid "+pid)
			Expect(os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)).To(Succeed(), "write comm for pid "+pid)
			Expect(os.Symlink(exe, filepath.Join(dir, "exe"))).To(Succeed(), "write exe link for pid "+pid)
			Expect(os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644)).To(Succeed(), "write cgroup for pid "+pid)
		}

		It("resolves the kubelet cgroup from its process, systemd layout", func() {
			procRoot := GinkgoT().TempDir()
			cgroupRoot := GinkgoT().TempDir()
			writeProcEntry(procRoot, "1234", "kubelet", "/usr/bin/kubelet", "0::/kubelet.slice/kubelet.service\n")
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "kubelet.slice", "kubelet.service"), 0o755)).To(Succeed(), "create cgroup dir")

			path, err := detectKubeletCgroupPath(procRoot, cgroupRoot)
			Expect(err).NotTo(HaveOccurred(), "detect kubelet cgroup")
			Expect(path).To(Equal("kubelet.slice/kubelet.service"), "resolved cgroup path")
		})

		It("resolves the kubelet cgroup when kubelet is not managed by systemd", func() {
			procRoot := GinkgoT().TempDir()
			cgroupRoot := GinkgoT().TempDir()
			writeProcEntry(procRoot, "1", "systemd-decoy", "/usr/lib/systemd/systemd", "0::/init.scope\n")
			writeProcEntry(procRoot, "42", "kubelet", "/usr/local/bin/kubelet", "0::/podruntime/kubelet\n")
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "podruntime", "kubelet"), 0o755)).To(Succeed(), "create cgroup dir")

			path, err := detectKubeletCgroupPath(procRoot, cgroupRoot)
			Expect(err).NotTo(HaveOccurred(), "detect kubelet cgroup")
			Expect(path).To(Equal("podruntime/kubelet"), "resolved cgroup path")
		})

		It("fails when there is no kubelet process", func() {
			procRoot := GinkgoT().TempDir()
			cgroupRoot := GinkgoT().TempDir()
			writeProcEntry(procRoot, "7", "containerd", "/usr/bin/containerd", "0::/system.slice/containerd.service\n")

			_, err := detectKubeletCgroupPath(procRoot, cgroupRoot)
			Expect(err).To(MatchError(ContainSubstring("no \"kubelet\" process")), "no kubelet process error")
		})

		It("ignores a process that only renamed its comm to kubelet", func() {
			procRoot := GinkgoT().TempDir()
			cgroupRoot := GinkgoT().TempDir()
			// comm says kubelet, but the executable is something else, so it must not
			// be trusted for the isolation match.
			writeProcEntry(procRoot, "8", "kubelet", "/tmp/evil/imposter", "0::/evil/imposter\n")

			_, err := detectKubeletCgroupPath(procRoot, cgroupRoot)
			Expect(err).To(MatchError(ContainSubstring("no \"kubelet\" process")), "imposter rejected")
		})

		It("fails when kubelet runs at the cgroup root", func() {
			procRoot := GinkgoT().TempDir()
			cgroupRoot := GinkgoT().TempDir()
			writeProcEntry(procRoot, "99", "kubelet", "/usr/bin/kubelet", "0::/\n")

			_, err := detectKubeletCgroupPath(procRoot, cgroupRoot)
			Expect(err).To(HaveOccurred(), "cgroup root rejected")
		})

		It("fails when the detected cgroup is not present under the cgroup root", func() {
			procRoot := GinkgoT().TempDir()
			cgroupRoot := GinkgoT().TempDir()
			// kubelet reports a path that does not exist under the cgroup root the
			// nftables match resolves against, e.g. a mismatched cgroup namespace.
			writeProcEntry(procRoot, "5", "kubelet", "/usr/bin/kubelet", "0::/podruntime/kubelet\n")

			_, err := detectKubeletCgroupPath(procRoot, cgroupRoot)
			Expect(err).To(MatchError(ContainSubstring("not present under")), "cgroup-namespace mismatch rejected")
		})
	})

	DescribeTable("derives the socket cgroupv2 match level from the cgroup path depth",
		func(cgroupPath, expectedLevel string) {
			Expect(cgroupv2Level(cgroupPath)).To(Equal(expectedLevel))
		},
		Entry("systemd layout", "kubelet.slice/kubelet.service", "level 2"),
		Entry("single component", "kubelet", "level 1"),
		Entry("nested layout", "runtime.slice/kubelet.slice/kubelet.service", "level 3"),
	)

	Context("nftables setup", func() {
		It("allows kubelet to reach UDN pods when the kubelet cgroup is known", func() {
			manager.kubeletCgroupPath = "kubelet.slice/kubelet.service"

			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump("kubelet.slice/kubelet.service"), fakeNFT.Dump())).To(Succeed())
		})

		It("still isolates UDN pods when the kubelet cgroup is unknown", func() {
			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump(""), fakeNFT.Dump())).To(Succeed())
		})

		It("drops the kubelet rule when it cannot be loaded, and reports it on the node", func() {
			manager.kubeletCgroupPath = "kubelet.slice/kubelet.service"
			manager.nft = &nftRunFailer{Interface: fakeNFT, failures: 1}

			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(manager.kubeletCgroupPath).To(BeEmpty())
			Expect(nodenft.MatchNFTRules(expectedDump(""), fakeNFT.Dump())).To(Succeed())
			Expect(recorder.Events).To(Receive(ContainSubstring("UDNKubeletProbesNotSupported")))
		})

		It("fails when the isolation rules cannot be loaded at all", func() {
			manager.nft = &nftRunFailer{Interface: fakeNFT, failures: 2}

			Expect(manager.setupUDNIsolationFromHost()).NotTo(Succeed())
		})
	})
})
