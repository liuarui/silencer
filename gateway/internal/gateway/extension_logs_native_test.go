//go:build silencer_engine && cgo

package gateway

import (
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
