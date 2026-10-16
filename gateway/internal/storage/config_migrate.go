package storage

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigMigrationReport describes the fields copied from a legacy JSON
// configuration. Secrets are deliberately counted as dropped fields and are
// never written to either destination file.
type ConfigMigrationReport struct {
	RuntimeKeys []string `json:"runtime_keys"`
	ControlKeys []string `json:"control_keys"`
	DroppedKeys []string `json:"dropped_keys"`
}

// MigrateConfigFile splits a legacy monolithic config into the Go runtime
// config and the control-plane sidecar config. Both files are replaced
// atomically and parent directories are created when needed.
func MigrateConfigFile(sourcePath, destinationPath string) (ConfigMigrationReport, error) {
	var report ConfigMigrationReport
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return report, fmt.Errorf("read config: %w", err)
	}
	var source map[string]any
	if err := json.Unmarshal(data, &source); err != nil {
		return report, fmt.Errorf("parse config: %w", err)
	}
	if source == nil {
		return report, fmt.Errorf("config must be a JSON object")
	}
	runtime := map[string]any{}
	control := map[string]any{}
	runtimeKeys := map[string]bool{
		"listen_addr": true, "upstream_url": true, "max_concurrency": true,
		"max_body_bytes": true, "max_response_bytes": true, "shutdown_timeout_seconds": true, "engine": true,
		"engine_library": true, "web_dist": true,
	}
	controlKeys := map[string]bool{
		"upstreams": true, "client_profiles": true, "fail_closed": true,
		"filter_enabled": true, "response_scan": true, "origin_check": true,
		"record_plaintext_words": true, "record_detail_data": true, "log_retention_days": true, "log_noise_allowlist": true, "stream_response": true,
		"stream_exclude_hosts": true, "stop_mode": true, "capture_mode": true,
		"egress_proxy": true, "audit": true, "builtin_rules": true,
		"sensitive": true, "sensitive_disabled": true, "sensitive_word_disabled": true,
		"sensitive_word_whole": true, "secret_prefixes": true, "session_ttl": true,
	}
	for key, value := range source {
		switch {
		case isSecretConfigKey(key):
			report.DroppedKeys = append(report.DroppedKeys, key)
		case runtimeKeys[key]:
			runtime[key] = sanitizeConfigValue(key, value, &report)
			report.RuntimeKeys = append(report.RuntimeKeys, key)
		case controlKeys[key]:
			control[key] = sanitizeConfigValue(key, value, &report)
			report.ControlKeys = append(report.ControlKeys, key)
		default:
			// Unknown legacy options are intentionally not silently treated as
			// runtime settings. Keep an audit trail in the command result.
			report.DroppedKeys = append(report.DroppedKeys, key)
		}
	}
	sort.Strings(report.RuntimeKeys)
	sort.Strings(report.ControlKeys)
	sort.Strings(report.DroppedKeys)
	if destinationPath == "" {
		return report, fmt.Errorf("config destination is required")
	}
	if err := writeJSONAtomic(destinationPath, runtime); err != nil {
		return report, fmt.Errorf("write runtime config: %w", err)
	}
	if err := writeJSONAtomic(destinationPath+".control.json", control); err != nil {
		return report, fmt.Errorf("write control config: %w", err)
	}
	return report, nil
}

func isSecretConfigKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	// Prefixes such as "sk-" identify credential shapes but are not secrets;
	// they are required by the masking rule library and safe to migrate.
	if lower == "secret_prefixes" {
		return false
	}
	for _, marker := range []string{"auth_token", "token", "password", "passwd", "secret", "api_key", "apikey", "mysql_dsn", "dsn"} {
		if lower == marker || strings.Contains(lower, marker) {
			return true
		}
	}
	return lower == "storage" || lower == "ext_token"
}

func sanitizeConfigValue(key string, value any, report *ConfigMigrationReport) any {
	switch object := value.(type) {
	case map[string]any:
		clone := map[string]any{}
		for child, childValue := range object {
			if isSecretConfigKey(child) {
				report.DroppedKeys = append(report.DroppedKeys, key+"."+child)
				continue
			}
			clone[child] = sanitizeConfigValue(key+"."+child, childValue, report)
		}
		return clone
	case []any:
		clone := make([]any, len(object))
		for i, child := range object {
			clone[i] = sanitizeConfigValue(fmt.Sprintf("%s[%d]", key, i), child, report)
		}
		return clone
	case string:
		leaf := key[strings.LastIndex(key, ".")+1:]
		if strings.EqualFold(leaf, "url") || strings.HasSuffix(strings.ToLower(leaf), "_url") {
			if parsed, err := url.Parse(object); err == nil && parsed.User != nil {
				parsed.User = nil
				report.DroppedKeys = append(report.DroppedKeys, key+".userinfo")
				return parsed.String()
			}
		}
	}
	return value
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".silencer-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
