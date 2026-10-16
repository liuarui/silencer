package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func extensionLedgerServer(t *testing.T, engine Engine) (*Server, *storage.Storage) {
	t.Helper()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "extension.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{AuthToken: "test-token", MaxConcurrency: 4, MaxResponseBytes: 1024}, engine)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	t.Cleanup(func() { _ = s.Shutdown(context.Background()); _ = store.Close() })
	return s, store
}

func extensionLedgerCall(t *testing.T, s *Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("X-Silencer-Token", "test-token")
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	var value map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &value); err != nil {
		t.Fatal(err, res.Body.String())
	}
	return res.Code, value
}

func ledgerMask(t *testing.T, s *Server, text string) map[string]any {
	t.Helper()
	code, masked := extensionLedgerCall(t, s, "POST", "/api/ext/mask", map[string]any{
		"text": text, "host": "chatgpt.com", "url": "https://chatgpt.com/backend-api/conversation?private=query", "method": "POST",
	})
	if code != 200 {
		t.Fatalf("mask: %d %v", code, masked)
	}
	return masked
}

func TestExtensionLedgerCapturesBodiesAndLinksEvents(t *testing.T) {
	s, store := extensionLedgerServer(t, contextualTestEngine{})
	raw := strings.Repeat("message ", 300) + "alice@example.com"
	masked := ledgerMask(t, s, raw)
	id, sid := masked["request_id"].(string), masked["sid"].(string)
	if !ledgerIDPattern.MatchString(id) || id == sid {
		t.Fatal("invalid ledger association", masked)
	}
	_, pending := extensionLedgerCall(t, s, "GET", "/api/requests/"+id, nil)
	if pending["request"].(map[string]any)["status"] != "awaiting_response" {
		t.Fatal(pending)
	}
	for i, text := range []string{"reply {{EMAIL_0000000000000001}}", " done"} {
		code, reply := extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{
			"sid": sid, "stream_id": "response", "text": text, "final": i == 1, "response_status": 201,
		})
		if code != 200 {
			t.Fatalf("restore: %d %v", code, reply)
		}
	}
	_, response := extensionLedgerCall(t, s, "GET", "/api/requests/"+id, nil)
	detail := response["request"].(map[string]any)
	if detail["raw"] != raw || detail["masked"] != masked["text"] || detail["upstream_response"] != "reply {{EMAIL_0000000000000001}} done" || detail["restored_response"] != "reply alice@example.com done" {
		t.Fatal("four-stage capture mismatch", detail)
	}
	if detail["status"] != "201" || detail["ingress"] != "ext" || detail["restored_count"] != float64(1) || detail["restore_status"] != "restored" {
		t.Fatal("completion evidence mismatch", detail)
	}
	if strings.Contains(detail["path"].(string), "private=query") {
		t.Fatal("query persisted in route metadata")
	}
	events, err := store.ListAllEvents(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("duplicate/missing events: %d", len(events))
	}
	for _, event := range events {
		if event.RequestID != id {
			t.Fatal("event not linked to request", event.RequestID)
		}
		view := map[string]any{"type": event.Type}
		s.attachRequestDetail(view, event.RequestID)
		if view["raw"] != raw || view["restored_response"] != detail["restored_response"] {
			t.Fatal("event detail lacks ledger bodies", view)
		}
	}
}

func TestExtensionLedgerRecordsNoMatchAndUnknownHTTPStatus(t *testing.T) {
	s, _ := extensionLedgerServer(t, contextualTestEngine{})
	masked := ledgerMask(t, s, "nothing sensitive here")
	code, _ := extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{"sid": masked["sid"], "stream_id": "plain", "text": "plain response", "final": true})
	if code != 200 {
		t.Fatal(code)
	}
	_, response := extensionLedgerCall(t, s, "GET", "/api/requests/"+masked["request_id"].(string), nil)
	detail := response["request"].(map[string]any)
	if detail["status"] != "completed" || detail["upstream_status_observed"] != false || detail["mask_count"] != float64(0) || detail["restored_response"] != "plain response" {
		t.Fatal(detail)
	}
}

