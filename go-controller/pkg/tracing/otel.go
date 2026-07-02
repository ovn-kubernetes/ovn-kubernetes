// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

const TraceparentAnnotation = "tracing.k8s.io/traceparent"

// traceparentHeader is the W3C trace context header name the SDK propagator
// reads from a carrier.
const traceparentHeader = "traceparent"

// traceparentPropagator parses propagated W3C trace context.
var traceparentPropagator = propagation.TraceContext{}

// noopTracer serves every suppressed path. The noop implementation ignores both
// the tracer name and the span name, so naming the span is wasted work.
var noopTracer = noop.NewTracerProvider().Tracer("")

const UnknownSpanNamePrefix = "ovnkube.unknown"

type Operation string

const (
	OperationAdd     Operation = "add"
	OperationUpdate  Operation = "update"
	OperationDelete  Operation = "delete"
	OperationUnknown Operation = "unknown"
)

type spanNamePrefixKey struct{}
type spansDisabledKey struct{}
type retryLoopKey struct{}
type operationKey struct{}

var (
	initOnce   sync.Once
	initErr    error
	shutdownFn func(context.Context) error

	traceparentAnnotationKey = TraceparentAnnotation
	spanRelationshipMode     = SpanRelationshipModeLinked
)

// Init initializes a global OTEL tracer provider once for the process. It is
// safe to call multiple times; later calls are no-ops returning the first
// result. Initialization is deliberately not retried: it only fails on invalid
// configuration, which retrying cannot fix, whereas transient connectivity
// failures are handled by the OTLP gRPC client's own reconnect logic.
func Init(component string, cfg config.TracingConfig, attrs ...attribute.KeyValue) error {
	initOnce.Do(func() {
		if cfg.ServiceName == "" {
			cfg.ServiceName = "ovn-kubernetes"
		}
		if cfg.PropagatedContextAnnotationKey == "" {
			cfg.PropagatedContextAnnotationKey = TraceparentAnnotation
		}
		if cfg.PropagatedContextMode == "" {
			cfg.PropagatedContextMode = SpanRelationshipModeLinked
		}
		traceparentAnnotationKey = cfg.PropagatedContextAnnotationKey
		spanRelationshipMode = cfg.PropagatedContextMode

		if spanRelationshipMode == SpanRelationshipModeParent && cfg.SamplingRate != 1.0 {
			klog.Warningf("Tracing sampling-rate %v is ignored with propagated-context-mode=%q",
				cfg.SamplingRate, spanRelationshipMode)
		}

		ctx := context.Background()
		exporterOpts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
		}
		if !cfg.UseTLS {
			exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
		} else {
			tlsCreds, err := tracingTLSCredentials(cfg)
			if err != nil {
				initErr = fmt.Errorf("failed to build OTLP TLS config: %w", err)
				return
			}
			exporterOpts = append(exporterOpts, otlptracegrpc.WithTLSCredentials(tlsCreds))
		}
		if cfg.ExportTimeout > 0 {
			exporterOpts = append(exporterOpts, otlptracegrpc.WithTimeout(time.Duration(cfg.ExportTimeout)*time.Second))
		}
		exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
		if err != nil {
			initErr = fmt.Errorf("failed to create OTLP trace exporter: %w", err)
			return
		}

		resourceAttrs := append([]attribute.KeyValue{
			attribute.String(ResourceAttrServiceName, cfg.ServiceName),
			attribute.String(ResourceAttrServiceComponent, component),
		}, attrs...)

		res, err := sdkresource.New(ctx, sdkresource.WithAttributes(resourceAttrs...))
		if err != nil {
			_ = exporter.Shutdown(ctx)
			initErr = fmt.Errorf("failed to build tracing resource: %w", err)
			return
		}

		batchOpts := []sdktrace.BatchSpanProcessorOption{}
		if cfg.BatchTimeout > 0 {
			batchOpts = append(batchOpts, sdktrace.WithBatchTimeout(time.Duration(cfg.BatchTimeout)*time.Second))
		}
		if cfg.ExportTimeout > 0 {
			batchOpts = append(batchOpts, sdktrace.WithExportTimeout(time.Duration(cfg.ExportTimeout)*time.Second))
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
		shutdownFn = tp.Shutdown
	})

	return initErr
}

func tracingTLSCredentials(cfg config.TracingConfig) (credentials.TransportCredentials, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify, //nolint:gosec // Config-controlled behavior.
		MinVersion:         tls.VersionTLS12,
	}

	if cfg.TLSCACert != "" {
		caBytes, err := os.ReadFile(cfg.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read tls-cacert %q: %w", cfg.TLSCACert, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("failed to parse tls-cacert %q", cfg.TLSCACert)
		}
		tlsCfg.RootCAs = pool
	}

	return credentials.NewTLS(tlsCfg), nil
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

