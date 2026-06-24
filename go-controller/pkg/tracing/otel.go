package tracing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const TraceparentAnnotation = "tracing.k8s.io/traceparent"
const UnknownSpanNamePrefix = "ovnkube.unknown"

type spanNamePrefixKey struct{}
type spansDisabledKey struct{}

type Config struct {
	Endpoint                       string
	Insecure                       bool
	ServiceName                    string
	SamplingRate                   float64
	PropagatedContextAnnotationKey string
	ExportTimeout                  time.Duration
	BatchTimeout                   time.Duration
	MaxExportBatchSize             int
	MaxQueueSize                   int
}

var (
	initOnce   sync.Once
	initErr    error
	tpGlobal   *sdktrace.TracerProvider
	shutdownFn func(context.Context) error

	annotationKey = TraceparentAnnotation
)

// Init initializes a global OTEL tracer provider once for the process.
// It is safe to call multiple times.
func Init(component string, cfg Config, attrs ...attribute.KeyValue) error {
	initOnce.Do(func() {
		if cfg.ServiceName == "" {
			cfg.ServiceName = "ovn-kubernetes"
		}
		if cfg.PropagatedContextAnnotationKey == "" {
			cfg.PropagatedContextAnnotationKey = TraceparentAnnotation
		}
		annotationKey = cfg.PropagatedContextAnnotationKey

		ctx := context.Background()
		exporterOpts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
		}
		if cfg.ExportTimeout > 0 {
			exporterOpts = append(exporterOpts, otlptracegrpc.WithTimeout(cfg.ExportTimeout))
		}
		exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
		if err != nil {
			initErr = fmt.Errorf("failed to create OTLP trace exporter: %w", err)
			return
		}

		resourceAttrs := append([]attribute.KeyValue{
			attribute.String("service.name", cfg.ServiceName),
			attribute.String("service.component", component),
		}, attrs...)

		res, err := sdkresource.New(ctx, sdkresource.WithAttributes(resourceAttrs...))
		if err != nil {
			initErr = fmt.Errorf("failed to build tracing resource: %w", err)
			return
		}

		batchOpts := []sdktrace.BatchSpanProcessorOption{}
		if cfg.BatchTimeout > 0 {
			batchOpts = append(batchOpts, sdktrace.WithBatchTimeout(cfg.BatchTimeout))
		}
		if cfg.ExportTimeout > 0 {
			batchOpts = append(batchOpts, sdktrace.WithExportTimeout(cfg.ExportTimeout))
		}
		if cfg.MaxExportBatchSize > 0 {
			batchOpts = append(batchOpts, sdktrace.WithMaxExportBatchSize(cfg.MaxExportBatchSize))
		}
		if cfg.MaxQueueSize > 0 {
			batchOpts = append(batchOpts, sdktrace.WithMaxQueueSize(cfg.MaxQueueSize))
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplingRate))),
			sdktrace.WithBatcher(exporter, batchOpts...),
			sdktrace.WithResource(res),
		)

		otel.SetTracerProvider(tp)
		tpGlobal = tp
		shutdownFn = tp.Shutdown
	})

	return initErr
}

// Shutdown flushes and shuts down the global OTEL tracer provider when initialized.
func Shutdown(ctx context.Context) error {
	if shutdownFn == nil {
		return nil
	}
	return shutdownFn(ctx)
}

func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

func ContextWithSpansDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, spansDisabledKey{}, true)
}

func SpansDisabled(ctx context.Context) bool {
	disabled, ok := ctx.Value(spansDisabledKey{}).(bool)
	return ok && disabled
}

func ContextWithSpanNamePrefix(ctx context.Context, prefix string) context.Context {
	if prefix == "" {
		return ctx
	}
	return context.WithValue(ctx, spanNamePrefixKey{}, prefix)
}