func TestExtensionLedgerIncompleteOutcomesNeverClaimRestoration(t *testing.T) {
	for _, outcome := range []string{"cancelled", "network_error", "restore_failed", "unsupported_response", "response_stream_error", "mask_not_applied", "response_not_completed", "gateway_shutdown"} {
		t.Run(outcome, func(t *testing.T) {
			s, store := extensionLedgerServer(t, contextualTestEngine{})
			masked := ledgerMask(t, s, "alice@example.com")
			if outcome == "response_not_completed" {
				s.extMu.Lock()
				s.extRestore[masked["sid"].(string)].lastUsed = time.Now().Add(-time.Hour)
				s.reapExpiredExtensionRestoresLocked(time.Now())
				s.extMu.Unlock()
			} else if outcome == "gateway_shutdown" {
				_ = s.Shutdown(context.Background())
			} else {
				extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{"sid": masked["sid"], "stream_id": "end", "text": "", "final": true, "outcome": outcome, "response_status": 200})
			}
			item, err := store.GetRequest(context.Background(), masked["request_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if item.Error != outcome || item.EndedAt.IsZero() {
				t.Fatal(item)
			}
			view := requestItem(item)
			if view["restored_count"] != 0 || view["restore_status"] != "incomplete" {
				t.Fatal(view)
			}
			if s.logRequestActive(item.ID) {
				t.Fatal("finished extension retained a live lease")
			}
		})
	}
}

func TestExtensionLedgerHonorsLoggingSwitchesAndRecordsMaskFailure(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		s, store := extensionLedgerServer(t, contextualTestEngine{})
		s.control["ext_record_events"] = false
		masked := ledgerMask(t, s, "alice@example.com")
		if masked["request_id"] != "" {
			t.Fatal(masked)
		}
		extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{"sid": masked["sid"], "stream_id": "off", "text": masked["text"], "final": true})
		rows, _ := store.ListRequestSummaries(context.Background(), 10)
		events, _ := store.ListAllEvents(context.Background(), 10)
		if len(rows)+len(events) != 0 {
			t.Fatal("logging switch ignored")
		}
	})
	t.Run("detail_off", func(t *testing.T) {
		s, store := extensionLedgerServer(t, contextualTestEngine{})
		s.control["record_detail_data"] = false
		masked := ledgerMask(t, s, "alice@example.com")
		extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{"sid": masked["sid"], "stream_id": "off", "text": masked["text"], "final": true})
		item, _ := store.GetRequest(context.Background(), masked["request_id"].(string))
		if len(item.Raw) != 0 || requestMeta(item)["body_store"] == true {
			t.Fatal("raw detail retained")
		}
		_, response := extensionLedgerCall(t, s, "GET", "/api/requests/"+item.ID, nil)
		if response["request"].(map[string]any)["raw"] != nil {
			t.Fatal("disabled body exposed")
		}
	})
	t.Run("mask_failure", func(t *testing.T) {
		s, store := extensionLedgerServer(t, testEngine{})
		code, _ := extensionLedgerCall(t, s, "POST", "/api/ext/mask", map[string]any{"text": "alice@example.com"})
		if code != http.StatusBadGateway {
			t.Fatal(code)
		}
		rows, err := store.ListRequestSummaries(context.Background(), 10)
		if err != nil || len(rows) != 1 || rows[0].Error != "mask_failed" {
			t.Fatal(err, rows)
		}
	})
}

