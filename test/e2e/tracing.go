// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

const (
	traceContextAnnotationKey = "tracing.k8s.io/traceparent"
	tempoNamespace            = "monitoring"
	tempoEndpoint             = "http://tempo.monitoring.svc.cluster.local:3200"
	queryTimeRange            = 10 * time.Minute
	nodeNameResourceAttr      = "k8s.node.name"

	// a secondary network is what makes cluster-manager allocate IPs, and
	// therefore emit spans; the default network is handled by ovnkube-node
	layer2NADName = "tracing-layer2"
	layer2CIDR    = "172.30.0.0/24"
)

type queryResponse struct {
	Traces []struct {
		TraceID       string `json:"traceID"`
		RootTraceName string `json:"rootTraceName"`
	} `json:"traces"`
}

type traceSpan struct {
	id   string
	name string
}

var _ = ginkgo.Describe("Tracing", feature.Tracing, func() {
	f := wrappedTestFramework("tracing")

	ginkgo.It("creates and deletes a pod and emits linked spans", func() {
		ctx := context.Background()
		queryPod := createTempoQueryPod(ctx, f)
		traceparent, linkedSpanID := newTraceparent()

		netConfig := createLayer2NAD(ctx, f)
		podName := fmt.Sprintf("trace-e2e-%d", time.Now().UnixNano())
		pod := e2epod.NewAgnhostPod(f.Namespace.Name, podName, nil, nil, nil)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[traceContextAnnotationKey] = traceparent
		for key, value := range networkSelectionElements(nadapi.NetworkSelectionElement{
			Name:      netConfig.name,
			Namespace: netConfig.namespace,
		}) {
			pod.Annotations[key] = value
		}
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "agnhost-container" {
				pod.Spec.Containers[i].Command = []string{"sleep", "infinity"}
			}
		}

		expectedSpans := [][]string{
			{
				"ovnkube-node.network-controller.pod.update",
				"ovnkube-node.network-controller.pod.setup-local-pod-network",
				"ovnkube-node.network-controller.pod.update-pod-network-annotation",
			},
			{
				"ovnkube-node.cni.pod.add",
				"ovnkube-node.cni.pod.configure-interface",
			},
			{
				"ovnkube-node.network-controller.pod.delete",
				"ovnkube-node.network-controller.pod.teardown-local-pod-network",
			},
			{
				"ovnkube-cluster-manager.network-controller.pod.update",
				"ovnkube-cluster-manager.network-controller.pod.allocate-pod-network",
				"ovnkube-cluster-manager.network-controller.pod.update-pod-network-annotation",
			},
			{
				"ovnkube-cluster-manager.network-controller.pod.delete",
				"ovnkube-cluster-manager.network-controller.pod.release-pod-network",
			},
		}

		ginkgo.By("Creating a pod with trace context: " + traceparent)
		createdPod := e2epod.NewPodClient(f).CreateSync(ctx, pod)

		ginkgo.By("Deleting the pod")
		e2epod.NewPodClient(f).DeleteSync(ctx, createdPod.Name, metav1.DeleteOptions{}, e2epod.DefaultPodDeletionTimeout)

		ginkgo.By("Querying Tempo and validating linked-mode add/delete spans")
		var allSpanRows [][]string
		var spansByNode map[string][]string
		gomega.Eventually(func() error {
			rootSpans, err := queryRootTraceSpansByLink(queryPod, linkedSpanID, queryTimeRange)
			if err != nil {
				return err
			}
			if len(rootSpans) == 0 {
				return fmt.Errorf("no traces found linked to span id %s", linkedSpanID)
			}

			rows, nodeSpans, err := collectSpanRows(queryPod, rootSpans, queryTimeRange)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return fmt.Errorf("no span rows found in linked traces")
			}
			allSpanRows = rows
			spansByNode = nodeSpans

			return validateExpectedSpanRows(expectedSpans, rows)
		}, 1*time.Minute, 15*time.Second).Should(gomega.Succeed())

		ginkgo.By("Validating only the node running the pod emitted spans")
		framework.ExpectNoError(validateSpanNodes(createdPod.Spec.NodeName, spansByNode))

		framework.Logf("validated linked spans for traceparent=%s, matched span rows=%v", traceparent, allSpanRows)
	})
})

