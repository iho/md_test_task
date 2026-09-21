// Package observability holds the concrete infrastructure adapters
// (tracing, logging, metrics) that implement the ports declared in
// internal/usecase. Nothing in internal/domain or internal/usecase imports
// this package's dependencies (otel, slog, prometheus) directly - they
// depend only on the interfaces in internal/usecase/ports.go.
package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"ordersvc/internal/usecase"
)

var (
	ErrServiceNameRequired      = errors.New("service name is required")
	ErrUnsupportedTraceExporter = errors.New("unsupported trace exporter")
)

// Tracer adapts an OpenTelemetry tracer to the usecase.Tracer port.
// Trace context lives in context.Context (Option A - see TRACING.md):
// otel's own propagators read/write it there, which is how trace context
// crosses the API GW -> Order Service -> Payment/Inventory Service hops
// for free once each service uses otelhttp/otelgrpc instrumentation.
type Tracer struct {
	tracer oteltrace.Tracer
}

func NewTracer(t oteltrace.Tracer) *Tracer {
	return &Tracer{tracer: t}
}

// SetupTracing preserves the self-contained JSON exporter used by tests and
// local development.
func SetupTracing(serviceName string, output io.Writer) (*sdktrace.TracerProvider, error) {
	return SetupTracingWithConfig(context.Background(), TraceConfig{
		ServiceName: serviceName,
		Exporter:    "stdout",
		Output:      output,
	})
}

type TraceConfig struct {
	ServiceName string
	Exporter    string
	Output      io.Writer
}

// SetupTracingWithConfig installs the SDK provider and W3C propagator. OTLP
// uses the standard OTEL_EXPORTER_OTLP_* environment variables.
func SetupTracingWithConfig(ctx context.Context, config TraceConfig) (*sdktrace.TracerProvider, error) {
	if config.ServiceName == "" {
		return nil, ErrServiceNameRequired
	}
	if config.Exporter == "" {
		config.Exporter = "stdout"
	}
	res := resource.NewSchemaless(attribute.String("service.name", config.ServiceName))
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}
	switch strings.ToLower(strings.TrimSpace(config.Exporter)) {
	case "stdout":
		if config.Output == nil {
			config.Output = io.Discard
		}
		options = append(options, sdktrace.WithBatcher(&jsonSpanExporter{encoder: json.NewEncoder(config.Output)}))
	case "otlp":
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
		}
		options = append(options, sdktrace.WithBatcher(exporter))
	case "none":
		options[1] = sdktrace.WithSampler(sdktrace.NeverSample())
	default:
		return nil, fmt.Errorf("%w %q (want stdout, otlp, or none)", ErrUnsupportedTraceExporter, config.Exporter)
	}
	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider, nil
}

func (t *Tracer) Start(ctx context.Context, spanName string) (context.Context, usecase.Span) {
	ctx, span := t.tracer.Start(ctx, spanName)
	return ctx, &otelSpan{span: span}
}

func (t *Tracer) SpanFromContext(ctx context.Context) usecase.Span {
	return &otelSpan{span: oteltrace.SpanFromContext(ctx)}
}

type otelSpan struct {
	span oteltrace.Span
}

func (s *otelSpan) AddEvent(name string, attrs map[string]any) {
	s.span.AddEvent(name, oteltrace.WithAttributes(toAttributes(attrs)...))
}

func (s *otelSpan) RecordError(err error) {
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
}

func (s *otelSpan) End() {
	s.span.End()
}

func toAttributes(attrs map[string]any) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		switch val := v.(type) {
		case string:
			kvs = append(kvs, attribute.String(k, val))
		case int:
			kvs = append(kvs, attribute.Int(k, val))
		case int64:
			kvs = append(kvs, attribute.Int64(k, val))
		case bool:
			kvs = append(kvs, attribute.Bool(k, val))
		default:
			kvs = append(kvs, attribute.String(k, fmt.Sprintf("%v", val)))
		}
	}
	return kvs
}

// TraceIDFromContext extracts the current trace ID as a hex string, used
// to stamp log lines (LOGGING.md) and metric exemplars (METRICS.md Q3)
// with the trace that produced them.
func TraceIDFromContext(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

func SpanIDFromContext(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.HasSpanID() {
		return ""
	}
	return sc.SpanID().String()
}

type jsonSpanExporter struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (e *jsonSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, span := range spans {
		events := make([]map[string]any, 0, len(span.Events()))
		for _, event := range span.Events() {
			events = append(events, map[string]any{
				"name":       event.Name,
				"at":         event.Time,
				"attributes": attributesToMap(event.Attributes),
			})
		}
		record := map[string]any{
			"type":           "trace_span",
			"trace_id":       span.SpanContext().TraceID().String(),
			"span_id":        span.SpanContext().SpanID().String(),
			"parent_span_id": span.Parent().SpanID().String(),
			"name":           span.Name(),
			"kind":           span.SpanKind().String(),
			"started_at":     span.StartTime(),
			"ended_at":       span.EndTime(),
			"status":         span.Status().Code.String(),
			"resource":       attributesToMap(span.Resource().Attributes()),
			"attributes":     attributesToMap(span.Attributes()),
			"events":         events,
		}
		if err := e.encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

func (e *jsonSpanExporter) Shutdown(context.Context) error { return nil }

func attributesToMap(attributes []attribute.KeyValue) map[string]any {
	result := make(map[string]any, len(attributes))
	for _, attr := range attributes {
		result[string(attr.Key)] = attr.Value.AsInterface()
	}
	return result
}
