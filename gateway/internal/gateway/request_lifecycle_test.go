package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestDrainKeepsCompletedBatchesClosed(t *testing.T) {
	var requests requestDrain
	select {
	case <-requests.drained():
	default:
		t.Fatal("an idle tracker must already be drained")
	}
	requests.begin()
	first := requests.drained()
	requests.end()
	requests.begin()
	defer requests.end()
	second := requests.drained()
	if first == second {
		t.Fatal("a new batch reused the previous completion signal")
	}
	select {
	case <-first:
	default:
		t.Fatal("a later request reopened a completed batch")
	}
	select {
	case <-second:
		t.Fatal("the new batch drained before its request finished")
	default:
	}
}

func TestEngineReplacementWaitsForItsRequestBatch(t *testing.T) {
	old := &shutdownTrackingEngine{closedCh: make(chan struct{})}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, old)
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	t.Cleanup(func() {
		for pending > 0 {
			s.requests.end()
			pending--
		}
		_ = s.Shutdown(context.Background())
	})
	s.requests.begin()
	pending++
	current := &shutdownTrackingEngine{closedCh: make(chan struct{})}
	if !s.replaceEngine(current) {
		t.Fatal("engine replacement failed")
	}
	select {
	case <-old.closedCh:
		t.Fatal("the old engine closed with a request still in flight")
	default:
	}
	s.requests.end()
	pending--
	s.requests.begin()
	pending++
	select {
	case <-old.closedCh:
	case <-time.After(time.Second):
		t.Fatal("the next request batch prevented the old engine from closing")
	}
	select {
	case <-current.closedCh:
		t.Fatal("the current engine closed while still in use")
	default:
	}
}

func TestShutdownRejectsNewRequests(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auth/status", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}
