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
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

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
		ovnNodePod := getAnyOVNNodePod(f)
		traceparent, linkedSpanID := newTraceparent()

		podName := fmt.Sprintf("trace-e2e-%d", time.Now().UnixNano())
		pod := e2epod.NewAgnhostPod(f.Namespace.Name, podName, nil, nil, nil)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[traceContextAnnotationKey] = traceparent
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "agnhost-container" {
				pod.Spec.Containers[i].Command = []string{"sleep", "infinity"}
			}
		}

		expectedSpans := [][]string{
			{
				"ovnkube-node.network-controller.pod.update",
				"ovnkube-node.network-controller.pod.setup-local-pod-network",
				"ovnkube-node.network-controller.pod.add-logical-port",
				"ovnkube-node.network-controller.pod.update-pod-network-annotation",
				"ovnkube-node.network-controller.pod.patch-pod-status-annotations",
			},
			{
				"ovnkube-node.cni.pod.add",
				"ovnkube-node.cni.pod.configure-interface",
			},
			{
				"ovnkube-node.network-controller.pod.update",
				"ovnkube-node.network-controller.pod.setup-local-pod-network",
			},
			{
				"ovnkube-node.network-controller.pod.delete",
				"ovnkube-node.network-controller.pod.teardown-local-pod-network",
				"ovnkube-node.network-controller.pod.delete-logical-port",
			},
		}

		ginkgo.By("Creating a default-network pod with trace context: " + traceparent)
		createdPod := e2epod.NewPodClient(f).CreateSync(ctx, pod)

		ginkgo.By("Deleting the pod")
		e2epod.NewPodClient(f).DeleteSync(ctx, createdPod.Name, metav1.DeleteOptions{}, e2epod.DefaultPodDeletionTimeout)

		ginkgo.By("Querying Tempo and validating linked-mode add/delete spans")
		var allSpanRows [][]string
		gomega.Eventually(func() error {
			rootSpans, err := queryRootTraceSpansByLink(ovnNodePod, tempoNamespace, linkedSpanID, queryTimeRange)
			if err != nil {
				return err
			}
			if len(rootSpans) == 0 {
				return fmt.Errorf("no traces found linked to span id %s", linkedSpanID)
			}

			rows, err := collectSpanRows(ovnNodePod, tempoNamespace, rootSpans, queryTimeRange)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return fmt.Errorf("no span rows found in linked traces")
			}
			allSpanRows = rows

			return validateExpectedSpanRows(expectedSpans, rows)
		}, 1*time.Minute, 15*time.Second).Should(gomega.Succeed())

		framework.Logf("validated linked spans for traceparent=%s, matched span rows=%v", traceparent, allSpanRows)
	})
})

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

func getAnyOVNNodePod(f *framework.Framework) *v1.Pod {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
	})
	framework.ExpectNoError(err)
	gomega.Expect(pods.Items).NotTo(gomega.BeEmpty(), "expected at least one ovnkube-node pod")
	return &pods.Items[0]
}

func queryRootTraceSpansByLink(ovnNodePod *v1.Pod, _ string, linkedSpanID string, lookback time.Duration) ([]traceSpan, error) {
	now := time.Now().Unix()
	start := now - int64(lookback.Seconds())
	cmd := fmt.Sprintf(
		`curl -fsS -G %s/api/search --data-urlencode 'q={ link:spanID = "%s" }' --data-urlencode 'limit=500' --data-urlencode 'start=%d' --data-urlencode 'end=%d'`,
		tempoEndpoint,
		linkedSpanID,
		start,
		now,
	)
	stdout, err := e2epodoutput.RunHostCmdWithRetries(ovnNodePod.Namespace, ovnNodePod.Name, cmd, framework.Poll, 20*time.Second)
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

func collectSpanRows(ovnNodePod *v1.Pod, _ string, rootSpans []traceSpan, lookback time.Duration) ([][]string, error) {
	now := time.Now().Unix()
	start := now - int64(lookback.Seconds())

	rows := [][]string{}
	for _, rootSpan := range rootSpans {
		cmd := fmt.Sprintf(
			`curl -fsS -G %s/api/traces/%s --data-urlencode 'start=%d' --data-urlencode 'end=%d'`,
			tempoEndpoint,
			rootSpan.id,
			start,
			now,
		)
		stdout, err := e2epodoutput.RunHostCmdWithRetries(ovnNodePod.Namespace, ovnNodePod.Name, cmd, framework.Poll, 20*time.Second)
		if err != nil {
			return nil, err
		}

		framework.Logf("Tempo trace response for trace %s: %s", rootSpan.id, stdout)
		var payload any
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			return nil, fmt.Errorf("failed parsing Tempo trace response for trace %s: %w", rootSpan.id, err)
		}
		rows = append(rows, extractSpanRows(rootSpan, payload))
	}
	return rows, nil
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
