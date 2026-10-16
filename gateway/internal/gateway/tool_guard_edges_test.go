package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolDangerBoundaryCases(t *testing.T) {
	for _, cmd := range []string{"rm *", "rm /srv/data/*", "rm -f /srv/data/*", `"rm" -rf /srv/data`, `r'm' -rf /srv/data`, "DELETE FROM users WHERE 1=1", "UPDATE users SET active=false WHERE TRUE", `UPDATE "users" SET name='where'`, "echo $(rm -rf /srv/data)"} {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		if len(inspectTool("terminal", string(args), false)) == 0 {
			t.Fatalf("missed: %s", cmd)
		}
	}
	for _, cmd := range []string{`echo "rm -rf /srv/data"`, `printf '%s' 'DROP DATABASE production'`, `SELECT 'DROP DATABASE production'`, `DELETE FROM users WHERE 1=1 AND id=42`, `DELETE FROM users WHERE name='abc--xyz'`, `UPDATE users SET note='/* WHERE */' WHERE id=3`, `git commit -m "do not run rm -rf /"`} {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		if got := inspectTool("terminal", string(args), false); len(got) > 0 {
			t.Fatalf("false positive %s: %+v", cmd, got)
		}
	}
	for _, tc := range []struct{ name, args string }{{"delete_directory", `{"path":"/srv/data","options":{"recursive":true}}`}, {"delete_many", `{"options":{"filter":{}}}`}} {
		if len(inspectTool(tc.name, tc.args, false)) == 0 {
			t.Fatal("nested deletion missed", tc)
		}
	}
}

func TestToolGuardIdentityCannotBeOverwritten(t *testing.T) {
	for _, wire := range []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"delete_all","arguments":"{}"}}]}}]}` + "\n\n" + `data: {"choices":[{"index":0,"message":{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"delete_","arguments":"{"}}]}}]}` + "\n\n" + `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"all","arguments":"}"}}]}}]}` + "\n\n",
		"event: content_block_start\ndata: " + `{"index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"delete_all","input":{}}}` + "\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_stop\ndata: {}\n\n",
	} {
		for _, mode := range []string{"rewrite", "block"} {
			out, findings := runToolGuard(t, "sse", mode, wire)
			if len(findings) == 0 || !strings.Contains(string(out), "请再次确认") {
				t.Fatalf("identity bypass %s: %s", mode, out)
			}
		}
	}
}

func TestToolGuardResponsesPrefixAndSequence(t *testing.T) {
	prior := map[string]any{"id": "msg_prior", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "existing text"}}}
	tool := map[string]any{"id": "fc_1", "type": "function_call", "name": "terminal", "arguments": `{"command":"rm -rf /srv/data"}`}
	wire := guardWire("sse", map[string]any{"type": "response.output_item.done", "sequence_number": 99, "output_index": 0, "item": prior}, map[string]any{"type": "response.output_item.added", "sequence_number": 100, "output_index": 1, "item": tool}, map[string]any{"type": "response.completed", "sequence_number": 101, "response": map[string]any{"id": "resp_test", "output": []any{prior, tool}}})
	out, _ := runToolGuard(t, "sse", "rewrite", wire)
	parser := protocolRestoreSession{kind: "sse", pending: out}
	last := -1
	replacementIndex := -1
	for len(parser.pending) > 0 {
		end := parser.frameEnd()
		if end == 0 {
			end = len(parser.pending)
		}
		obj, _, err := decodeToolGuardFrame("sse", parser.pending[:end])
		parser.pending = parser.pending[end:]
		if err != nil {
			t.Fatal(err)
		}
		if obj == nil {
			continue
		}
		seq, ok := numericMetaInt(obj["sequence_number"])
		if !ok || seq <= last {
			t.Fatalf("sequence moved backwards %d -> %d: %s", last, seq, out)
		}
		last = seq
		if obj["type"] == "response.output_item.added" {
			replacementIndex, _ = numericMetaInt(obj["output_index"])
		}
		if obj["type"] == "response.completed" {
			items := obj["response"].(map[string]any)["output"].([]any)
			if len(items) != 2 || replacementIndex != 1 || items[0].(map[string]any)["id"] != "msg_prior" {
				t.Fatalf("inconsistent final snapshot: %s", out)
			}
		}
	}
}

func TestToolGuardParallelCallsRemainIndependent(t *testing.T) {
	wire := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"terminal","arguments":"{\"command\":\"rm -"}},{"index":1,"function":{"name":"terminal","arguments":"{\"command\":\"ls"}}]}}]}` + "\n\n" + `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":" -lah\"}"}},{"index":0,"function":{"arguments":"rf /srv/data\"}"}}]}}]}` + "\n\ndata: [DONE]\n\n"
	out, findings := runToolGuard(t, "sse", "rewrite", wire)
	if len(findings) == 0 || strings.Contains(string(out), `"tool_calls"`) {
		t.Fatalf("parallel calls leaked: %s", out)
	}
}

func TestToolGuardInspectionLimitsFailClosed(t *testing.T) {
	var wire strings.Builder
	for i := 0; i < 130; i++ {
		wire.WriteString(guardWire("sse", map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"name": "safe", "arguments": "{}"}}}}}}}))
	}
	for _, mode := range []string{"rewrite", "block"} {
		out, findings := runToolGuard(t, "sse", mode, wire.String())
		if len(findings) == 0 || strings.Contains(string(out), `"tool_calls"`) || !strings.Contains(string(out), "silencer_tool_blocked") {
			t.Fatalf("overflow released calls: %s", out)
		}
	}
}

func TestToolGuardModePersistsAndErrorLeakSwitchIsIndependent(t *testing.T) {
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", filepath.Join(t.TempDir(), "control.json"))
	s, err := NewServer(Config{Engine: "go-fallback", MaxConcurrency: 1}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	for _, mode := range []string{"block", "rewrite", "observe"} {
		body := fmt.Sprintf(`{"key":"audit","op":"merge","path":[],"value":{"tool_guard":{"enabled":true,"mode":%q}}}`, mode)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/config/patch", strings.NewReader(body)))
		if rr.Code != 200 {
			t.Fatalf("save failed: %s", rr.Body.String())
		}
		restarted, err := NewServer(Config{Engine: "go-fallback", MaxConcurrency: 1}, testEngine{})
		if err != nil {
			t.Fatal(err)
		}
		policy := restarted.toolGuardSettings()
		restarted.Shutdown(context.Background())
		if policy.mode != mode || !policy.enabled {
			t.Fatalf("restart lost selection: %+v", policy)
		}
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/config/patch", strings.NewReader(`{"key":"audit","op":"merge","path":[],"value":{"enabled":false,"passive":false,"signals":{"error_leak":false}}}`)))
	if rr.Code != 200 || !s.toolGuardSettings().enabled {
		t.Fatal("error-leak switch disabled tool guard")
	}
}
