package gateway

import (
	"encoding/json"
	"testing"
)

func TestContextualCredentialRuleCatalogAndOrder(t *testing.T) {
	control := defaultControlConfig()
	builtin := control["builtin_rules"].(map[string]any)
	meta := builtinRuleMeta()
	labels := []string{"BASIC_AUTH", "COOKIE_SESSION", "URL_QUERY_SECRET"}
	for _, label := range labels {
		if enabled, ok := builtin[label].(bool); !ok || enabled {
			t.Fatalf("new rule %s must be present and disabled by default", label)
		}
		if description, _ := meta[label].(string); description == "" {
			t.Fatalf("rule %s has no description", label)
		}
		builtin[label] = true
	}
	control["sensitive"] = map[string]any{"CUSTOM": []any{"demo"}}
	raw, err := engineConfigFromControl(control)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Rules []struct {
			Label        string `json:"label"`
			Pattern      string `json:"pattern"`
			CaptureGroup int    `json:"capture_group"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Rules) < len(labels) {
		t.Fatalf("missing contextual rules: %s", raw)
	}
	for i, label := range labels {
		rule := config.Rules[i]
		if rule.Label != label || rule.Pattern == "" || rule.CaptureGroup != 1 {
			t.Fatalf("rule %d must mask the complete %s value first: %#v", i, label, rule)
		}
		count := 0
		for _, rule := range config.Rules {
			if rule.Label == label {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("rule %s generated %d times, want 1", label, count)
		}
	}
}