func SpanNamePrefixFromContext(ctx context.Context) string {
	prefix, ok := ctx.Value(spanNamePrefixKey{}).(string)
	if ok && prefix != "" {
		return prefix
	}
	return UnknownSpanNamePrefix
}

func SpanName(ctx context.Context, operation string) string {
	prefix := SpanNamePrefixFromContext(ctx)
	if prefix == "" {
		return operation
	}
	if operation == "" {
		return prefix
	}
	return prefix + "." + operation
}

func StartSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	prefix := SpanNamePrefixFromContext(ctx)
	if SpansDisabled(ctx) {
		return noop.NewTracerProvider().Tracer(prefix).Start(ctx, SpanName(ctx, operation))
	}
	return Tracer(prefix).Start(ctx, SpanName(ctx, operation))
}

// StartLinkedSpan starts a new reconcile root span and optionally links it to
// propagated trace context from Pod annotations.
func StartLinkedSpan(ctx context.Context, operation string, annotations map[string]string) (context.Context, trace.Span) {
	prefix := SpanNamePrefixFromContext(ctx)
	name := SpanName(ctx, operation)
	if SpansDisabled(ctx) {
		return noop.NewTracerProvider().Tracer(prefix).Start(ctx, name)
	}

	opts := []trace.SpanStartOption{trace.WithNewRoot()}
	if linkedSC, ok := SpanContextFromPodAnnotations(annotations); ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: linkedSC}))
	}
	return Tracer(prefix).Start(ctx, name, opts...)
}

// SpanContextFromPodAnnotations extracts propagated upstream span context from
// Pod annotations without setting it as the active remote parent.
func SpanContextFromPodAnnotations(annotations map[string]string) (trace.SpanContext, bool) {
	if len(annotations) == 0 {
		return trace.SpanContext{}, false
	}
	traceparent, ok := annotations[annotationKey]
	if !ok {
		return trace.SpanContext{}, false
	}
	sc, err := spanContextFromTraceparent(traceparent)
	if err != nil {
		return trace.SpanContext{}, false
	}
	return sc, true
}

func spanContextFromTraceparent(tp string) (trace.SpanContext, error) {
	parts := strings.Split(strings.TrimSpace(tp), "-")
	if len(parts) != 4 {
		return trace.SpanContext{}, errors.New("invalid traceparent format")
	}

	version := parts[0]
	traceIDHex := parts[1]
	parentSpanIDHex := parts[2]
	flagsHex := parts[3]

	if len(version) != 2 || len(traceIDHex) != 32 || len(parentSpanIDHex) != 16 || len(flagsHex) != 2 {
		return trace.SpanContext{}, errors.New("invalid traceparent field lengths")
	}
	if version == "ff" {
		return trace.SpanContext{}, errors.New("invalid traceparent version")
	}

	traceID, err := trace.TraceIDFromHex(traceIDHex)
	if err != nil {
		return trace.SpanContext{}, fmt.Errorf("invalid trace ID: %w", err)
	}
	parentSpanID, err := trace.SpanIDFromHex(parentSpanIDHex)
	if err != nil {
		return trace.SpanContext{}, fmt.Errorf("invalid parent span ID: %w", err)
	}
	flags, err := strconv.ParseUint(flagsHex, 16, 8)
	if err != nil {
		return trace.SpanContext{}, fmt.Errorf("invalid trace flags: %w", err)
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     parentSpanID,
		TraceFlags: trace.TraceFlags(flags),
		Remote:     true,
	})
	if !sc.IsValid() {
		return trace.SpanContext{}, errors.New("invalid span context")
	}
	return sc, nil
}

func PodAttrs(namespace, podName, podUID string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("k8s.pod.namespace", namespace),
		attribute.String("k8s.pod.name", podName),
		attribute.String("k8s.pod.uid", podUID),
	}
}

func ForceFlush(ctx context.Context) error {
	if tpGlobal == nil {
		return nil
	}
	return tpGlobal.ForceFlush(ctx)
}
