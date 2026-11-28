// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const podAnnotationReadbackTimeout = 5 * time.Second

type podAnnotationKey struct {
	namespace, name string
	uid             ktypes.UID
}

type podAnnotationOperation struct {
	token chan struct{}
	users int
	// Only the holder of token may access pod. It is the last server snapshot,
	// not an informer snapshot, so queued writers see preceding commits.
	pod *corev1.Pod
}

// PodAnnotationUpdateCoordinator serializes allocation updates across networks
// for a pod. Entries live only while callers are executing or waiting; no pod
// or allocation closure is retained after the last caller returns.
// Acquire this before entering allocation callbacks; never acquire it while
// holding an IPAM allocator lock. Different pods make progress independently.
// The zero value is not ready for use; use NewPodAnnotationUpdateCoordinator.
type PodAnnotationUpdateCoordinator struct {
	mu         sync.Mutex
	operations map[podAnnotationKey]*podAnnotationOperation
	client     kubernetes.Interface
}

// NewPodAnnotationUpdateCoordinator creates a coordinator shared by a manager's
// network allocators. Other processes remain protected by JSON patch tests.
func NewPodAnnotationUpdateCoordinator(client kubernetes.Interface) *PodAnnotationUpdateCoordinator {
	return &PodAnnotationUpdateCoordinator{client: client, operations: make(map[podAnnotationKey]*podAnnotationOperation)}
}

func (c *PodAnnotationUpdateCoordinator) acquire(ctx context.Context, key podAnnotationKey) (*podAnnotationOperation, func(), error) {
	c.mu.Lock()
	op := c.operations[key]
	if op == nil {
		op = &podAnnotationOperation{token: make(chan struct{}, 1)}
		op.token <- struct{}{}
		c.operations[key] = op
	}
	op.users++
	c.mu.Unlock()
	releaseUser := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		op.users--
		if op.users == 0 {
			delete(c.operations, key)
		}
	}
	select {
	case <-ctx.Done():
		releaseUser()
		return nil, nil, ctx.Err()
	case <-op.token:
		return op, func() { op.token <- struct{}{}; releaseUser() }, nil
	}
}

// Update performs the existing allocation/retry/rollback operation while
// coordinating its reads and writes. A batch of overlapping network writers
// starts with one live GET, then hands returned patch snapshots to waiters.
func (c *PodAnnotationUpdateCoordinator) Update(ctx context.Context, podLister listers.PodLister, k kube.Interface, pod *corev1.Pod, allocate AllocateToPodWithRollbackFunc) error {
	key := podAnnotationKey{namespace: pod.Namespace, name: pod.Name, uid: pod.UID}
	op, release, err := c.acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	reader := &coordinatedPodReader{PodLister: podLister, ctx: ctx, client: c.client, key: key, op: op}
	writer := &coordinatedPodWriter{Interface: k, reader: reader, kube: &kube.Kube{KClient: c.client}}
	return UpdatePodWithRetryOrRollback(ctx, reader, writer, pod, allocate)
}

type coordinatedPodReader struct {
	listers.PodLister
	ctx    context.Context
	client kubernetes.Interface
	key    podAnnotationKey
	op     *podAnnotationOperation
}

type coordinatedPodNamespaceReader struct {
	listers.PodNamespaceLister
	reader *coordinatedPodReader
}

func (r *coordinatedPodReader) Pods(namespace string) listers.PodNamespaceLister {
	return &coordinatedPodNamespaceReader{PodNamespaceLister: r.PodLister.Pods(namespace), reader: r}
}

func (r *coordinatedPodNamespaceReader) Get(_ string) (*corev1.Pod, error) {
	reader := r.reader
	if err := reader.ctx.Err(); err != nil {
		return nil, err
	}
	if reader.op.pod == nil {
		pod, err := reader.client.CoreV1().Pods(reader.key.namespace).Get(reader.ctx, reader.key.name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if pod.UID != reader.key.uid {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, reader.key.name)
		}
		reader.op.pod = pod
	}
	return reader.op.pod, nil
}

type coordinatedPodWriter struct {
	kube.Interface
	reader *coordinatedPodReader
	kube   *kube.Kube
}

func (w *coordinatedPodWriter) PatchPodStatusAnnotations(ctx context.Context, oldPod, newPod *corev1.Pod) error {
	updated, err := w.kube.PatchPodStatusAnnotationsWithContext(ctx, oldPod, newPod)
	if err == nil {
		w.reader.op.pod = updated
		return nil
	}
	w.reader.op.pod = nil
	// An error can follow a successful commit. Read back before invoking the
	// existing rollback path, including when the caller's request timed out.
	readbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), podAnnotationReadbackTimeout)
	defer cancel()
	current, readErr := w.reader.client.CoreV1().Pods(oldPod.Namespace).Get(readbackCtx, oldPod.Name, metav1.GetOptions{})
	if readErr == nil && current.UID == oldPod.UID {
		w.reader.op.pod = current
		if podAnnotationChangesPresent(oldPod.Annotations, newPod.Annotations, current.Annotations) {
			return nil
		}
	}
	return err
}

// Check only this operation's changes: an external writer may have added a
// sibling network entry after our commit but before the readback.
func podAnnotationChangesPresent(old, next, current map[string]string) bool {
	keys := maps.Clone(old)
	if keys == nil {
		keys = make(map[string]string)
	}
	maps.Copy(keys, next)
	for key := range keys {
		before, beforeOK := old[key]
		after, afterOK := next[key]
		if beforeOK == afterOK && before == after {
			continue
		}
		value, ok := current[key]
		if afterOK == ok && after == value {
			continue
		}
		if key != types.OvnPodAnnotationName || !afterOK || !ok {
			return false
		}
		var oldNetworks, newNetworks, currentNetworks map[string]json.RawMessage
		if beforeOK && json.Unmarshal([]byte(before), &oldNetworks) != nil {
			return false
		}
		if json.Unmarshal([]byte(after), &newNetworks) != nil || json.Unmarshal([]byte(value), &currentNetworks) != nil {
			return false
		}
		networks := maps.Clone(oldNetworks)
		if networks == nil {
			networks = make(map[string]json.RawMessage)
		}
		maps.Copy(networks, newNetworks)
		for network := range networks {
			if reflect.DeepEqual(oldNetworks[network], newNetworks[network]) {
				continue
			}
			// Unmarshal before comparing to tolerate JSON formatting/key order.
			_, expectedExists := newNetworks[network]
			_, actualExists := currentNetworks[network]
			if expectedExists != actualExists {
				return false
			}
			var expected, actual interface{}
			if data, exists := newNetworks[network]; exists {
				if json.Unmarshal(data, &expected) != nil {
					return false
				}
			}
			if data, exists := currentNetworks[network]; exists {
				if json.Unmarshal(data, &actual) != nil {
					return false
				}
			}
			if !reflect.DeepEqual(expected, actual) {
				return false
			}
		}
	}
	return true
}