func TestExtensionLedgerWaitsForBrowserConfirmation(t *testing.T) {
	for _, outcome := range []string{"completed", "restore_failed", "cancelled", "response_not_completed"} {
		t.Run(outcome, func(t *testing.T) {
			s, store := extensionLedgerServer(t, contextualTestEngine{})
			masked := ledgerMask(t, s, "alice@example.com")
			id := masked["request_id"].(string)
			frame := map[string]any{"sid": masked["sid"], "stream_id": "confirmed", "text": "{{EMAIL_0000000000000001}}", "final": true, "await_ack": true, "response_status": 201}
			code, response := extensionLedgerCall(t, s, "POST", "/api/ext/restore", frame)
			if code != 200 || response["text"] != "alice@example.com" {
				t.Fatal(code, response)
			}
			item, _ := store.GetRequest(context.Background(), id)
			if item.Status != "awaiting_response" || !item.EndedAt.IsZero() {
				t.Fatal("restoration finalized before receipt", item)
			}
			if outcome == "response_not_completed" {
				s.extMu.Lock()
				s.extRestore[masked["sid"].(string)].lastUsed = time.Now().Add(-time.Hour)
				s.reapExpiredExtensionRestoresLocked(time.Now())
				s.extMu.Unlock()
			} else {
				frame["await_ack"], frame["text"], frame["outcome"] = false, "", outcome
				code, _ = extensionLedgerCall(t, s, "POST", "/api/ext/restore", frame)
				if code != 200 {
					t.Fatal(code)
				}
			}
			item, _ = store.GetRequest(context.Background(), id)
			view := requestItem(item)
			if item.EndedAt.IsZero() || s.logRequestActive(id) {
				t.Fatal("confirmation did not release ledger", item)
			}
			if outcome == "completed" {
				if item.Status != "201" || view["restored_count"] != 1 {
					t.Fatal("confirmation duplicated response evidence", view)
				}
			} else if item.Error != outcome || view["restore_status"] != "incomplete" {
				t.Fatal(view)
			}
			// A late success must not overwrite cancellation/failure.
			frame["await_ack"], frame["text"], frame["outcome"] = false, "", "completed"
			code, _ = extensionLedgerCall(t, s, "POST", "/api/ext/restore", frame)
			if code == 200 {
				t.Fatal("terminal ledger accepted another completion")
			}
		})
	}
}

func TestExtensionLedgerKeepsInFlightLoggingDecision(t *testing.T) {
	s, store := extensionLedgerServer(t, contextualTestEngine{})
	masked := ledgerMask(t, s, "alice@example.com")
	s.configMu.Lock()
	s.control["ext_record_events"] = false
	s.configMu.Unlock()
	extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{"sid": masked["sid"], "stream_id": "end", "text": masked["text"], "final": true})
	events, err := store.ListAllEvents(context.Background(), 10)
	if err != nil || len(events) != 2 {
		t.Fatal("in-flight MASK/RESTORE pair lost", err, events)
	}
	if next := ledgerMask(t, s, "alice@example.com"); next["request_id"] != "" {
		t.Fatal("new request ignored disabled setting", next)
	}
}

func TestExtensionMaskEventFlagReflectsStorageFailure(t *testing.T) {
	s, store := extensionLedgerServer(t, contextualTestEngine{})
	record, err := s.newExtensionRecord(context.Background(), "hello", "example.com", "", "POST")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	s.recordExtensionMask(record, []byte("hello"), nil)
	if record.maskEventRecorded {
		t.Fatal("failed MASK insertion claimed success")
	}
	record.finish(0, extensionLogError("test_end"))
}

type blockingExtensionEngine struct {
	contextualTestEngine
	maskStarted, maskContinue       chan struct{}
	restoreStarted, restoreContinue chan struct{}
	released                        atomic.Int32
}

type blockingExtensionMask struct {
	*contextualTestMaskSession
	engine *blockingExtensionEngine
}

type blockingExtensionRestore struct {
	RestoreSession
	engine *blockingExtensionEngine
}

func (e *blockingExtensionEngine) NewMaskingSession(context.Context) (MaskingSession, error) {
	return &blockingExtensionMask{&contextualTestMaskSession{originals: map[string]string{}}, e}, nil
}

func (s *blockingExtensionMask) Push(ctx context.Context, body []byte) ([]byte, error) {
	if s.engine.maskStarted != nil {
		close(s.engine.maskStarted)
		<-s.engine.maskContinue
	}
	return s.contextualTestMaskSession.Push(ctx, body)
}

func (s *blockingExtensionMask) NewRestoreSession(ctx context.Context) (RestoreSession, error) {
	restore, err := s.contextualTestMaskSession.NewRestoreSession(ctx)
	return &blockingExtensionRestore{restore, s.engine}, err
}

func (s *blockingExtensionMask) Release() { s.engine.released.Add(1) }

func (s *blockingExtensionRestore) Push(ctx context.Context, body []byte) ([]byte, error) {
	if s.engine.restoreStarted != nil {
		close(s.engine.restoreStarted)
		<-s.engine.restoreContinue
	}
	return s.RestoreSession.Push(ctx, body)
}

