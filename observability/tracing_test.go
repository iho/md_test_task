package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

var errTestSpan = errors.New("boom")

func TestSetupTracing_RecordsExportsAndMarksErrorSpans(t *testing.T) {
	var output bytes.Buffer
	provider, err := SetupTracing("orders-test", &output)
	require.NoError(t, err)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tracer := NewTracer(otel.Tracer("test"))

	ctx, span := tracer.Start(context.Background(), "test.operation")
	require.NotEmpty(t, TraceIDFromContext(ctx))
	span.AddEvent("order.tested", map[string]any{"order_id": "ord_1"})
	span.RecordError(errTestSpan)
	span.End()
	require.NoError(t, provider.ForceFlush(context.Background()))

	var record map[string]any
	require.NoError(t, json.NewDecoder(&output).Decode(&record))
	assert.Equal(t, "trace_span", record["type"])
	assert.Equal(t, "test.operation", record["name"])
	assert.Equal(t, "Error", record["status"])
	assert.NotEmpty(t, record["trace_id"])
	events := record["events"].([]any)
	event := events[0].(map[string]any)
	assert.Equal(t, "order.tested", event["name"])
	assert.Equal(t, "ord_1", event["attributes"].(map[string]any)["order_id"])
}

func TestSetupTracingWithConfig_SupportsDisabledExporterAndRejectsUnknown(t *testing.T) {
	_, err := SetupTracingWithConfig(context.Background(), TraceConfig{})
	require.ErrorIs(t, err, ErrServiceNameRequired)

	provider, err := SetupTracingWithConfig(context.Background(), TraceConfig{
		ServiceName: "orders-test",
		Exporter:    "none",
	})
	require.NoError(t, err)
	require.NoError(t, provider.Shutdown(context.Background()))

	_, err = SetupTracingWithConfig(context.Background(), TraceConfig{
		ServiceName: "orders-test",
		Exporter:    "invented",
	})
	require.ErrorIs(t, err, ErrUnsupportedTraceExporter)
}
