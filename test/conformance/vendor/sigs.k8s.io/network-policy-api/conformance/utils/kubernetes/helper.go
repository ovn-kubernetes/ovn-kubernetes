package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/network-policy-api/conformance/utils/config"
)

var (
	numStatufulSetReplicas int32 = 2
)

// RunCommandFromPod is a utility function that runs kubectl exec command on the Pod specified and returns the result.
func RunCommandFromPod(client k8sclient.Interface, kubeConfig *rest.Config, podNamespace, podName string, cmd []string) (stdout string, stderr string, err error) {
	// TODO(dyanngg): find a better way to derive container name
	containerName := podName[:len(podName)-1] + "client"
	request := client.CoreV1().RESTClient().Post().
		Namespace(podNamespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		Param("container", containerName).
		VersionedParams(&corev1.PodExecOptions{
			Command: cmd,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(kubeConfig, "POST", request.URL())
	if err != nil {
		return "", "", err
	}
	ctx, cancelFn := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancelFn()
	var stdoutB, stderrB bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdoutB,
		Stderr: &stderrB,
	}); err != nil {
		return stdoutB.String(), stderrB.String(), err
	}
	return stdoutB.String(), stderrB.String(), nil
}

// PokeServer is a utility function that checks if the connection from the provided clientPod in clientNamespace towards the targetHost:targetPort
// using the provided protocol can be established or not and returns the result based on if the expectation is shouldConnect or !shouldConnect
func PokeServer(t *testing.T, client k8sclient.Interface, kubeConfig *rest.Config, clientNamespace, clientPod, protocol, targetHost string, targetPort int32, timeout time.Duration, shouldConnect bool) bool {
	t.Helper()
	timeoutArg := fmt.Sprintf("--timeout=%v", timeout)
	protocolArg := fmt.Sprintf("--protocol=%s", protocol)
	ipPortArg := net.JoinHostPort(targetHost, fmt.Sprintf("%d", targetPort))

	// we leverage the dial connect from agnhost, that is already supporting multiple protocols
	connectCommand := strings.Split(fmt.Sprintf("/agnhost connect %s %s %s",
		timeoutArg,
		protocolArg,
		ipPortArg), " ")

	stdout, stderr, err := RunCommandFromPod(client, kubeConfig, clientNamespace, clientPod, connectCommand)
	// TODO(tssurya): See if we need to add a wait&retry mechanism to test connectivity
	// See https://github.com/kubernetes-sigs/network-policy-api/issues/108 for details.
	if err != nil && stderr == "" {
		// If err != nil and stderr == "", then it means this probe failed because of the command instead of connectivity.
		t.Logf("FAILED to execute command %s on pod %s/%s: %v", connectCommand, clientNamespace, clientPod, err.Error())
		return false
	}
	if shouldConnect && len(stderr) > 0 {
		t.Logf("FAILED Command was %s", connectCommand)
		t.Logf("Expected connection to succeed from %s/%s to %s, but instead it miserably failed. stderr: %v",
			clientNamespace, clientPod, targetHost, stderr)
		return false
	} else if !shouldConnect {
		if stdout == "" && stderr == "" {
			t.Logf("FAILED Command was %s", connectCommand)
			t.Logf("Expected connection to fail from %s/%s to %s, but instead it successfully connected.",
				clientNamespace, clientPod, targetHost)
			return false
		} else if !strings.Contains(stderr, "TIMEOUT") {
			t.Logf("FAILED Command was %s", connectCommand)
			// Other possible results include "REFUSED" for example, signaling the connection is rejected.
			t.Logf("Expected connection to be dropped from %s/%s to %s, but instead it returned a different status: %s",
				clientNamespace, clientPod, targetHost, stderr)
			return false
		}
	}
	return true
}

// NamespacesMustBeReady waits until all Pods are marked Ready. This will
// cause the test to halt if the specified timeout is exceeded.
func NamespacesMustBeReady(t *testing.T, c client.Client, timeoutConfig config.TimeoutConfig, namespaces []string, statefulSetNames []string) {
	t.Helper()

	waitErr := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, timeoutConfig.NamespacesMustBeReady, true, func(ctx context.Context) (bool, error) {
		for i, ns := range namespaces {
			statefulSet := &appsv1.StatefulSet{}
			statefulSetKey := types.NamespacedName{
				Namespace: ns,
				Name:      statefulSetNames[i],
			}
			if err := c.Get(ctx, statefulSetKey, statefulSet); err != nil {
				t.Errorf("Error retrieving StatefulSet %s from namespace %s: %v", statefulSetNames[i], ns, err)
			}
			if statefulSet.Status.ReadyReplicas != numStatufulSetReplicas {
				t.Logf("StatefulSet replicas in namespace %s not rolled out yet. %d/%d replicas are available.", ns, statefulSet.Status.ReadyReplicas, numStatufulSetReplicas)
				return false, nil
			}
		}
		t.Logf("Namespaces and Pods in %s namespaces are ready", strings.Join(namespaces, ", "))
		return true, nil
	})
	require.NoErrorf(t, waitErr, "error waiting for %s namespaces to be ready", strings.Join(namespaces, ", "))
}
