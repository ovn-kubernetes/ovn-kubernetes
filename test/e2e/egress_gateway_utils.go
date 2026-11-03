// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/utils/client-go/ktesting"
)

func deployClientDS(ctx context.Context, f *framework.Framework, name string, numNodes int) *appsv1.DaemonSet {
	ginkgo.GinkgoHelper()
	clientLabels := map[string]string{"app": name}
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: clientLabels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: clientLabels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: clientLabels},
				Spec: v1.PodSpec{
					Containers:  []v1.Container{{Name: "client", Image: images.AgnHost(), Command: []string{"/agnhost", "pause"}}},
					Tolerations: []v1.Toleration{{Operator: v1.TolerationOpExists}},
				},
			},
		},
	}
	daemonSet, err := f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Create(ctx, daemonSet, metav1.CreateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "create client DS %s", name)
	err = wait.PollUntilContextTimeout(ctx, time.Second, f.Timeouts.PodStart, true, func(ctx context.Context) (bool, error) {
		current, err := f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Get(ctx, daemonSet.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if int(current.Status.DesiredNumberScheduled) != numNodes || int(current.Status.NumberAvailable) != numNodes {
			return false, nil
		}
		daemonSet = current
		return true, nil
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "wait for client DS %s on %d nodes", name, numNodes)
	return daemonSet
}

func setupIPInfoServerInKind(ipFamilies []v1.IPFamily) []string {
	ginkgo.GinkgoHelper()

	ginkgo.By("Creating an IP-info server on the primary provider network")
	providerCtx := infraprovider.Get().NewTestContext()
	primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	server, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
		Name: "ipinfo-server", Image: images.AgnHost(),
		Network: primaryNetwork, ExtPort: 8080,
		CmdArgs: getAgnHostHTTPPortBindCMDArgs(8080),
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	framework.Logf("IP-info container %s: IPv4=%s IPv6=%s", server.Name, server.GetIPv4(), server.GetIPv6())

	var ret []string
	for _, family := range ipFamilies {
		ip := server.GetIPv4()
		if family == v1.IPv6Protocol {
			ip = server.GetIPv6()
		}
		gomega.Expect(ip).NotTo(gomega.BeEmpty(), "IP-info server needs an %s address", family)
		ret = append(ret, "http://"+net.JoinHostPort(ip, server.GetPortStr())+"/clientip")
	}
	return ret
}

func fetchProbeSourceIPs(tCtx ktesting.TContext, daemonSet *appsv1.DaemonSet, endpoints []string) ([]string, error) {
	pods, err := tCtx.Client().CoreV1().Pods(daemonSet.Namespace).List(tCtx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(daemonSet.Spec.Selector),
	})
	if err != nil {
		return nil, fmt.Errorf("list probe pods in namespace %s: %w", daemonSet.Namespace, err)
	}
	if len(pods.Items) != int(daemonSet.Status.DesiredNumberScheduled) {
		return nil, fmt.Errorf("expected %d probe pods, got %d", daemonSet.Status.DesiredNumberScheduled, len(pods.Items))
	}
	got := make([]string, 0, len(pods.Items)*len(endpoints))
	for _, pod := range pods.Items {
		for _, endpoint := range endpoints {
			out, stderr, err := e2epod.Exec(tCtx, e2epod.ExecOptions{
				Namespace: daemonSet.Namespace, PodName: pod.Name, ContainerName: "client",
				CaptureStdout: true, CaptureStderr: true,
				Command: []string{"curl", "--fail", "--silent", "--show-error", "--noproxy", "*", "--max-time", "5", "--connect-timeout", "3", endpoint},
			})
			if err != nil {
				return nil, fmt.Errorf("node %s pod %s: HTTP request to %s failed (%s): %w", pod.Spec.NodeName, pod.Name, endpoint, stderr, err)
			}
			source := strings.TrimSpace(out)
			if host, _, err := net.SplitHostPort(source); err == nil {
				source = host
			}
			ip := net.ParseIP(source)
			if ip == nil {
				return nil, fmt.Errorf("node %s pod %s: invalid client IP response %q from %s", pod.Spec.NodeName, pod.Name, out, endpoint)
			}
			if source = ip.String(); !slices.Contains(got, source) {
				got = append(got, source)
			}
		}
	}
	return got, nil
}
