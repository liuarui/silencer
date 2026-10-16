package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

// Buffer all text until Finish to ensure the terminal frame cannot overtake
// the final restored suffix, including a suffix inside tool arguments.
type completionRestoreSession struct{ pending []byte }

func (s *completionRestoreSession) Push(_ context.Context, b []byte) ([]byte, error) {
	s.pending = append(s.pending, b...)
	return nil, nil
}
func (s *completionRestoreSession) Finish(context.Context) ([]byte, error) {
	out := strings.ReplaceAll(string(s.pending), "{{EMAIL_abc123}}", "restored@example.test")
	s.pending = nil
	return []byte(out), nil
}
func (s *completionRestoreSession) Abort() { s.pending = nil }

type completionRestoreEngine struct{ replaceRestoreEngine }

func (completionRestoreEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return &completionRestoreSession{}, nil
}

func completionServer(t *testing.T, handler http.HandlerFunc) (*Server, *storage.Storage) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "completion.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 2, Engine: "go-fallback"}, completionRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, store
}

func completedLedger(t *testing.T, store *storage.Storage) storage.Request {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		items, err := store.ListRequests(context.Background(), 10)
		if err == nil && len(items) == 1 && items[0].Status != "running" {
			return items[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("response ledger did not finish")
	return storage.Request{}
}

func TestProtocolTerminalCompletesBeforeUpstreamEOF(t *testing.T) {
	cases := []struct{ name, contentType, frames string }{
		{"chat", "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{{EMAIL_abc123}}\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"},
		{"tools", "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"email\\\":\\\"{{EMAIL_abc123}}\\\"}\"}}]}}]}\n\ndata: [DONE]\n\n"},
		{"anthropic", "text/event-stream", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"{{EMAIL_abc123}}\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"responses", "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"{{EMAIL_abc123}}\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"},
		{"ndjson", "application/x-ndjson", "{\"response\":\"{{EMAIL_abc123}}\",\"done\":false}\n{\"response\":\"\",\"done\":true}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, store := completionServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.frames)
				w.(http.Flusher).Flush()
				// The protocol is finished, but the upstream transport is not.
				<-r.Context().Done()
			})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/requests/stream", nil)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			cancel()
			_ = resp.Body.Close()
			if err != nil || strings.Contains(string(body), "{{EMAIL_abc123}}") || !strings.Contains(string(body), "restored@example.test") {
				t.Fatalf("completed stream body=%q err=%v", body, err)
			}
			item := completedLedger(t, store)
			var meta map[string]any
			_ = json.Unmarshal([]byte(item.Meta), &meta)
			if item.Status != "200" || item.Error != "" || meta["restore_restored"] != float64(1) || meta["restore_evidence_complete"] != true {
				t.Fatalf("completed stream status=%s error=%s meta=%s", item.Status, item.Error, item.Meta)
			}
			if tc.name == "chat" && meta["usage_completion_tokens"] != float64(7) {
				t.Fatal("per-choice completion discarded the trailing usage frame")
			}
		})
	}
}

func TestCancelImmediatelyAfterTerminalKeepsSuccessfulLedger(t *testing.T) {
	s, store := completionServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{{EMAIL_abc123}}\"}}]}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/requests/stream", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	found := false
	for scanner.Scan() {
		if scanner.Text() == "data: [DONE]" {
			found = true
			cancel()
			break
		}
	}
	_ = resp.Body.Close()
	if !found {
		t.Fatal("terminal frame not delivered")
	}
	if item := completedLedger(t, store); item.Status != "200" || item.Error != "" {
		t.Fatalf("terminal cancellation status=%s error=%s", item.Status, item.Error)
	}
}

type failedCompletionWriter struct{ *httptest.ResponseRecorder }

func (*failedCompletionWriter) Write([]byte) (int, error) { return 0, context.Canceled }

