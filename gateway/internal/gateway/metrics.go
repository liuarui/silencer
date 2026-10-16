package gateway

import (
	"fmt"
	"sync/atomic"
	"time"
)

type metrics struct {
	requests, proxyRequests, proxyErrors, maskRequests, maskErrors, storageErrors, rejected atomic.Uint64
	inFlight                                                                                atomic.Int64
	status2xx, status3xx, status4xx, status5xx                                              atomic.Uint64
	latencyBuckets                                                                          [8]atomic.Uint64
	latencyCount, latencySum                                                                atomic.Uint64 // nanoseconds
}

var latencyBounds = [...]time.Duration{10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}

func (m *metrics) observe(status int, d time.Duration) {
	switch {
	case status < 300:
		m.status2xx.Add(1)
	case status < 400:
		m.status3xx.Add(1)
	case status < 500:
		m.status4xx.Add(1)
	default:
		m.status5xx.Add(1)
	}
	m.latencyCount.Add(1)
	if d < 0 {
		d = 0
	}
	m.latencySum.Add(uint64(d.Nanoseconds()))
	for i, b := range latencyBounds {
		if d <= b {
			m.latencyBuckets[i].Add(1)
		}
	}
}
func (m *metrics) text() string {
	counts := make([]uint64, len(latencyBounds))
	for i := range counts {
		counts[i] = m.latencyBuckets[i].Load()
	}
	return fmt.Sprintf("# HELP silencer_gateway_requests_total Total HTTP requests received.\n# TYPE silencer_gateway_requests_total counter\nsilencer_gateway_requests_total %d\n# HELP silencer_gateway_proxy_requests_total Proxy requests forwarded upstream.\n# TYPE silencer_gateway_proxy_requests_total counter\nsilencer_gateway_proxy_requests_total %d\n# HELP silencer_gateway_proxy_errors_total Proxy and upstream errors.\n# TYPE silencer_gateway_proxy_errors_total counter\nsilencer_gateway_proxy_errors_total %d\n# HELP silencer_gateway_rejected_requests_total Requests rejected by concurrency limits.\n# TYPE silencer_gateway_rejected_requests_total counter\nsilencer_gateway_rejected_requests_total %d\n# HELP silencer_gateway_in_flight_requests Requests currently in flight.\n# TYPE silencer_gateway_in_flight_requests gauge\nsilencer_gateway_in_flight_requests %d\n# HELP silencer_gateway_mask_requests_total Request bodies sent to the masking engine.\n# TYPE silencer_gateway_mask_requests_total counter\nsilencer_gateway_mask_requests_total %d\n# HELP silencer_gateway_mask_errors_total Masking engine errors.\n# TYPE silencer_gateway_mask_errors_total counter\nsilencer_gateway_mask_errors_total %d\n# HELP silencer_gateway_storage_errors_total Storage errors.\n# TYPE silencer_gateway_storage_errors_total counter\nsilencer_gateway_storage_errors_total %d\n# HELP silencer_gateway_response_status_total HTTP responses by status class.\n# TYPE silencer_gateway_response_status_total counter\nsilencer_gateway_response_status_total{code=\"2xx\"} %d\nsilencer_gateway_response_status_total{code=\"3xx\"} %d\nsilencer_gateway_response_status_total{code=\"4xx\"} %d\nsilencer_gateway_response_status_total{code=\"5xx\"} %d\n# HELP silencer_gateway_request_duration_seconds HTTP request duration.\n# TYPE silencer_gateway_request_duration_seconds histogram\nsilencer_gateway_request_duration_seconds_bucket{le=\"0.01\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"0.025\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"0.05\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"0.1\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"0.25\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"0.5\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"1\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"2\"} %d\nsilencer_gateway_request_duration_seconds_bucket{le=\"+Inf\"} %d\nsilencer_gateway_request_duration_seconds_count %d\nsilencer_gateway_request_duration_seconds_sum %f\n", m.requests.Load(), m.proxyRequests.Load(), m.proxyErrors.Load(), m.rejected.Load(), m.inFlight.Load(), m.maskRequests.Load(), m.maskErrors.Load(), m.storageErrors.Load(), m.status2xx.Load(), m.status3xx.Load(), m.status4xx.Load(), m.status5xx.Load(), counts[0], counts[1], counts[2], counts[3], counts[4], counts[5], counts[6], counts[7], m.latencyCount.Load(), m.latencyCount.Load(), float64(m.latencySum.Load())/1e9)
}
