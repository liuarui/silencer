//go:build silencer_engine && cgo

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeExtensionLedgerRecordsActualRestoration(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"alice@example.com","label":"EMAIL"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := extensionLedgerServer(t, engine)
	masked := ledgerMask(t, s, `{"prompt":"contact alice@example.com"}`)
	token := maskedTokenPattern.FindString(masked["text"].(string))
	if token == "" {
		t.Fatal("native request was not masked", masked)
	}
	code, restored := extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{
		"sid": masked["sid"], "stream_id": "native", "text": `{"reply":"` + token + `"}`,
		"content_type": "application/json", "final": true, "await_ack": true, "response_status": 200,
	})
	if code != 200 || !strings.Contains(restored["text"].(string), "alice@example.com") {
		t.Fatal(code, restored)
	}
	code, _ = extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{
		"sid": masked["sid"], "stream_id": "native", "text": "", "final": true, "outcome": "completed",
	})
	if code != 200 {
		t.Fatal("native confirmation failed", code)
	}
	_, response := extensionLedgerCall(t, s, "GET", "/api/requests/"+masked["request_id"].(string), nil)
	detail := response["request"].(map[string]any)
	if detail["restored_count"] != float64(1) || detail["restore_status"] != "restored" || !strings.Contains(detail["restored_response"].(string), "alice@example.com") {
		t.Fatal(detail)
	}
}

func TestNativeExtensionDurableScopeOutlivesMaskHTTPContext(t *testing.T) {
	original := "测试 \"quoted\" \\path\ncontact@example.invalid"
	rules, _ := json.Marshal(map[string]any{"rules": []map[string]string{{"needle": original, "label": "TEST"}}})
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: string(rules)})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := extensionLedgerServer(t, engine)
	scope := "1234567890abcdef1234567890abcdef"
	mask := func(text, host, identity string) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"text": text, "host": host, "scope": identity})
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest("POST", "/api/ext/mask", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("X-Silencer-Token", "test-token")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, req)
		cancel() // Real net/http cancels the mask request before any restore RPC.
		if response.Code != 200 {
			t.Fatalf("mask status %d", response.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := mask(original, "chatgpt.com", scope)
	token := maskedTokenPattern.FindString(first["text"].(string))
	if token == "" {
		t.Fatal("native test input was not masked")
	}
	for _, test := range []struct {
		host, scope string
		expected    bool
	}{
		{"chatgpt.com", scope, true},
		{"another-origin.example", scope, false},
		{"chatgpt.com", "ffffffffffffffffffffffffffffffff", false},
	} {
		next := mask("[]", test.host, test.scope)
		code, reply := extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{
			"sid": next["sid"], "stream_id": "logical-values", "text": token,
			"content_type": "text/plain", "value_complete": true,
		})
		if code != 200 || (reply["text"] == original) != test.expected {
			t.Fatalf("durable value restoration/isolation failed: status=%d expected=%v", code, test.expected)
		}
		code, final := extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{
			"sid": next["sid"], "stream_id": "logical-values", "text": "", "final": true, "await_ack": true,
		})
		if code != 200 || final["text"] != "" {
			t.Fatal("value channel leaked buffered text at stream finalization")
		}
		extensionLedgerCall(t, s, "POST", "/api/ext/restore", map[string]any{
			"sid": next["sid"], "stream_id": "logical-values", "text": "", "final": true,
		})
	}
}