// createLayer2NAD attaches a secondary layer2 network to the test namespace.
func createLayer2NAD(ctx context.Context, f *framework.Framework) networkAttachmentConfig {
	nadClient, err := nadclient.NewForConfig(f.ClientConfig())
	framework.ExpectNoError(err)

	netConfig := newNetworkAttachmentConfig(networkAttachmentConfigParams{
		name:      layer2NADName,
		namespace: f.Namespace.Name,
		topology:  "layer2",
		cidr:      layer2CIDR,
	})

	ginkgo.By("Creating a layer2 NAD in namespace " + netConfig.namespace)
	_, err = nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
		ctx,
		generateNAD(netConfig, f.ClientSet),
		metav1.CreateOptions{},
	)
	framework.ExpectNoError(err)

	return netConfig
}

func createTempoQueryPod(ctx context.Context, f *framework.Framework) *v1.Pod {
	podName := fmt.Sprintf("tempo-query-%d", time.Now().UnixNano())
	pod := e2epod.NewAgnhostPod(tempoNamespace, podName, nil, nil, nil)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "agnhost-container" {
			pod.Spec.Containers[i].Command = []string{"sleep", "infinity"}
		}
	}

	ginkgo.By("Creating a Tempo query pod in monitoring namespace")
	createdPod, err := f.ClientSet.CoreV1().Pods(tempoNamespace).Create(ctx, pod, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	ginkgo.DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e2epod.DefaultPodDeletionTimeout)
		defer cancel()

		framework.ExpectNoError(f.ClientSet.CoreV1().Pods(tempoNamespace).Delete(cleanupCtx, createdPod.Name, metav1.DeleteOptions{}))
		framework.ExpectNoError(e2epod.WaitForPodNotFoundInNamespace(
			cleanupCtx, f.ClientSet, createdPod.Name, createdPod.Namespace, e2epod.DefaultPodDeletionTimeout))
	})

	framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, createdPod.Name, createdPod.Namespace))

	return createdPod
}

func newTraceparent() (traceparent string, spanID string) {
	traceID := randomHex(16)
	spanID = randomHex(8)
	return fmt.Sprintf("00-%s-%s-01", traceID, spanID), spanID
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	framework.ExpectNoError(err)
	return hex.EncodeToString(b)
}

func queryRootTraceSpansByLink(queryPod *v1.Pod, linkedSpanID string, lookback time.Duration) ([]traceSpan, error) {
	now := time.Now().Unix()
	start := now - int64(lookback.Seconds())
	cmd := fmt.Sprintf(
		`curl -fsS -G %s/api/search --data-urlencode 'q={ link:spanID = "%s" }' --data-urlencode 'limit=500' --data-urlencode 'start=%d' --data-urlencode 'end=%d'`,
		tempoEndpoint,
		linkedSpanID,
		start,
		now,
	)
	stdout, err := e2epodoutput.RunHostCmdWithRetries(queryPod.Namespace, queryPod.Name, cmd, framework.Poll, 20*time.Second)
	if err != nil {
		return nil, err
	}

	var resp queryResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		return nil, fmt.Errorf("failed parsing Tempo search response: %w, body=%q", err, stdout)
	}

	rootSpans := make([]traceSpan, 0, len(resp.Traces))
	seen := map[string]struct{}{}
	for _, t := range resp.Traces {
		if t.TraceID == "" {
			continue
		}
		if _, ok := seen[t.TraceID]; ok {
			continue
		}
		seen[t.TraceID] = struct{}{}
		rootSpans = append(rootSpans, traceSpan{id: t.TraceID, name: t.RootTraceName})
	}
	return rootSpans, nil
}

// collectSpanRows returns the span names of every fetched trace, plus the span
// names grouped by the node that emitted them.
func collectSpanRows(queryPod *v1.Pod, rootSpans []traceSpan, lookback time.Duration) ([][]string, map[string][]string, error) {
	now := time.Now().Unix()
	start := now - int64(lookback.Seconds())

	rows := [][]string{}
	spansByNode := map[string][]string{}
	for _, rootSpan := range rootSpans {
		cmd := fmt.Sprintf(
			`curl -fsS -G %s/api/traces/%s --data-urlencode 'start=%d' --data-urlencode 'end=%d'`,
			tempoEndpoint,
			rootSpan.id,
			start,
			now,
		)
		stdout, err := e2epodoutput.RunHostCmdWithRetries(queryPod.Namespace, queryPod.Name, cmd, framework.Poll, 20*time.Second)
		if err != nil {
			return nil, nil, err
		}

		framework.Logf("Tempo trace response for trace %s: %s", rootSpan.id, stdout)
		var payload any
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			return nil, nil, fmt.Errorf("failed parsing Tempo trace response for trace %s: %w", rootSpan.id, err)
		}
		rows = append(rows, extractSpanRows(rootSpan, payload))
		collectSpansByNode(payload, spansByNode)
	}
	return rows, spansByNode, nil
}