func SpansDisabledFromContext(ctx context.Context) bool {
	disabled, ok := ctx.Value(spansDisabledKey{}).(bool)
	return ok && disabled
}

func ContextWithSpanNamePrefix(ctx context.Context, prefix string) context.Context {
	if prefix == "" {
		return ctx
	}
	return context.WithValue(ctx, spanNamePrefixKey{}, prefix)
}

// ContextWithRetryLoop annotates ctx when reconcile work is driven from the retry loop.
func ContextWithRetryLoop(ctx context.Context, retry bool) context.Context {
	if !retry {
		return ctx
	}
	return context.WithValue(ctx, retryLoopKey{}, true)
}

// RetryLoopFromContext reports whether ctx was annotated as a retry-loop reconcile.
func RetryLoopFromContext(ctx context.Context) bool {
	retry, ok := ctx.Value(retryLoopKey{}).(bool)
	return ok && retry
}

func ContextWithOperation(ctx context.Context, operation Operation) context.Context {
	if operation == "" {
		return ctx
	}
	return context.WithValue(ctx, operationKey{}, operation)
}

func OperationFromContext(ctx context.Context) Operation {
	operation, _ := ctx.Value(operationKey{}).(Operation)
	if operation == "" {
		return OperationUnknown
	}
	return operation
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
	if SpansDisabledFromContext(ctx) {
		return noopTracer.Start(ctx, "")
	}
	prefix := SpanNamePrefixFromContext(ctx)
	return Tracer(prefix).Start(ctx, SpanName(ctx, operation))
}

// StartTrace starts a reconcile span from propagated trace context.
// In linked mode, it starts a new root span with a link.
// In parent mode, it starts a child span with propagated context as parent.
func StartTrace(ctx context.Context, annotations map[string]string) (context.Context, trace.Span) {
	if !config.OVNKubernetesFeature.EnableTracing {
		ctx = ContextWithSpansDisabled(ctx)
		return noopTracer.Start(ctx, "")
	}
	if SpansDisabledFromContext(ctx) {
		return noopTracer.Start(ctx, "")
	}

	linkedSC, ok := SpanContextFromPodAnnotations(annotations)
	if !ok {
		// Only emit spans when propagated context is present. Mark the context so
		// child spans in this reconcile path are also suppressed.
		ctx = ContextWithSpansDisabled(ctx)
		return noopTracer.Start(ctx, "")
	}

	prefix := SpanNamePrefixFromContext(ctx)
	name := SpanName(ctx, string(OperationFromContext(ctx)))
	if spanRelationshipMode == SpanRelationshipModeParent {
		remoteParentCtx := trace.ContextWithRemoteSpanContext(ctx, linkedSC)
		return Tracer(prefix).Start(remoteParentCtx, name)
	}

	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithLinks(trace.Link{SpanContext: linkedSC}),
	}
	return Tracer(prefix).Start(ctx, name, opts...)
}

// SpanContextFromPodAnnotations extracts propagated upstream span context from
// Pod annotations without setting it as the active remote parent.
func SpanContextFromPodAnnotations(annotations map[string]string) (trace.SpanContext, bool) {
	if len(annotations) == 0 {
		return trace.SpanContext{}, false
	}
	traceparent, ok := annotations[traceparentAnnotationKey]
	if !ok {
		return trace.SpanContext{}, false
	}
	sc, err := spanContextFromTraceparent(traceparent)
	if err != nil {
		klog.Warningf("Ignoring propagated trace context in pod annotation %q: %v",
			traceparentAnnotationKey, err)
		return trace.SpanContext{}, false
	}
	return sc, true
}

// spanContextFromTraceparent parses a W3C traceparent value into a remote span
// context. Parsing is delegated to the SDK propagator so that spec details
// (version handling, forward-compatible extension fields, reserved flag bits)
// stay owned upstream.
func spanContextFromTraceparent(tp string) (trace.SpanContext, error) {
	// annotation values routinely pick up surrounding whitespace, which the
	// propagator treats as a malformed field
	carrier := propagation.MapCarrier{traceparentHeader: strings.TrimSpace(tp)}
	sc := trace.SpanContextFromContext(traceparentPropagator.Extract(context.Background(), carrier))
	if !sc.IsValid() {
		return trace.SpanContext{}, fmt.Errorf("invalid traceparent %q", tp)
	}
	return sc, nil
}

func PodAttrs(namespace, podName, podUID string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(SpanAttrK8sPodNamespace, namespace),
		attribute.String(SpanAttrK8sPodName, podName),
		attribute.String(SpanAttrK8sPodUID, podUID),
	}
}
