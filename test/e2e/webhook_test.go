// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/e2e/framework"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
)

var _ = ginkgo.Describe("Pod network annotation admission", func() {
	f := wrappedTestFramework("pod-network-admission")
	ginkgo.BeforeEach(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := f.ClientSet.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "ovn-kubernetes-admission-webhook-pod", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			ginkgo.Skip("requires the OVN pod admission webhook")
		}
		framework.ExpectNoError(err, "read OVN pod admission webhook configuration")
	})
	ginkgo.DescribeTable("rejects protected annotations on creation and updates", func(key string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		pods := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "annotation-test", Annotations: map[string]string{key: ""}}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "test", Image: images.AgnHost()}},
			// Keep the pod unscheduled so no allocator races with the admission checks.
			SchedulingGates: []corev1.PodSchedulingGate{{Name: "example.com/admission-test"}},
		}}
		_, err := pods.Create(ctx, pod, metav1.CreateOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred(), "creation with protected annotation %s must be rejected", key)
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("must not be supplied on pod creation"), "creation with %s must be rejected by the OVN webhook", key)
		pod.Annotations = nil
		_, err = pods.Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create pod without protected annotations")
		patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:"forged"}}}`, key))
		for _, subresource := range [][]string{nil, {"status"}} {
			_, err = pods.Patch(ctx, pod.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{}, subresource...)
			gomega.Expect(err).To(gomega.HaveOccurred(), "patch of protected annotation %s via subresource %v must be rejected", key, subresource)
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("not allowed to set"), "patch of %s via subresource %v must be rejected by the OVN webhook", key, subresource)
		}
		_, err = pods.Patch(ctx, pod.Name, ktypes.MergePatchType, []byte(`{"metadata":{"annotations":{"example.com/note":"allowed"}}}`), metav1.PatchOptions{})
		framework.ExpectNoError(err, "patch unrelated annotation on pod %s", pod.Name)
	},
		ginkgo.Entry("pod network allocation", OvnPodAnnotationName),
		ginkgo.Entry("DPU connection details", "k8s.ovn.org/dpu.connection-details"),
		ginkgo.Entry("DPU connection status", "k8s.ovn.org/dpu.connection-status"),
	)
})

var _ = ginkgo.Describe("NAD network name admission", feature.NetworkSegmentation, func() {
	const nadName = "name-test"
	f := wrappedTestFramework("nad-name-admission")
	var nadClient nadclient.K8sCniCncfIoV1Interface

	ovnConfig := func(networkName string) string {
		return fmt.Sprintf(`{"cniVersion":"1.0.0","name":%q,"type":"ovn-k8s-cni-overlay","topology":"layer2","subnets":"10.200.0.0/16","role":"secondary","netAttachDefName":%q}`,
			networkName, f.Namespace.Name+"/"+nadName)
	}
	expectAdmission := func(err error, rejection, action string) {
		if rejection == "" {
			framework.ExpectNoError(err, "%s must be admitted", action)
			return
		}
		gomega.Expect(err).To(gomega.HaveOccurred(), "%s must be rejected", action)
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(rejection), "%s must be rejected by the OVN webhook", action)
	}

	ginkgo.BeforeEach(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := f.ClientSet.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "ovn-kubernetes-admission-webhook-nad", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			ginkgo.Skip("requires the OVN NAD admission webhook")
		}
		framework.ExpectNoError(err, "read OVN NAD admission webhook configuration")
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "create NAD client")
	})

	// Requests are dry-run, so admission runs but no OVN network is created.
	ginkgo.DescribeTable("reserves user defined network names on creation and updates", func(networkName func(namespace string) string, rejection string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		nads := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name)
		name := networkName(f.Namespace.Name)
		dryRun := []string{metav1.DryRunAll}

		_, err := nads.Create(ctx, generateNetAttachDef(f.Namespace.Name, nadName, ovnConfig(name)), metav1.CreateOptions{DryRun: dryRun})
		expectAdmission(err, rejection, fmt.Sprintf("creation with network name %q", name))

		// A NAD of another CNI may use any name, until its config is changed to an OVN-Kubernetes one.
		foreignConfig := fmt.Sprintf(`{"cniVersion":"1.0.0","name":%q,"type":"bridge"}`, name)
		nad, err := nads.Create(ctx, generateNetAttachDef(f.Namespace.Name, nadName, foreignConfig), metav1.CreateOptions{})
		framework.ExpectNoError(err, "create NAD of another CNI with network name %q", name)
		nad.Spec.Config = ovnConfig(name)
		_, err = nads.Update(ctx, nad, metav1.UpdateOptions{DryRun: dryRun})
		expectAdmission(err, rejection, fmt.Sprintf("update to network name %q", name))
	},
		ginkgo.Entry("rejects a ClusterUserDefinedNetwork name",
			func(string) string { return "cluster_udn_test" }, "is reserved for ClusterUserDefinedNetwork"),
		ginkgo.Entry("rejects a UserDefinedNetwork name of another namespace",
			func(string) string { return "default_test" }, "is reserved for UserDefinedNetwork"),
		ginkgo.Entry("allows a UserDefinedNetwork name of its own namespace",
			func(namespace string) string { return namespace + "_test" }, ""),
		ginkgo.Entry("allows a name without a namespace prefix",
			func(string) string { return "test-network" }, ""),
	)
})
