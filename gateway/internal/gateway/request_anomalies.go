package gateway

import (
	"strings"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

type requestAnomaly struct {
	kind string
	at   time.Time
}

func anomalyPriority(kind string) int {
	switch kind {
	case "block":
		return 4
	case "restore_failed":
		return 3
	case "error":
		return 2
	case "response_warning":
		return 1
	}
	return 0
}

func outcomeAnomaly(status, failure string, meta map[string]any, rules []logNoiseRule) string {
	status = canonicalRestoreStatus(status, meta)
	if matchesLogNoise(rules, status, failure, meta) {
		return ""
	}
	if strings.EqualFold(status, "blocked") || meta["tool_guard_action"] == "block" {
		return "block"
	}
	for _, key := range []string{"restore_unresolved", "unresolved"} {
		if count, _ := numericMetaInt(meta[key]); count > 0 {
			return "restore_failed"
		}
	}
	for _, value := range []any{status, meta["restore_status"]} {
		if value == "unresolved" || value == "restore_skipped" {
			return "restore_failed"
		}
	}
	if failure == "restore_failed" {
		return "restore_failed"
	}
	code := ledgerHTTPStatus(status)
	metaCode, _ := numericMetaInt(meta["http_status"])
	if failure != "" || status == "error" || status == "failed" || status == "canceled" || status == "cancelled" || code >= 400 || metaCode >= 400 {
		return "error"
	}
	if stage, _ := meta["failure_stage"].(string); stage != "" {
		return "error"
	}
	if meta["tool_guard_action"] == "rewrite" || meta["tool_guard_action"] == "observe" {
		return "response_warning"
	}
	return ""
}

// Each request contributes at most one primary category. Stored phases and
// audit signals enrich that request instead of incrementing separate totals.
func classifyRequestAnomalies(requests []storage.Request, events []storage.Event, rules []logNoiseRule) map[string]requestAnomaly {
	result := make(map[string]requestAnomaly)
	requestTimes := make(map[string]time.Time, len(requests))
	latest := make(map[string]storage.Event)
	for _, event := range events {
		typ := strings.ToUpper(strings.TrimSpace(event.Type))
		if typ != "MASK" && typ != "RESTORE" {
			continue
		}
		phase := eventRequestKey(event) + "\x00" + typ
		previous, exists := latest[phase]
		if !exists || event.CreatedAt.After(previous.CreatedAt) || event.CreatedAt.Equal(previous.CreatedAt) && event.ID > previous.ID {
			latest[phase] = event
		}
	}
	add := func(key, kind string, at time.Time) {
		if kind == "" {
			return
		}
		if requestAt, ok := requestTimes[key]; ok {
			at = requestAt
		}
		if previous, exists := result[key]; !exists || anomalyPriority(kind) > anomalyPriority(previous.kind) {
			result[key] = requestAnomaly{kind: kind, at: at}
		}
	}
	for _, item := range requests {
		requestTimes[item.ID] = item.CreatedAt
		_, hasRestore := latest[item.ID+"\x00RESTORE"]
		status, failure, meta := item.Status, item.Error, requestMeta(item)
		if hasRestore {
			status, failure, meta = withoutStaleRestoreFailure(status, failure, meta)
		}
		kind := outcomeAnomaly(status, failure, meta, rules)
		// Legacy statistics may have response bodies but no restoration counters.
		if !hasRestore && kind != "block" && inferredRestoreStatus(item, false) == "unresolved" {
			kind = "restore_failed"
		}
		add(item.ID, kind, item.CreatedAt)
	}
	for _, event := range events {
		typ := strings.ToUpper(strings.TrimSpace(event.Type))
		key := eventRequestKey(event)
		if typ == "MASK" || typ == "RESTORE" {
			continue
		}
		switch {
		case typ == "BLOCK":
			add(key, "block", event.CreatedAt)
		case typ == "SCAN_WARN" || typ == "AUDIT" || strings.HasPrefix(typ, "AUDIT_") || strings.Contains(typ, "ALERT"):
			add(key, "response_warning", event.CreatedAt)
		default:
			kind := outcomeAnomaly(event.Status, event.Error, eventMeta(event), rules)
			if kind == "" && (typ == "ERR" || typ == "CANCEL" || typ == "DNS_ERROR") {
				kind = "error"
			}
			add(key, kind, event.CreatedAt)
		}
	}
	for _, event := range latest {
		status, failure, meta := event.Status, event.Error, eventMeta(event)
		if strings.EqualFold(event.Type, "MASK") {
			if _, hasRestore := latest[eventRequestKey(event)+"\x00RESTORE"]; hasRestore {
				status, failure, meta = withoutStaleRestoreFailure(status, failure, meta)
			}
		}
		add(eventRequestKey(event), outcomeAnomaly(status, failure, meta, rules), event.CreatedAt)
	}
	return result
}

// Latest RESTORE evidence owns the restoration result. Keep independent HTTP,
// cancellation and transport failures from the request or MASK phase.
func withoutStaleRestoreFailure(status, failure string, meta map[string]any) (string, string, map[string]any) {
	copied := false
	for _, key := range []string{"restore_unresolved", "unresolved", "restore_status"} {
		if _, exists := meta[key]; exists {
			if !copied {
				meta = cloneReadMetadata(meta)
				copied = true
			}
			delete(meta, key)
		}
	}
	if status == "unresolved" || status == "restore_skipped" {
		status = ""
	}
	if failure == "restore_failed" {
		failure = ""
	}
	return status, failure, meta
}

func anomalyCounts(anomalies map[string]requestAnomaly) map[string]int {
	counts := map[string]int{"block": 0, "restore_failed": 0, "error": 0, "response_warning": 0}
	for _, anomaly := range anomalies {
		counts[anomaly.kind]++
	}
	return counts
}