func (s *blockingExtensionRestore) Abort() {
	if aborter, ok := s.RestoreSession.(AbortableRestoreSession); ok {
		aborter.Abort()
	}
}

func TestExtensionShutdownDeadlineAndLateMaskRegistration(t *testing.T) {
	for _, phase := range []string{"mask", "restore"} {
		t.Run(phase, func(t *testing.T) {
			started, resume := make(chan struct{}), make(chan struct{})
			engine := &blockingExtensionEngine{}
			if phase == "mask" {
				engine.maskStarted, engine.maskContinue = started, resume
			} else {
				engine.restoreStarted, engine.restoreContinue = started, resume
			}
			s, store := extensionLedgerServer(t, engine)
			path, body := "/api/ext/mask", map[string]any{"text": "alice@example.com"}
			if phase == "restore" {
				masked := ledgerMask(t, s, "alice@example.com")
				path, body = "/api/ext/restore", map[string]any{"sid": masked["sid"], "stream_id": "blocked", "text": "{{EMAIL_0000000000000001}}", "final": false}
			}
			raw, _ := json.Marshal(body)
			request := httptest.NewRequest("POST", path, bytes.NewReader(raw))
			request.Header.Set("X-Silencer-Token", "test-token")
			finished := make(chan struct{})
			go func() { s.Handler().ServeHTTP(httptest.NewRecorder(), request); close(finished) }()
			defer func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
				<-finished
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("engine did not start")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			shutdown := make(chan error, 1)
			go func() { shutdown <- s.Shutdown(ctx) }()
			select {
			case err := <-shutdown:
				if err != context.DeadlineExceeded {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown deadline blocked on active extension")
			}
			close(resume)
			<-finished
			deadline := time.Now().Add(time.Second)
			for {
				rows, err := store.ListRequestSummaries(context.Background(), 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) == 1 && rows[0].Error == "gateway_shutdown" && !s.logRequestActive(rows[0].ID) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("shutdown left orphan ledger", rows)
				}
				time.Sleep(time.Millisecond)
			}
			s.extMu.Lock()
			remaining := len(s.extRestore)
			s.extMu.Unlock()
			if remaining != 0 || engine.released.Load() != 1 {
				t.Fatal("session owner leaked", remaining, engine.released.Load())
			}
		})
	}
}

func TestExtensionShutdownNotBlockedByMaskRegistrationLock(t *testing.T) {
	s, store := extensionLedgerServer(t, contextualTestEngine{})
	// Model map contention independently of engine latency. Registration must
	// never keep lifecycleMu held while waiting on the extension map.
	s.extMu.Lock()
	// The held map represents another admitted handler; keep the drain signal
	// open even if shutdown rejects the MASK just before it reaches the map.
	s.requests.begin()
	var holderEnded sync.Once
	endHolder := func() { holderEnded.Do(s.requests.end) }
	var unlocked sync.Once
	unlock := func() { unlocked.Do(s.extMu.Unlock) }
	defer unlock()
	request := httptest.NewRequest("POST", "/api/ext/mask", strings.NewReader(`{"text":"alice@example.com"}`))
	request.Header.Set("X-Silencer-Token", "test-token")
	finished := make(chan struct{})
	go func() { s.Handler().ServeHTTP(httptest.NewRecorder(), request); close(finished) }()
	defer func() { unlock(); endHolder(); <-finished }()
	deadline := time.Now().Add(time.Second)
	for {
		rows, _ := store.ListRequestSummaries(context.Background(), 10)
		if len(rows) == 1 && rows[0].Status == "awaiting_response" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mask did not reach registration")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- s.Shutdown(ctx) }()
	select {
	case err := <-shutdown:
		if err != context.DeadlineExceeded {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("registration map lock blocked shutdown deadline")
	}
	unlock()
	<-finished
	endHolder()
	deadline = time.Now().Add(time.Second)
	for {
		rows, _ := store.ListRequestSummaries(context.Background(), 10)
		if len(rows) == 1 && rows[0].Error == "gateway_shutdown" && !s.logRequestActive(rows[0].ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late registration escaped shutdown cleanup", rows)
		}
		time.Sleep(time.Millisecond)
	}
}
