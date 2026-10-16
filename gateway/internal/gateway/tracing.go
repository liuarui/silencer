package gateway

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const tracerName = "github.com/silencer/gateway"

// ConfigureTracing enables OTLP/HTTP exporting only when an endpoint is
// explicitly configured. With no endpoint, the OpenTelemetry API remains a
// cheap no-op and the gateway never attempts a network connection at startup.
func ConfigureTracing(ctx context.Context) (func(context.Context) error, error) {
	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" {
		otel.SetTextMapPropagator(propagator)
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes("", attribute.String("service.name", "silencer-gateway")),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagator)
	return provider.Shutdown, nil
}
