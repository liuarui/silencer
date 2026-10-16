package gateway

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestTracingPropagatesTraceContextAndBaggageWithoutExporter(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := ConfigureTracing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	traceID := trace.TraceID{1}
	spanID := trace.SpanID{2}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
	baggageValue, err := baggage.NewMember("tenant", "bench")
	if err != nil {
		t.Fatal(err)
	}
	baggageContext, err := baggage.New(baggageValue)
	if err != nil {
		t.Fatal(err)
	}
	ctx := baggage.ContextWithBaggage(trace.ContextWithSpanContext(context.Background(), spanContext), baggageContext)
	carrier := propagation.HeaderCarrier(http.Header{})
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if carrier.Get("Traceparent") == "" {
		t.Fatal("traceparent was not injected")
	}
	if carrier.Get("Baggage") != "tenant=bench" {
		t.Fatalf("baggage=%q", carrier.Get("Baggage"))
	}
}
