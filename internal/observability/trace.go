package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// RequestTrace returns the active OpenTelemetry span context when available,
// keeping response correlation IDs and downstream propagation on one trace.
func RequestTrace(ctx context.Context, in string) (traceparent, traceID string) {
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		flags := byte(spanContext.TraceFlags())
		return fmt.Sprintf("00-%s-%s-%02x", spanContext.TraceID(), spanContext.SpanID(), flags), spanContext.TraceID().String()
	}
	return EnsureTraceparent(in)
}

func EnsureTraceparent(in string) (traceparent, traceID string) {
	parts := strings.Split(in, "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 {
		return in, parts[1]
	}
	trace := make([]byte, 16)
	span := make([]byte, 8)
	_, _ = rand.Read(trace)
	_, _ = rand.Read(span)
	tid := hex.EncodeToString(trace)
	return "00-" + tid + "-" + hex.EncodeToString(span) + "-01", tid
}
