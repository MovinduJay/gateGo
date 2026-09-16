package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestRequestTraceUsesActiveSpanContext(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("b7ad6b7169203331")
	if err != nil {
		t.Fatal(err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)

	traceparent, gotTraceID := RequestTrace(ctx, "")
	if want := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"; traceparent != want {
		t.Fatalf("traceparent = %q, want %q", traceparent, want)
	}
	if want := traceID.String(); gotTraceID != want {
		t.Fatalf("trace ID = %q, want %q", gotTraceID, want)
	}
}

func TestRequestTraceFallsBackToIncomingHeader(t *testing.T) {
	incoming := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	traceparent, traceID := RequestTrace(context.Background(), incoming)
	if traceparent != incoming {
		t.Fatalf("traceparent = %q, want %q", traceparent, incoming)
	}
	if traceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("trace ID = %q", traceID)
	}
}
