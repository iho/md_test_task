package observability

import (
	"context"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusMetrics implements usecase.MetricsRecorder. All label sets are
// small and fixed at design time, never derived from a request field -
// that is what keeps this bounded (see METRICS.md Q2):
//   - orders_created_total{status,customer_tier}   -> 2 x 3 = 6 series
//   - order_processing_duration_seconds{step}      -> 1 series per known step
//   - orders_pending_count                          -> 1 series
type PrometheusMetrics struct {
	ordersCreated           *prometheus.CounterVec
	stepDuration            *prometheus.HistogramVec
	pendingCollectionErrors prometheus.Counter
}

// PendingOrderCounter supplies an authoritative count when Prometheus scrapes
// the gauge. It intentionally sits outside the request-path repository port.
type PendingOrderCounter interface {
	CountPending(ctx context.Context) (int, error)
}

func NewPrometheusMetrics(reg prometheus.Registerer, pending PendingOrderCounter) *PrometheusMetrics {
	m := &PrometheusMetrics{
		ordersCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_created_total",
			Help: "Orders created, by outcome and customer tier.",
		}, []string{"status", "customer_tier"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "order_processing_duration_seconds",
			Help:    "Order processing duration by pipeline step.",
			Buckets: prometheus.DefBuckets,
		}, []string{"step"}),
		pendingCollectionErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "orders_pending_collection_errors_total",
			Help: "Failures while collecting the authoritative pending-order gauge.",
		}),
	}
	pendingOrders := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "orders_pending_count",
		Help: "Orders currently pending payment or fulfillment.",
	}, func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		count, err := pending.CountPending(ctx)
		if err != nil {
			m.pendingCollectionErrors.Inc()
			return math.NaN()
		}
		return float64(count)
	})
	reg.MustRegister(m.ordersCreated, m.stepDuration, pendingOrders, m.pendingCollectionErrors)
	return m
}

func (m *PrometheusMetrics) IncOrdersCreated(status, customerTier string) {
	switch status {
	case "success", "failure":
	default:
		status = "failure"
	}
	switch customerTier {
	case "free", "premium", "unknown":
	default:
		customerTier = "unknown"
	}
	m.ordersCreated.WithLabelValues(status, customerTier).Inc()
}

// ObserveStepDuration records the histogram observation and, when the
// context carries an active trace, attaches the trace ID as a Prometheus
// exemplar instead of a label. Exemplars are sampled and capped by the
// server, so they give "click through from a slow bucket to one concrete
// trace" correlation (METRICS.md Q3) without the label ever being
// per-request/per-order - which is exactly the cardinality trap in
// METRICS.md Q2.
func (m *PrometheusMetrics) ObserveStepDuration(ctx context.Context, step string, d time.Duration) {
	switch step {
	case "validation", "payment", "fulfillment":
	default:
		step = "unknown"
	}
	observer := m.stepDuration.WithLabelValues(step)
	traceID := TraceIDFromContext(ctx)
	if traceID == "" {
		observer.Observe(d.Seconds())
		return
	}
	if exemplarObserver, ok := observer.(prometheus.ExemplarObserver); ok {
		exemplarObserver.ObserveWithExemplar(d.Seconds(), prometheus.Labels{"trace_id": traceID})
		return
	}
	observer.Observe(d.Seconds())
}