func TestTerminalReadDoesNotHideDownstreamWriteFailure(t *testing.T) {
	s, store := completionServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"text\":\"{{EMAIL_abc123}}\"}\n\ndata: [DONE]\n\n")
	})
	s.Handler().ServeHTTP(&failedCompletionWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/api/requests/stream", nil))
	item := completedLedger(t, store)
	var meta map[string]any
	_ = json.Unmarshal([]byte(item.Meta), &meta)
	if item.Status != "499" || item.Error != "canceled" || meta["restore_restored"] != float64(0) || meta["restore_evidence_complete"] != false {
		t.Fatalf("write failure status=%s error=%s meta=%s", item.Status, item.Error, item.Meta)
	}
}

type failedCompletionFlushWriter struct{ *httptest.ResponseRecorder }

func (*failedCompletionFlushWriter) FlushError() error { return context.Canceled }

func TestTerminalReadDoesNotHideDownstreamFlushFailure(t *testing.T) {
	s, store := completionServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"text\":\"{{EMAIL_abc123}}\"}\n\ndata: [DONE]\n\n")
	})
	s.Handler().ServeHTTP(&failedCompletionFlushWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/api/requests/stream", nil))
	item := completedLedger(t, store)
	if item.Status != "499" || item.Error != "canceled" {
		t.Fatalf("flush failure status=%s error=%s", item.Status, item.Error)
	}
}

func TestProtocolLocalCompletionPreservesOtherChoicesAndUsage(t *testing.T) {
	s := newProtocolRestoreSession("sse", func(context.Context) (RestoreSession, error) { return &completionRestoreSession{}, nil })
	for _, frame := range []string{
		`{"choices":[{"index":0,"delta":{"content":"{{EMAIL_abc123}}"},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":1,"delta":{"content":"{{EMAIL_abc123}}"}}]}`,
		`{"choices":[],"usage":{"completion_tokens":7}}`,
	} {
		if _, err := s.Push(context.Background(), []byte("data: "+frame+"\n\n")); err != nil {
			t.Fatal(err)
		}
		if s.StreamComplete() {
			t.Fatal("per-choice completion ended the whole response")
		}
	}
	// A marker split across transport reads only completes after its empty line.
	for i, b := range []byte("data: [DONE]\r\n\r\n") {
		if _, err := s.Push(context.Background(), []byte{b}); err != nil {
			t.Fatal(err)
		}
		if i < len("data: [DONE]\r\n") && s.StreamComplete() {
			t.Fatal("partial terminal frame ended the response")
		}
	}
	if !s.StreamComplete() {
		t.Fatal("whole response marker was not recognized")
	}
	if _, err := s.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if counts := s.RestoreCounts(); !counts.Complete || counts.Restored != 2 || counts.Unresolved != 0 {
		t.Fatalf("parallel choice evidence=%+v", counts)
	}
}

func TestRestoredEOFIgnoresLateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &restoreReadCloser{source: io.NopCloser(strings.NewReader("{{EMAIL_abc123}}")), session: &completionRestoreSession{}, ctx: ctx}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := r.Close(); err != nil {
		t.Fatalf("late cancellation changed successful EOF: %v", err)
	}
}

func TestProtocolTerminalReadErrorDoesNotOverrideCompletedPayload(t *testing.T) {
	s := newProtocolRestoreSession("sse", func(context.Context) (RestoreSession, error) { return &completionRestoreSession{}, nil })
	r := &restoreReadCloser{source: &terminalAndErrorReader{}, session: s, ctx: context.Background()}
	body, err := io.ReadAll(r)
	if err != nil || !strings.Contains(string(body), "data: [DONE]") || !s.RestoreCounts().Complete {
		t.Fatalf("terminal payload discarded: body=%q err=%v", body, err)
	}
	_ = r.Close()
}

type terminalAndErrorReader struct{ read bool }

func (r *terminalAndErrorReader) Read(b []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	return copy(b, "data: [DONE]\n\n"), context.Canceled
}
func (*terminalAndErrorReader) Close() error { return errors.New("closed after protocol terminal") }
