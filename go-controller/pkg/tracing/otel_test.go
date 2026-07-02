package tracing

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type endedSpanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *endedSpanRecorder) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (r *endedSpanRecorder) OnEnd(s sdktrace.ReadOnlySpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, s)
}

func (r *endedSpanRecorder) Shutdown(context.Context) error { return nil }

func (r *endedSpanRecorder) ForceFlush(context.Context) error { return nil }

func (r *endedSpanRecorder) ended() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(r.spans))
	copy(out, r.spans)
	return out
}

func TestSpanContextFromPodAnnotationsUsesTraceparentAnnotation(t *testing.T) {
	const traceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const upstreamSpanID = "bbbbbbbbbbbbbbbb"
	sc, ok := SpanContextFromPodAnnotations(map[string]string{
		TraceparentAnnotation: "00-" + traceID + "-" + upstreamSpanID + "-01",
	})
	if !ok {
		t.Fatalf("expected traceparent annotation to be parsed")
	}
	if !sc.IsValid() {
		t.Fatalf("expected valid span context")
	}
	if !sc.IsRemote() {
		t.Fatalf("expected span context to be marked remote")
	}
	if got := sc.TraceID().String(); got != traceID {
		t.Fatalf("expected trace ID %q, got %q", traceID, got)
	}
	if got := sc.SpanID().String(); got != upstreamSpanID {
		t.Fatalf("expected upstream span ID %q, got %q", upstreamSpanID, got)
	}
	if sc.TraceFlags()&trace.FlagsSampled == 0 {
		t.Fatalf("expected sampled trace flag")
	}
}

func TestSpanContextFromPodAnnotationsIgnoresMissingOrInvalidTraceparent(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
	}{
		{
			name:        "missing annotations",
			annotations: nil,
		},
		{
			name: "missing traceparent annotation",
			annotations: map[string]string{
				"other": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01",
			},
		},
		{
			name: "invalid traceparent annotation",
			annotations: map[string]string{
				TraceparentAnnotation: "invalid",
			},
		},
		{
			name: "zero trace ID",
			annotations: map[string]string{
				TraceparentAnnotation: "00-00000000000000000000000000000000-bbbbbbbbbbbbbbbb-01",
			},
		},
		{
			name: "zero parent span ID",
			annotations: map[string]string{
				TraceparentAnnotation: "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-0000000000000000-01",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := SpanContextFromPodAnnotations(tt.annotations)
			if ok {
				t.Fatalf("expected traceparent annotation to be ignored")
			}
		})
	}
}

func TestSpanContextFromPodAnnotationsUsesConfiguredAnnotationKey(t *testing.T) {
	oldAnnotationKey := annotationKey
	annotationKey = "example.com/traceparent"
	defer func() { annotationKey = oldAnnotationKey }()

	const traceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const upstreamSpanID = "bbbbbbbbbbbbbbbb"
	sc, ok := SpanContextFromPodAnnotations(map[string]string{
		TraceparentAnnotation:     "00-11111111111111111111111111111111-2222222222222222-01",
		"example.com/traceparent": "00-" + traceID + "-" + upstreamSpanID + "-01",
	})
	if !ok {
		t.Fatalf("expected configured traceparent annotation to be parsed")
	}
	if got := sc.TraceID().String(); got != traceID {
		t.Fatalf("expected trace ID %q, got %q", traceID, got)
	}
	if got := sc.SpanID().String(); got != upstreamSpanID {
		t.Fatalf("expected upstream span ID %q, got %q", upstreamSpanID, got)
	}
}

func TestStartTraceCreatesRootWithLink(t *testing.T) {
	const upstreamTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const upstreamSpanID = "bbbbbbbbbbbbbbbb"
	oldMode := spanRelationshipMode
	spanRelationshipMode = SpanRelationshipModeLinked
	defer func() { spanRelationshipMode = oldMode }()

	sr := &endedSpanRecorder{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	ctx := ContextWithSpanNamePrefix(context.Background(), "test.component")
	ctx = ContextWithOperation(ctx, OperationAdd)
	ctx, span := StartTrace(ctx, map[string]string{
		TraceparentAnnotation: "00-" + upstreamTraceID + "-" + upstreamSpanID + "-01",
	})
	span.End()

	spans := sr.ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := spans[0]
	if got.Parent().IsValid() {
		t.Fatalf("expected root span without parent, got parent trace ID %q", got.Parent().TraceID().String())
	}
	if got.SpanContext().TraceID().String() == upstreamTraceID {
		t.Fatalf("expected local root trace ID to differ from linked trace ID")
	}
	if len(got.Links()) != 1 {
		t.Fatalf("expected 1 span link, got %d", len(got.Links()))
	}
	link := got.Links()[0]
	if link.SpanContext.TraceID().String() != upstreamTraceID {
		t.Fatalf("expected linked trace ID %q, got %q", upstreamTraceID, link.SpanContext.TraceID().String())
	}
	if link.SpanContext.SpanID().String() != upstreamSpanID {
		t.Fatalf("expected linked span ID %q, got %q", upstreamSpanID, link.SpanContext.SpanID().String())
	}
	if trace.SpanFromContext(ctx) != span {
		t.Fatalf("expected returned context to carry the started span")
	}
}

func TestStartTraceParentChildModeCreatesChildSpan(t *testing.T) {
	const upstreamTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const upstreamSpanID = "bbbbbbbbbbbbbbbb"
	oldMode := spanRelationshipMode
	spanRelationshipMode = SpanRelationshipModeParent
	defer func() { spanRelationshipMode = oldMode }()

	sr := &endedSpanRecorder{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	ctx := ContextWithSpanNamePrefix(context.Background(), "test.component")
	ctx = ContextWithOperation(ctx, OperationAdd)
	ctx, span := StartTrace(ctx, map[string]string{
		TraceparentAnnotation: "00-" + upstreamTraceID + "-" + upstreamSpanID + "-01",
	})
	span.End()

	spans := sr.ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := spans[0]
	if !got.Parent().IsValid() {
		t.Fatalf("expected child span with remote parent")
	}
	if got.Parent().TraceID().String() != upstreamTraceID {
		t.Fatalf("expected parent trace ID %q, got %q", upstreamTraceID, got.Parent().TraceID().String())
	}
	if got.Parent().SpanID().String() != upstreamSpanID {
		t.Fatalf("expected parent span ID %q, got %q", upstreamSpanID, got.Parent().SpanID().String())
	}
	if got.SpanContext().TraceID().String() != upstreamTraceID {
		t.Fatalf("expected child span to remain in upstream trace %q, got %q", upstreamTraceID, got.SpanContext().TraceID().String())
	}
	if len(got.Links()) != 0 {
		t.Fatalf("expected no links in parent mode, got %d", len(got.Links()))
	}
	if trace.SpanFromContext(ctx) != span {
		t.Fatalf("expected returned context to carry the started span")
	}
}

func TestStartTraceWithoutAnnotationSuppressesSpanEmission(t *testing.T) {
	sr := &endedSpanRecorder{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	ctx := ContextWithSpanNamePrefix(context.Background(), "test.component")
	ctx = ContextWithOperation(ctx, OperationAdd)
	ctx, span := StartTrace(ctx, nil)
	if !SpansDisabled(ctx) {
		t.Fatalf("expected context to disable tracing when propagated context is missing")
	}
	_, child := StartSpan(ctx, "child")
	child.End()
	span.End()

	spans := sr.ended()
	if len(spans) != 0 {
		t.Fatalf("expected 0 spans, got %d", len(spans))
	}
}
