package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateConfigFileSplitsRuntimeAndControlAndDropsSecrets(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy.json")
	destination := filepath.Join(dir, "go.json")
	legacy := map[string]any{
		"listen_addr":           "127.0.0.1:9000",
		"upstream_url":          "http://upstream-user:password@127.0.0.1:9001",
		"max_concurrency":       float64(32),
		"fail_closed":           true,
		"audit":                 map[string]any{"enabled": true},
		"egress_proxy":          map[string]any{"enabled": true, "url": "http://user:password@example.test:8080"},
		"auth_token":            "do-not-copy",
		"storage":               map[string]any{"mysql_dsn": "user:password@tcp(db)/x"},
		"unknown_legacy_option": "drop-me",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := MigrateConfigFile(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	runtimeData, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	controlData, err := os.ReadFile(destination + ".control.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(runtimeData) == "" || string(controlData) == "" {
		t.Fatal("migration wrote empty config")
	}
	if contains := string(runtimeData) + string(controlData); contains == "" ||
		json.Valid(runtimeData) == false || json.Valid(controlData) == false {
		t.Fatalf("invalid output: runtime=%s control=%s", runtimeData, controlData)
	}
	if string(runtimeData) == string(controlData) ||
		containsString(string(runtimeData), "do-not-copy") || containsString(string(controlData), "do-not-copy") ||
		containsString(string(runtimeData), "upstream-user:password") || containsString(string(controlData), "user:password") {
		t.Fatalf("secret leaked into config: runtime=%s control=%s", runtimeData, controlData)
	}
	if len(report.RuntimeKeys) != 3 || len(report.ControlKeys) != 3 || len(report.DroppedKeys) < 4 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestMigrateConfigFilePreservesRuleLibrary(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy-rules.json")
	destination := filepath.Join(dir, "go-rules.json")
	legacy := map[string]any{
		"sensitive":               map[string]any{"PEOPLE": []any{"Alice"}},
		"sensitive_disabled":      []any{"PEOPLE"},
		"sensitive_word_disabled": map[string]any{"PEOPLE": []any{"Alice"}},
		"sensitive_word_whole":    []any{"Alice"},
		"secret_prefixes":         []any{"sk-"},
		"builtin_rules":           map[string]any{"PHONE": true},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateConfigFile(source, destination); err != nil {
		t.Fatal(err)
	}
	controlData, err := os.ReadFile(destination + ".control.json")
	if err != nil {
		t.Fatal(err)
	}
	var control map[string]any
	if err := json.Unmarshal(controlData, &control); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sensitive", "sensitive_disabled", "sensitive_word_disabled", "sensitive_word_whole", "secret_prefixes", "builtin_rules"} {
		if _, ok := control[key]; !ok {
			t.Fatalf("migrated control omitted %s: %s", key, controlData)
		}
	}
}

func containsString(value, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
