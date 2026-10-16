package gateway

import (
	"fmt"
	"strings"

	"github.com/silencer/gateway/internal/storage"
)

type logNoiseRule struct{ method, path string }

func defaultLogNoiseAllowlist() []any {
	return []any{
		map[string]any{"method": "POST", "path": "/r/sub2api/api/show", "status": float64(404)},
		map[string]any{"method": "GET", "path": "/r/sub2api/api/tags", "status": float64(404)},
	}
}

// These rules affect presentation only. No route, response, masking or stored
// outcome is changed. Invalid persisted rules fail closed as ordinary errors.
func parseLogNoiseAllowlist(value any) ([]logNoiseRule, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 100 {
		return nil, fmt.Errorf("log_noise_allowlist must be an array of at most 100 exact 404 rules")
	}
	rules := make([]logNoiseRule, 0, len(list))
	seen := map[logNoiseRule]bool{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok || len(item) != 3 {
			return nil, fmt.Errorf("log_noise_allowlist entries require only method, path and status")
		}
		method, _ := item["method"].(string)
		path, _ := item["path"].(string)
		code, validCode := numericMetaInt(item["status"])
		validMethod := method == "GET" || method == "POST" || method == "HEAD" || method == "OPTIONS" || method == "PUT" || method == "PATCH" || method == "DELETE"
		if n, ok := item["status"].(float64); ok && n != 404 {
			validCode = false
		}
		if !validMethod || !validCode || code != 404 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || len(path) > 2048 || strings.ContainsAny(path, "?#*\\ \t\r\n") {
			return nil, fmt.Errorf("log_noise_allowlist requires an uppercase HTTP method, exact absolute path (no wildcard/query/fragment), and status 404")
		}
		for _, c := range path {
			if c < 32 || c == 127 {
				return nil, fmt.Errorf("log_noise_allowlist path contains a control character")
			}
		}
		rule := logNoiseRule{method, path}
		if seen[rule] {
			return nil, fmt.Errorf("log_noise_allowlist contains duplicate rules")
		}
		seen[rule], rules = true, append(rules, rule)
	}
	return rules, nil
}

func (s *Server) logNoiseRules() []logNoiseRule {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	rules, _ := parseLogNoiseAllowlist(s.control["log_noise_allowlist"])
	return rules
}

func matchesLogNoise(rules []logNoiseRule, status, failure string, meta map[string]any) bool {
	code, _ := numericMetaInt(meta["http_status"])
	if code == 0 {
		code = ledgerHTTPStatus(status)
	}
	if code != 404 || failure != "" || status == "error" || status == "canceled" || meta["ingress"] != "proxy" || meta["failure_stage"] != nil && meta["failure_stage"] != "" {
		return false
	}
	for _, key := range []string{"unresolved", "restore_unresolved"} {
		if count, _ := numericMetaInt(meta[key]); count > 0 {
			return false
		}
	}
	// Conflicting recorded status must never be suppressed by metadata.
	if recorded := ledgerHTTPStatus(status); recorded != 0 && recorded != 404 {
		return false
	}
	for _, rule := range rules {
		if meta["method"] == rule.method && meta["path"] == rule.path {
			return true
		}
	}
	return false
}

func (s *Server) projectLogNoise(event storage.Event) storage.Event {
	return projectLogNoise(event, s.logNoiseRules())
}

func projectLogNoise(event storage.Event, rules []logNoiseRule) storage.Event {
	meta := cloneReadMetadata(eventMeta(event))
	meta["noise_whitelisted"] = matchesLogNoise(rules, event.Status, event.Error, meta)
	event.StatsMeta = meta
	return event
}
