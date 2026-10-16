package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

type transportTimeout struct{ wrapsDeadline bool }

func (transportTimeout) Error() string   { return "transport response timeout" }
func (transportTimeout) Timeout() bool   { return true }
func (transportTimeout) Temporary() bool { return true }
func (e transportTimeout) Is(target error) bool {
	return e.wrapsDeadline && target == context.DeadlineExceeded
}

func TestUpstreamTimeoutIsNotClientCancellation(t *testing.T) {
	for _, wrapsDeadline := range []bool{false, true} {
		err := fmt.Errorf("transport failed: %w", transportTimeout{wrapsDeadline})
		status, failure := proxyFailure(context.Background(), err)
		if status != http.StatusGatewayTimeout || ledgerErrorCategory(failure) != "upstream_timeout" {
			t.Fatalf("deadline wrapper=%v status=%d category=%s", wrapsDeadline, status, ledgerErrorCategory(failure))
		}
		if !errors.Is(failure, errUpstreamTimeout) {
			t.Fatal("upstream timeout identity was lost")
		}
	}
}

func TestInboundContextDeterminesCancellationReason(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		want := "canceled"
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = "deadline_exceeded"
		}
		cancel()
		status, failure := proxyFailure(ctx, errors.New("transport failed without a context wrapper"))
		if status != 499 || ledgerErrorCategory(failure) != want {
			t.Fatalf("deadline=%v status=%d category=%s", deadline, status, ledgerErrorCategory(failure))
		}
	}
}

func TestHeaderTimeoutReturns504AndErrorEvents(t *testing.T) {
	s, store := completionServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	// Use the real transport timeout error rather than a particular Go version's
	// string or DeadlineExceeded wrapping behavior.
	s.proxy.Transport.(*http.Transport).ResponseHeaderTimeout = 30 * time.Millisecond
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/requests/slow-header", nil))
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("header timeout status=%d body=%s", rr.Code, rr.Body.String())
	}
	item := completedLedger(t, store)
	var meta map[string]any
	_ = json.Unmarshal([]byte(item.Meta), &meta)
	if item.Error != "upstream_timeout" || meta["http_status"] != float64(504) || meta["failure_stage"] != "upstream_request" {
		t.Fatalf("timeout ledger category=%s meta=%s", item.Error, item.Meta)
	}
	events, err := store.ListEvents(context.Background(), item.ID, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("timeout events count=%d err=%v", len(events), err)
	}
	for _, event := range events {
		if displayLogEventType(event, nil) != "ERR" || logEventView(event, false)["failure_stage"] != "upstream_request" {
			t.Fatalf("timeout projected as cancellation: type=%s category=%s", event.Type, event.Error)
		}
	}
	view := requestItem(item)
	if view["failure_stage"] != "upstream_request" {
		t.Fatal("request detail dropped the failure stage")
	}
}

func TestSuccessfulResponseOmitsTransientFailureStage(t *testing.T) {
	_, store := newLogTestServer(t)
	item := storage.Request{ID: "late-read-error", Payload: []byte("GET /test"), Status: "running", StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}
	if err := store.AppendRequest(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record := &requestRecord{store: store, item: item}
	record.noteFailure("upstream_read")
	record.finish(200, nil)
	completed, err := store.GetRequest(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := requestMeta(completed)["failure_stage"]; present {
		t.Fatal("successful terminal response exposed an ignored tail error")
	}
}

type timeoutResponseBody struct{}

func (timeoutResponseBody) Read([]byte) (int, error) { return 0, transportTimeout{true} }
func (timeoutResponseBody) Close() error             { return nil }

func TestResponseReadTimeoutKeepsUpstreamStage(t *testing.T) {
	_, store := newLogTestServer(t)
	item := storage.Request{ID: "upstream-read-timeout", Payload: []byte("GET /test"), Status: "running", StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}
	if err := store.AppendRequest(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record := &requestRecord{store: store, item: item}
	r := &recordingReadCloser{ReadCloser: timeoutResponseBody{}, record: record, status: 200, ctx: context.Background()}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("upstream read timeout disappeared")
	}
	_ = r.Close()
	got, err := store.GetRequest(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta := requestMeta(got)
	if got.Error != "upstream_timeout" || meta["http_status"] != float64(504) || meta["failure_stage"] != "upstream_read" {
		t.Fatalf("upstream timeout misclassified: error=%s meta=%s", got.Error, got.Meta)
	}
}
