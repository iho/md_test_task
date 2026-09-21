package observability

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

var errPendingCollection = errors.New("database unavailable")

type pendingCounterStub struct {
	count int
	err   error
	calls int
}

func (counter *pendingCounterStub) CountPending(context.Context) (int, error) {
	counter.calls++
	return counter.count, counter.err
}

func TestPrometheusMetrics_BoundsExternallySuppliedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewPrometheusMetrics(registry, &pendingCounterStub{})
	metrics.IncOrdersCreated("invented-status", "attacker-controlled-tier")

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "orders_created_total" {
			continue
		}
		require.Len(t, family.Metric, 1)
		labels := family.Metric[0].Label
		assert.Equal(t, "customer_tier", labels[0].GetName())
		assert.Equal(t, "unknown", labels[0].GetValue())
		assert.Equal(t, "status", labels[1].GetName())
		assert.Equal(t, "failure", labels[1].GetValue())
		return
	}
	t.Fatal("orders_created_total was not gathered")
}

func TestPrometheusMetrics_AttachesTraceIDAsExemplarNotLabel(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewPrometheusMetrics(registry, &pendingCounterStub{})
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	metrics.ObserveStepDuration(ctx, "payment", 200*time.Millisecond)

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "order_processing_duration_seconds" {
			continue
		}
		require.Len(t, family.Metric, 1)
		// The only ordinary label is the bounded step. trace_id exists solely
		// on an exemplar attached to the observed bucket.
		require.Len(t, family.Metric[0].Label, 1)
		assert.Equal(t, "step", family.Metric[0].Label[0].GetName())
		for _, bucket := range family.Metric[0].GetHistogram().Bucket {
			exemplar := bucket.GetExemplar()
			if exemplar == nil {
				continue
			}
			require.Len(t, exemplar.Label, 1)
			assert.Equal(t, "trace_id", exemplar.Label[0].GetName())
			assert.Equal(t, traceID.String(), exemplar.Label[0].GetValue())
			return
		}
		t.Fatal("histogram observation had no exemplar")
	}
	t.Fatal("order_processing_duration_seconds was not gathered")
}

func TestPrometheusMetrics_CollectsPendingCountOnlyWhenScraped(t *testing.T) {
	registry := prometheus.NewRegistry()
	counter := &pendingCounterStub{count: 7}
	_ = NewPrometheusMetrics(registry, counter)
	require.Zero(t, counter.calls)

	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Equal(t, 1, counter.calls)
	for _, family := range families {
		if family.GetName() == "orders_pending_count" {
			require.Len(t, family.Metric, 1)
			assert.Equal(t, float64(7), family.Metric[0].GetGauge().GetValue())
			return
		}
	}
	t.Fatal("orders_pending_count was not gathered")
}

func TestPrometheusMetrics_ExposesPendingCollectionFailure(t *testing.T) {
	registry := prometheus.NewRegistry()
	_ = NewPrometheusMetrics(registry, &pendingCounterStub{err: errPendingCollection})

	_, err := registry.Gather()
	require.NoError(t, err)
	// A collector may be gathered concurrently with the gauge that increments
	// it, so inspect the next scrape for the collection-error counter.
	families, err := registry.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, family := range families {
		switch family.GetName() {
		case "orders_pending_count":
			values[family.GetName()] = family.Metric[0].GetGauge().GetValue()
		case "orders_pending_collection_errors_total":
			values[family.GetName()] = family.Metric[0].GetCounter().GetValue()
		}
	}
	assert.True(t, math.IsNaN(values["orders_pending_count"]))
	assert.GreaterOrEqual(t, values["orders_pending_collection_errors_total"], float64(1))
}
