package gateway

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func (r *requestRecord) recordToolDecision(findings []toolFinding, mode string) {
	if r == nil || len(findings) == 0 {
		return
	}
	var evidence []string
	var rules []any
	for _, f := range findings {
		evidence = append(evidence, toolFindingEvidence(f))
		rules = append(rules, f.rule)
	}
	r.mu.Lock()
	meta := requestMeta(r.item)
	meta["tool_guard_action"] = mode
	meta["tool_guard_rules"] = rules
	meta["confirmation_required"] = mode != "observe"
	encoded, _ := json.Marshal(meta)
	r.item.Meta = string(encoded)
	id, path := r.item.ID, meta["path"]
	method := meta["method"]
	r.mu.Unlock()
	if r.store == nil {
		return
	}
	eventMeta := map[string]any{"signal_type": "dangerous_action", "severity": "HIGH", "evidence": strings.Join(evidence, "\n"), "action": mode, "confirmation_required": mode != "observe", "tool_guard_rules": rules, "path": path, "method": method, "ingress": "proxy"}
	encoded, _ = json.Marshal(eventMeta)
	r.appendLogEvent(storage.Event{ID: newRequestID(), RequestID: id, Type: "AUDIT_dangerous_action", Payload: []byte{}, Status: mode, Meta: string(encoded), CreatedAt: time.Now().UTC()})
}
