package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsHistogramAndExposition(t *testing.T) {
	var m metrics
	m.observe(200, 20*time.Millisecond)
	m.observe(503, 3*time.Second)
	text := m.text()
	for _, want := range []string{
		"# TYPE silencer_gateway_request_duration_seconds histogram",
		"silencer_gateway_request_duration_seconds_bucket{le=\"0.025\"} 1",
		"silencer_gateway_request_duration_seconds_bucket{le=\"+Inf\"} 2",
		"silencer_gateway_request_duration_seconds_count 2",
		"silencer_gateway_request_duration_seconds_sum 3.020000",
		"silencer_gateway_response_status_total{code=\"2xx\"} 1",
		"silencer_gateway_response_status_total{code=\"5xx\"} 1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q:\n%s", want, text)
		}
	}
}
