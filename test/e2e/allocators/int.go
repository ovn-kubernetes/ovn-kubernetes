// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
)

var errAllocationFull = errors.New("allocation full")

var allocatorBackoff = wait.Backoff{
	Steps:    50,
	Duration: 100 * time.Millisecond,
}

const (
	configMapNamespace = "ovn-kubernetes-e2e-allocators"
)

// AllocateInt allocates and returns an integer that is not currently allocated
// by previous calls to this function under the same key. The allocation range
// is [1, max]. Within the range, the allocation is random. The allocation is
// backed up by a config map in the tested cluster. The purpose is for test
// cases to use this allocation directly or as a seed for other allocations that
// need to avoid collisions when running in parallel. This is useful when the
// orchestration handling the parallelization does not provide any context where
// this allocation can happen. De-allocate with DeallocateInt.
func AllocateInt(f *framework.Framework, key string, max int) (int, error) {
	if max < 1 {
		return 0, fmt.Errorf("max must be at least 1, got %d", max)
	}

	ctx := context.Background()

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: configMapNamespace}}
	_, err := f.ClientSet.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return 0, fmt.Errorf("failed to ensure allocator namespace: %w", err)
	}

	cm := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key}}
	client := f.ClientSet.CoreV1().ConfigMaps(configMapNamespace)

	_, err = client.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return 0, fmt.Errorf("failed to ensure allocator config map: %w", err)
	}

	var allocated int
	err = retry.OnError(
		allocatorBackoff,
		func(err error) bool {
			return apierrors.IsConflict(err) || errors.Is(err, errAllocationFull)
		},
		func() error {
			cm, err := client.Get(ctx, cm.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}

			if cm.Data == nil {
				cm.Data = map[string]string{}
			}

			if len(cm.Data) >= max {
				return fmt.Errorf("no free allocation in [1, %d] for key %q: %w", max, key, errAllocationFull)
			}

			start := rand.IntN(max)
			idx := 0
			for i := range max {
				idx = (start+i)%max + 1
				if _, ok := cm.Data[strconv.Itoa(idx)]; !ok {
					break
				}
			}
			cm.Data[strconv.Itoa(idx)] = ""
			_, err = client.Update(ctx, cm, metav1.UpdateOptions{})
			if err == nil {
				allocated = idx
			}
			return err
		},
	)

	if err == nil {
		framework.Logf("AllocateInt: %s=%d", key, allocated)
	}
	return allocated, err
}

// DeallocateInt deallocates an integer previously allocated with AllocateInt.
func DeallocateInt(f *framework.Framework, key string, index int) error {
	ctx := context.Background()
	client := f.ClientSet.CoreV1().ConfigMaps(configMapNamespace)

	return retry.RetryOnConflict(allocatorBackoff, func() error {
		cm, err := client.Get(ctx, key, metav1.GetOptions{})
		if err != nil {
			return err
		}

		delete(cm.Data, strconv.Itoa(index))
		_, err = client.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}
