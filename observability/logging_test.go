package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

var errSensitiveLogPayload = errors.New("card 4111111111111111 belongs to alice@example.com")

func tracedContext(t *testing.T) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	require.NoError(t, err)
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
}

func decodeLog(t *testing.T, buffer *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	require.NoError(t, json.Unmarshal(buffer.Bytes(), &entry))
	return entry
}

func TestLogger_AlwaysAddsTraceFieldsAndRedactsPIIRecursively(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(slog.New(slog.NewJSONHandler(&buffer, nil)))

	logger.Info(tracedContext(t), "order.created",
		"customer_id", "cust_secret",
		"message", "contact alice@example.com using 4111 1111 1111 1111",
		"nested", map[string]any{"authorization": "Bearer secret-token", "safe": "value"},
	)

	entry := decodeLog(t, &buffer)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", entry["trace_id"])
	assert.Equal(t, "0123456789abcdef", entry["span_id"])
	assert.Equal(t, "[REDACTED]", entry["customer_id"])
	assert.Equal(t, "contact [REDACTED_EMAIL] using [REDACTED_CARD]", entry["message"])
	nested := entry["nested"].(map[string]any)
	assert.Equal(t, "[REDACTED]", nested["authorization"])
	assert.Equal(t, "value", nested["safe"])
}

func TestLogger_ErrorSanitizesErrorTextAndIncludesStack(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewLogger(slog.New(slog.NewJSONHandler(&buffer, nil)))

	logger.Error(context.Background(), "request.failed", errSensitiveLogPayload)

	entry := decodeLog(t, &buffer)
	assert.Equal(t, "", entry["trace_id"])
	assert.Equal(t, "card [REDACTED_CARD] belongs to [REDACTED_EMAIL]", entry["error"])
	assert.Contains(t, entry["stack"], "TestLogger_ErrorSanitizesErrorTextAndIncludesStack")
}
