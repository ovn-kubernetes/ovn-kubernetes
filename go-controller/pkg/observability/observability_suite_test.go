// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestObservabilityManager(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Observability Manager Suite")
}

// cacheSyncTimeout bounds how long a spec waits for an informer cache to sync. Fake clients
// sync near-instantly, so reaching this means something is actually broken.
const cacheSyncTimeout = 30 * time.Second

// expectCacheSync waits for the given informer caches to sync, and fails the spec if they do
// not. The wait is bounded: cache.WaitForCacheSync only returns early when its stop channel
// closes, and specs close theirs in a defer that cannot run while the spec is still blocked
// here, so an informer that never syncs would otherwise hang the whole suite instead of
// failing this assertion.
func expectCacheSync(synced ...cache.InformerSynced) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), cacheSyncTimeout)
	defer cancel()
	Expect(cache.WaitForCacheSync(ctx.Done(), synced...)).To(BeTrue(), "informer caches failed to sync")
}
