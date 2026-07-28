package retry

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/tracing"
)

// ctxRecordingHandler records the context handed to AddResource so that tests
// can assert on what was propagated to the handler.
type ctxRecordingHandler struct {
	DefaultEventHandler
	obj    interface{}
	addCtx context.Context
}

func (h *ctxRecordingHandler) AddResource(ctx context.Context, _ interface{}, _ bool) error {
	h.addCtx = ctx
	return nil
}

func (h *ctxRecordingHandler) UpdateResource(_, _ interface{}, _ bool) error { return nil }

func (h *ctxRecordingHandler) DeleteResource(_, _ interface{}) error { return nil }

func (h *ctxRecordingHandler) GetResourceFromInformerCache(_ string) (interface{}, error) {
	return h.obj, nil
}

func (h *ctxRecordingHandler) FilterOutResource(_ interface{}) bool { return false }

// TestResourceRetryPreservesInitialListSpanSuppression asserts that an add that
// originated from the informer's initial list stays untraced when it is retried,
// so a pre-existing object whose startup add failed does not emit spans later.
func TestResourceRetryPreservesInitialListSpanSuppression(t *testing.T) {
	tests := []struct {
		name             string
		isInInitialList  bool
		expectSuppressed bool
	}{
		{
			name:             "add from initial list stays suppressed on retry",
			isInInitialList:  true,
			expectSuppressed: true,
		},
		{
			name:             "add from a regular event stays traced on retry",
			isInInitialList:  false,
			expectSuppressed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "namespace"},
			}
			handler := &ctxRecordingHandler{obj: pod}
			r := &RetryFramework{
				retryEntries: syncmap.NewSyncMap[*retryObjEntry](),
				retryChan:    make(chan struct{}, 1),
				doneWg:       &sync.WaitGroup{},
				ResourceHandler: &ResourceHandler{
					ObjType:      reflect.TypeOf(&corev1.Pod{}),
					EventHandler: handler,
				},
			}

			key := "namespace/pod"
			r.retryEntries.LoadOrStore(key, &retryObjEntry{
				newObj: pod,
				// well in the past so the backoff timer does not defer the retry
				timeStamp:       time.Now().Add(-time.Hour),
				backoff:         initialBackoff,
				isInInitialList: tt.isInInitialList,
			})

			r.resourceRetry(key, time.Now())

			if handler.addCtx == nil {
				t.Fatal("expected AddResource to be called on retry")
			}
			if got := tracing.SpansDisabledFromContext(handler.addCtx); got != tt.expectSuppressed {
				t.Errorf("expected spans disabled on retry to be %v but it was %v",
					tt.expectSuppressed, got)
			}
		})
	}
}