// validateSpanNodes reports an error if a node other than the one running the
// pod emitted spans. Pods are only traced by the node they are scheduled on, so
// a span from anywhere else means a remote reconcile was traced.
func validateSpanNodes(podNode string, spansByNode map[string][]string) error {
	if podNode == "" {
		return fmt.Errorf("pod has no node assigned")
	}
	for node, spans := range spansByNode {
		if node != podNode {
			return fmt.Errorf("node %q emitted spans %v, expected spans only from pod node %q", node, spans, podNode)
		}
	}
	if len(spansByNode[podNode]) == 0 {
		return fmt.Errorf("no spans carry resource attribute %s=%s", nodeNameResourceAttr, podNode)
	}
	return nil
}

// collectSpansByNode groups span names by the node that emitted them. Batches
// without a node name belong to components that are not node scoped, such as
// ovnkube-cluster-manager, and are left out.
func collectSpansByNode(v any, spansByNode map[string][]string) {
	for _, batch := range tempoResourceBatches(v) {
		node := resourceAttr(batch["resource"], nodeNameResourceAttr)
		if node == "" {
			continue
		}
		spanNames := []string{}
		walkTempoSpans(batch["scopeSpans"], &spanNames)
		spansByNode[node] = append(spansByNode[node], spanNames...)
	}
}

// tempoResourceBatches returns the per-resource span batches of a trace. Tempo
// names the list "batches", plain OTLP JSON names it "resourceSpans".
func tempoResourceBatches(v any) []map[string]any {
	payload, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	batches := []map[string]any{}
	for _, key := range []string{"batches", "resourceSpans"} {
		list, _ := payload[key].([]any)
		for _, entry := range list {
			if batch, ok := entry.(map[string]any); ok {
				batches = append(batches, batch)
			}
		}
	}
	return batches
}

func resourceAttr(v any, key string) string {
	resource, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	attrs, _ := resource["attributes"].([]any)
	for _, entry := range attrs {
		attr, ok := entry.(map[string]any)
		if !ok || stringFromAny(attr["key"]) != key {
			continue
		}
		value, ok := attr["value"].(map[string]any)
		if !ok {
			return ""
		}
		return stringFromAny(value["stringValue"])
	}
	return ""
}

func validateExpectedSpanRows(expectedSpans, actualRows [][]string) error {
	usedRows := make([]bool, len(actualRows))

	for _, expectedRow := range expectedSpans {
		if len(expectedRow) == 0 {
			continue
		}
		rootName := expectedRow[0]
		expectedChildren := expectedRow[1:]

		matched := false
		for i, actualRow := range actualRows {
			if usedRows[i] || len(actualRow) == 0 || actualRow[0] != rootName {
				continue
			}
			if rowContainsAll(actualRow, expectedChildren) {
				usedRows[i] = true
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("no matching span row for expected=%v, actualRows=%v", expectedRow, actualRows)
		}
	}
	return nil
}

func rowContainsAll(actualRow, expectedItems []string) bool {
	matched, err := gomega.ContainElements(expectedItems).Match(actualRow)
	return err == nil && matched
}

func extractSpanRows(rootSpan traceSpan, v any) []string {
	row := []string{rootSpan.name}
	spanNames := []string{}
	walkTempoSpans(v, &spanNames)
	row = append(row, spanNames...)
	return row
}

func walkTempoSpans(v any, spanNames *[]string) {
	switch t := v.(type) {
	case map[string]any:
		spanID := stringFromAny(t["spanId"])
		if spanID == "" {
			spanID = stringFromAny(t["spanID"])
		}
		if spanID != "" {
			name := stringFromAny(t["name"])
			if name == "" {
				name = stringFromAny(t["operationName"])
			}
			if name != "" {
				*spanNames = append(*spanNames, name)
			}
		}
		for _, child := range t {
			walkTempoSpans(child, spanNames)
		}
	case []any:
		for _, child := range t {
			walkTempoSpans(child, spanNames)
		}
	}
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
