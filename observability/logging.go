package observability

import (
	"context"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
)

// redactedKeys never make it into a log line as-is. Real PII policy would
// live in config, but the mechanism (redact-by-key at the logging
// boundary, not scattered `if key == "password"` checks at call sites) is
// what LOGGING.md asks for.
var redactedKeys = map[string]struct{}{
	"password":        {},
	"card_number":     {},
	"cvv":             {},
	"ssn":             {},
	"email":           {},
	"customer_id":     {},
	"payment_ref":     {},
	"authorization":   {},
	"token":           {},
	"idempotency_key": {},
}

var (
	emailPattern  = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	cardPattern   = regexp.MustCompile(`\b(?:[0-9][ -]*?){13,19}\b`)
	bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]+=*`)
)

// Logger adapts slog to the usecase.Logger port. Every line gets the
// current trace ID attached automatically (when present), so logs and
// traces can be correlated by trace_id in the log backend - see
// LOGGING.md.
type Logger struct {
	base *slog.Logger
}

func NewLogger(base *slog.Logger) *Logger {
	return &Logger{base: base}
}

func (l *Logger) Info(ctx context.Context, msg string, kv ...any) {
	l.base.InfoContext(ctx, msg, append(contextAttrs(ctx), redact(kv)...)...)
}

func (l *Logger) Error(ctx context.Context, msg string, err error, kv ...any) {
	errorText := "<nil>"
	if err != nil {
		errorText = err.Error()
	}
	kv = append(kv, "error", errorText, "stack", string(debug.Stack()))
	l.base.ErrorContext(ctx, msg, append(contextAttrs(ctx), redact(kv)...)...)
}

func contextAttrs(ctx context.Context) []any {
	return []any{
		"trace_id", TraceIDFromContext(ctx),
		"span_id", SpanIDFromContext(ctx),
	}
}

func redact(kv []any) []any {
	out := make([]any, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		if _, sensitive := redactedKeys[normalizeKey(key)]; sensitive {
			out = append(out, kv[i], "[REDACTED]")
			continue
		}
		out = append(out, kv[i], sanitizeValue(kv[i+1]))
	}
	return out
}

func normalizeKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(key), "-", "_")
}

func sanitizeValue(value any) any {
	switch value := value.(type) {
	case string:
		return sanitizeText(value)
	case error:
		return sanitizeText(value.Error())
	case map[string]any:
		clean := make(map[string]any, len(value))
		for key, nested := range value {
			if _, sensitive := redactedKeys[normalizeKey(key)]; sensitive {
				clean[key] = "[REDACTED]"
			} else {
				clean[key] = sanitizeValue(nested)
			}
		}
		return clean
	case []any:
		clean := make([]any, len(value))
		for i, nested := range value {
			clean[i] = sanitizeValue(nested)
		}
		return clean
	default:
		return value
	}
}

func sanitizeText(value string) string {
	value = emailPattern.ReplaceAllString(value, "[REDACTED_EMAIL]")
	value = cardPattern.ReplaceAllString(value, "[REDACTED_CARD]")
	return bearerPattern.ReplaceAllString(value, "[REDACTED_TOKEN]")
}
