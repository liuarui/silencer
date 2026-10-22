package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

// defaultControlConfig is the public control-plane document. It intentionally
// contains only serializable configuration; secrets and process internals are
// kept out of the response.
func defaultControlConfig() map[string]any {
	return map[string]any{
		"upstreams": []any{}, "client_profiles": []any{},
		"fail_closed": true, "filter_enabled": true, "response_scan": true,
		"origin_check": true, "record_plaintext_words": true, "record_detail_data": true, "log_retention_days": 3, "ext_record_events": true, "stream_response": true,
		"stream_exclude_hosts": []any{}, "stop_mode": "error", "capture_mode": "reverse",
		"session_ttl": 600, "log_noise_allowlist": defaultLogNoiseAllowlist(),
		"sensitive": map[string]any{}, "sensitive_disabled": []any{},
		"sensitive_word_disabled": map[string]any{}, "sensitive_word_whole": []any{},
		"secret_prefixes": []any{"sk-", "ghp_", "AIza"},
		"builtin_rules":   defaultBuiltinRules(),
		"_meta":           map[string]any{"builtin_rule_meta": builtinRuleMeta(), "version": releaseVersion},
		"egress_proxy":    map[string]any{"enabled": false, "url": ""},
		"audit":           map[string]any{"enabled": false, "passive": true, "fail_closed": true, "severity_floor": "MEDIUM", "signals": map[string]any{}, "tool_guard": map[string]any{"enabled": true, "mode": "observe"}},
	}
}

// defaultBuiltinRules mirrors the previous runtime's inventory. Seven
// conservative PII/credential rules are enabled by default; higher-risk
// patterns remain available in the UI but start disabled.
func defaultBuiltinRules() map[string]any {
	return map[string]any{
		"API_KEY": true, "CARD": true, "CONNSTR": true, "EMAIL": true,
		"IDCARD": true, "LANDLINE": true, "PHONE": true,
		"ACCESS_KEY": false, "HKID": false, "IBAN": false,
		"IP_INTERNAL": false, "IP_PRIVATE": false, "IP_PUBLIC": false,
		"IPV6_PRIVATE": false, "JWT": false, "MAC": false, "PLATE": false,
		"PRIVATE_KEY": false, "SECRET": false, "TOKEN": false, "USCC": false,
		"COOKIE_SESSION": false, "BASIC_AUTH": false, "URL_QUERY_SECRET": false,
	}
}

func builtinRuleMeta() map[string]any {
	return map[string]any{
		"PRIVATE_KEY": "PEM 私钥（整块替换）", "CONNSTR": "连接串密码（只脱密码）",
		"PHONE": "手机号", "EMAIL": "邮箱", "LANDLINE": "座机号码",
		"PLATE": "车牌", "HKID": "港澳通行证", "IDCARD": "身份证",
		"IP_PRIVATE": "私网 IPv4", "IP_INTERNAL": "内网 IPv4", "IP_PUBLIC": "公网 IPv4",
		"IPV6_PRIVATE": "私网 IPv6", "CARD": "银行卡号", "IBAN": "IBAN",
		"USCC": "统一社会信用代码", "MAC": "MAC 地址", "API_KEY": "常见 API Key",
		"ACCESS_KEY": "云厂商访问密钥", "JWT": "JWT", "TOKEN": "Bearer Token", "SECRET": "凭据赋值形态",
		"COOKIE_SESSION":   "常见会话字段与登录 Cookie（只脱值）",
		"BASIC_AUTH":       "HTTP Basic 认证载荷（只脱值）",
		"URL_QUERY_SECRET": "URL 查询参数中的凭据与签名（只脱值）",
	}
}

// Match the known ASCII names in either case. Spell out case pairs because
// std::regex ECMAScript has no inline flag syntax.
func builtinNamePattern(names ...string) string {
	patterns := make([]string, 0, len(names))
	for _, name := range names {
		var pattern strings.Builder
		for _, char := range name {
			if char >= 'a' && char <= 'z' {
				pattern.WriteByte('[')
				pattern.WriteRune(char)
				pattern.WriteRune(char - 'a' + 'A')
				pattern.WriteByte(']')
			} else {
				pattern.WriteString(regexp.QuoteMeta(string(char)))
			}
		}
		patterns = append(patterns, pattern.String())
	}
	return "(?:" + strings.Join(patterns, "|") + ")"
}

// Built-in patterns use ECMAScript syntax supported by the C++ engine. They
// intentionally avoid lookbehind and engine-specific regex extensions.
func builtinRulePatterns(label string) []string {
	if label == "COOKIE_SESSION" {
		names := builtinNamePattern("session", "sessionid", "session_id", "session-id", "sid", "jsessionid", "phpsessid", "asp.net_sessionid", "connect.sid", "next-auth.session-token", "authjs.session-token", "__secure-next-auth.session-token", "__host-next-auth.session-token", "__secure-authjs.session-token", "__host-authjs.session-token", "__host-session", "__secure-session")
		// Ampersands and hashes can belong to cookie values. Exclude query
		// parameter boundaries so their separators are handled by the URL rule.
		return []string{`(?:^|[^A-Za-z0-9_.?&-])` + names + `["']?[ \t]*[:=][ \t]*["']?([^;\s"'\\,{}\[\]]{8,})`}
	}
	if label == "BASIC_AUTH" {
		names := builtinNamePattern("authorization", "proxy-authorization")
		return []string{`\b` + names + `["']?[ \t]*[:=][ \t]*["']?[Bb][Aa][Ss][Ii][Cc][ \t]+([A-Za-z0-9+/]+={0,2})(?:$|[ \t\r\n"'\\,;}])`}
	}
	if label == "URL_QUERY_SECRET" {
		names := builtinNamePattern("access_token", "accesstoken", "refresh_token", "id_token", "api_key", "api-key", "apikey", "token", "password", "passwd", "pwd", "client_secret", "secret", "signature", "sig", "x-amz-signature", "x-goog-signature", "x-amz-security-token")
		return []string{`[?&]` + names + `=([^&#\s"'<>\\{}\[\]]+)`}
	}
	patterns := map[string][]string{
		"PRIVATE_KEY": {`-----BEGIN (RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----[\s\S]{20,}-----END [^-]*PRIVATE KEY-----`},
		"API_KEY":     {`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{20,}`, `github_pat_[A-Za-z0-9_]{50,}`, `AIza[0-9A-Za-z_-]{35,38}`, `xox[baprs]-[0-9A-Za-z-]{10,}`, `[sr]k_(live|test)_[0-9A-Za-z]{20,}`, `cli_[a-z0-9]{16,}`, `ding[a-z0-9]{6,}`},
		"ACCESS_KEY":  {`(AKIA|ASIA)[A-Z0-9]{16}`, `LTAI[A-Za-z0-9]{12,20}`, `AKID[A-Za-z0-9]{13,32}`, `(aws_secret_access_key|AWS_SECRET_ACCESS_KEY)["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})`},
		"JWT":         {`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`},
		"TOKEN":       {`[Bb][Ee][Aa][Rr][Ee][Rr]\s+([A-Za-z0-9._~+/-]{20,}=*)`},
		// Keep this pattern within the C++ ECMAScript dialect: lookaround is
		// unsupported by std::regex and would make the whole engine fail to load.
		"SECRET":  {`((^|[^A-Za-z0-9_.])(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|密码|口令|令牌|密钥|秘钥|密匙|凭据|凭证|私钥|授权码|访问密钥|接口密钥)["'“”「」]?\s*[:=：＝]\s*["'“”「」]?([A-Za-z0-9!@#$%^&*_~+=-]{6,64}))`},
		"CONNSTR": {`[a-z][a-z0-9+.-]{0,63}://[^\s:@/]+:([^\s@/]{4,})@`},
		"PHONE":   {`1[3-9][0-9]{9}`, `1[3-9][0-9]([ -])[0-9]{4}\1[0-9]{4}`, `(\+?86|0086|\(\+?86\)|（\+?86）)([ -])?1[3-9][0-9]{9}`, `(\+?86|0086|\(\+?86\)|（\+?86）)([ -])?1[3-9][0-9]([ -])[0-9]{4}\3[0-9]{4}`},
		// Match UTF-8 local parts as opaque non-space bytes; explicit byte
		// ranges are rejected by libstdc++'s ECMAScript regex implementation.
		"EMAIL":    {`([^\s@]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`},
		"LANDLINE": {`0(10|2[0-9]|[3-9][0-9]{2})[- ]?[2-9][0-9]{6,7}`},
		"IDCARD":   {`((1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{15}[0-9Xx]|(1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{13})`},
		"CARD":     {`[3-6][0-9]{12,18}`}, "IBAN": {`[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}`},
		"USCC": {`[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}`}, "MAC": {`[0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}`},
		"IP_PRIVATE":  {`(192\.168|169\.254|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7]))\.[0-9]{1,3}\.[0-9]{1,3}`},
		"IP_INTERNAL": {`(10\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}`},
		// Spell out the repeated octet prefix so the native regex safety check
		// does not reject a quantified group containing quantifiers.
		"IP_PUBLIC":    {`([0-9]\.|[0-9][0-9]\.|[0-9][0-9][0-9]\.){3}[0-9]{1,3}`},
		"IPV6_PRIVATE": {`(fe[89AaBb]|fc|fd)[0-9A-Fa-f:]{1,44}`},
		// The final character class intentionally accepts the full plate suffix;
		// semantic validation remains in the Go/C++ label-specific matcher.
		"PLATE": {`(京|津|沪|渝|冀|豫|云|辽|黑|湘|皖|鲁|新|苏|浙|赣|鄂|桂|甘|晋|蒙|陕|吉|闽|贵|粤|青|藏|川|宁|琼|使|领)[A-Z][A-Z0-9]{5,6}`}, "HKID": {`H[0-9]{8}`},
	}
	return patterns[label]
}

// ensureExtensionTokenLocked creates the token used by the browser extension
// when the panel enables the extension bridge. The token lives in the control
// config beside the bridge switch and is returned only through the authenticated
// management config response.
func (s *Server) ensureExtensionTokenLocked() error {
	if s.extensionDisabled {
		s.control["ext_bridge_enabled"] = false
		return nil
	}
	enabled, _ := s.control["ext_bridge_enabled"].(bool)
	if !enabled {
		return nil
	}
	if token, _ := s.control["ext_token"].(string); strings.TrimSpace(token) != "" {
		return nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate extension token: %w", err)
	}
	s.control["ext_token"] = hex.EncodeToString(raw)
	return nil
}

func extensionTokenFromRequest(r *http.Request) string {
	provided := strings.TrimSpace(r.Header.Get("X-Silencer-Token"))
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(provided), "bearer ") {
			provided = strings.TrimSpace(provided[len("Bearer "):])
		}
	}
	return provided
}

func (s *Server) requireExtensionToken(w http.ResponseWriter, r *http.Request) bool {
	if s.extensionDisabled {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "extension_unavailable"})
		return false
	}
	s.configMu.RLock()
	enabledValue, enabledConfigured := s.control["ext_bridge_enabled"]
	enabled, _ := enabledValue.(bool)
	token, _ := s.control["ext_token"].(string)
	s.configMu.RUnlock()
	// Servers constructed without a persisted extension configuration retain
	// the original gateway-authenticated test/API behavior. Once the panel has
	// configured the bridge, all extension calls use the dedicated token.
	if !enabledConfigured && token == "" {
		if s.managementAuthorized(r) {
			return true
		}
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "management_token_required"})
		return false
	}
	if !enabled {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "ext_bridge_disabled"})
		return false
	}
	provided := extensionTokenFromRequest(r)
	if token == "" || len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "invalid_token"})
		return false
	}
	return true
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch child := v.(type) {
		case map[string]any:
			out[k] = cloneMap(child)
		case []any:
			out[k] = cloneConfigList(child)
		default:
			out[k] = child
		}
	}
	return out
}

func cloneConfigList(in []any) []any {
	out := make([]any, len(in))
	for i, value := range in {
		switch child := value.(type) {
		case map[string]any:
			out[i] = cloneMap(child)
		case []any:
			out[i] = cloneConfigList(child)
		default:
			out[i] = value
		}
	}
	return out
}

func mergeControlConfig(dst map[string]any, patch map[string]any) {
	for key, value := range patch {
		if key == "auth_token" || key == "engine" || key == "engine_library" {
			continue
		}
		if current, ok := dst[key].(map[string]any); ok {
			if incoming, ok := value.(map[string]any); ok {
				for childKey, childValue := range incoming {
					current[childKey] = childValue
				}
				continue
			}
		}
		dst[key] = value
	}
}

// An explicit rule table is authoritative. Fill its missing catalog entries
// with false so a sparse table stays disabled rather than reviving defaults.
// Only a missing table uses installation defaults.
func ensureBuiltinRuleInventory(control map[string]any) bool {
	value, present := control["builtin_rules"]
	if !present {
		control["builtin_rules"] = defaultBuiltinRules()
		return true
	}
	existing, _ := value.(map[string]any)
	rules := defaultBuiltinRules()
	changed := len(existing) != len(rules)
	for name := range rules {
		enabled, valid := existing[name].(bool)
		rules[name] = valid && enabled
		if !valid {
			changed = true
		}
	}
	// Keep future/unknown catalog entries visible, without inventing rules.
	for name, value := range existing {
		if _, known := rules[name]; !known {
			rules[name] = value
		}
	}
	control["builtin_rules"] = rules
	return changed
}

func engineConfigChanged(key string) bool {
	switch key {
	case "builtin_rules", "sensitive", "sensitive_disabled", "sensitive_word_disabled", "sensitive_word_whole":
		return true
	default:
		return false
	}
}

// engineConfigFromControl converts the control document into the small,
// stable JSON understood by the C++ engine. The Go control plane remains the
// source of truth, while the native engine receives only executable rules.
func engineConfigFromControl(control map[string]any) (string, error) {
	sensitive, hasSensitive := control["sensitive"].(map[string]any)
	builtin, hasBuiltin := control["builtin_rules"].(map[string]any)
	if !hasSensitive && !hasBuiltin {
		return "", nil
	}

	rules := make([]map[string]any, 0)
	seen := make(map[string]struct{})
	addLiteral := func(needle, label string) {
		needle = strings.TrimSpace(needle)
		label = strings.TrimSpace(label)
		if needle == "" || label == "" {
			return
		}
		key := "literal:" + label + ":" + needle
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		rules = append(rules, map[string]any{"needle": needle, "label": label})
	}
	addPattern := func(pattern, label string) {
		key := "pattern:" + label + ":" + pattern
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		rules = append(rules, map[string]any{"pattern": pattern, "label": label})
	}
	addBuiltinPattern := func(pattern, label string) {
		key := "pattern:" + label + ":" + pattern
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		rule := map[string]any{"pattern": pattern, "label": label}
		switch {
		case label == "SECRET":
			rule["capture_group"] = 3
		case label == "CONNSTR", label == "BEARER", label == "COOKIE_SESSION", label == "BASIC_AUTH", label == "URL_QUERY_SECRET":
			rule["capture_group"] = 1
		case label == "ACCESS_KEY" && strings.Contains(pattern, "aws_secret_access_key"):
			rule["capture_group"] = 2
		}
		switch label {
		case "CARD", "IDCARD", "PHONE", "LANDLINE", "EMAIL", "IBAN", "JWT", "IP_PRIVATE", "IP_INTERNAL", "IP_PUBLIC", "IPV6_PRIVATE", "PLATE", "USCC", "SECRET":
			rule["semantic"] = true
		}
		rules = append(rules, rule)
	}

	// Mask whole credential values before custom words and generic recognizers
	// can replace a fragment inside them. Deduplication keeps these rules out
	// of the later alphabetical pass.
	for _, label := range []string{"BASIC_AUTH", "COOKIE_SESSION", "URL_QUERY_SECRET"} {
		if enabled, _ := builtin[label].(bool); enabled {
			for _, pattern := range builtinRulePatterns(label) {
				addBuiltinPattern(pattern, label)
			}
		}
	}

	if hasSensitive {
		if !hasBuiltin {
			// Older control documents had only the custom sensitive-word map.
			// Keep the two historical safety defaults for those documents, but
			// an explicit builtin_rules map is authoritative, including an empty
			// or all-disabled map.
			addPattern(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`, "EMAIL")
			addPattern(`Bearer\s+[A-Za-z0-9._~+/-]{12,}=*`, "BEARER")
		}
		disabledLabels := stringSet(control["sensitive_disabled"])
		wordDisabled := stringSetMap(control["sensitive_word_disabled"])
		labels := make([]string, 0, len(sensitive))
		for label := range sensitive {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			if disabledLabels[label] {
				continue
			}
			words := []string{}
			switch rawWords := sensitive[label].(type) {
			case []any:
				for _, raw := range rawWords {
					if word, ok := raw.(string); ok {
						words = append(words, word)
					}
				}
			case []string:
				words = append(words, rawWords...)
			}
			for _, word := range words {
				if wordDisabled[label][word] {
					continue
				}
				if strings.HasPrefix(word, "re:") {
					pattern := strings.TrimPrefix(word, "re:")
					// A malformed user pattern must not take down the whole engine;
					// preserve the legacy behavior of skipping just that entry.
					if pattern == "" {
						continue
					}
					if _, err := regexp.Compile(pattern); err != nil {
						continue
					}
					addPattern(pattern, label)
					continue
				}
				addLiteral(word, label)
			}
		}
	}
	if hasBuiltin {
		labels := make([]string, 0, len(builtin))
		for label := range builtin {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			enabled, _ := builtin[label].(bool)
			if !enabled {
				continue
			}
			patterns := builtinRulePatterns(strings.ToUpper(label))
			if len(patterns) == 0 {
				// Preserve forward compatibility for a newly introduced toggle:
				// it remains visible and executable as a literal until its native
				// pattern is added.
				addLiteral(label, label)
				continue
			}
			for _, pattern := range patterns {
				patternLabel := strings.ToUpper(label)
				if patternLabel == "TOKEN" {
					patternLabel = "BEARER"
				}
				addBuiltinPattern(pattern, patternLabel)
			}
		}
	}
	// Once either rule map is present, send an explicit rules array even when
	// it is empty. C++ treats {"rules":[]} as an intentional disable-all
	// configuration; an empty config would restore its built-in defaults.
	raw, err := json.Marshal(map[string]any{"rules": rules})
	return string(raw), err
}

func stringSet(value any) map[string]bool {
	result := map[string]bool{}
	switch items := value.(type) {
	case []any:
		for _, raw := range items {
			if text, ok := raw.(string); ok {
				result[text] = true
			}
		}
	case []string:
		for _, text := range items {
			result[text] = true
		}
	}
	return result
}

func stringSetMap(value any) map[string]map[string]bool {
	result := map[string]map[string]bool{}
	items, _ := value.(map[string]any)
	for label, raw := range items {
		result[label] = stringSet(raw)
	}
	return result
}

func (s *Server) requestItem(item storage.Request) map[string]any {
	view := requestItem(item)
	rules := s.logNoiseRules()
	view["noise_whitelisted"] = matchesLogNoise(rules, item.Status, item.Error, requestMeta(item))
	view["anomaly"] = outcomeAnomaly(item.Status, item.Error, requestMeta(item), rules)
	view["abnormal"] = view["anomaly"] != ""
	return view
}

func requestItem(item storage.Request) map[string]any {
	startedAt := item.StartedAt
	if startedAt.IsZero() {
		startedAt = item.CreatedAt
	}
	endedAt := item.EndedAt
	if endedAt.IsZero() {
		endedAt = startedAt
	}
	status := item.Status
	if status == "" {
		status = "running"
	}
	path := string(item.Payload)
	truncated := 0
	available := len(item.Raw)+len(item.Masked)+len(item.Upstream)+len(item.Restored) > 0
	if item.SummaryBodyAvailable != nil {
		available = *item.SummaryBodyAvailable
	}
	if item.Meta != "" {
		var meta map[string]any
		if json.Unmarshal([]byte(item.Meta), &meta) == nil {
			if meta["body_store"] == true {
				available = true
			}
			if meta["log_expired"] == true || meta["body_storage_error"] == true {
				available = false
			}
			if value, ok := meta["path"].(string); ok {
				path = value
			}
			for _, key := range []string{"body_truncated", "upstream_truncated", "restored_truncated"} {
				if value, ok := meta[key].(bool); ok && value {
					truncated = 1
					break
				}
			}
		}
	}
	view := map[string]any{
		"request_id": item.ID, "started": startedAt.Unix(), "ended": endedAt.Unix(),
		"status": status, "path": path, "error": item.Error, "truncated": truncated,
		"body_unavailable": !available,
	}
	meta := requestMeta(item)
	for _, key := range []string{"ingress", "host", "client", "upstream", "model", "body_scope", "mask_status", "response_status", "upstream_status", "upstream_status_observed", "request_body_incomplete", "failure_stage"} {
		if value, ok := meta[key]; ok {
			view[key] = value
		}
	}
	maskCount, _ := numericMetaInt(meta["mask_token_count"])
	count, _ := numericMetaInt(meta["restore_count"])
	restored, _ := numericMetaInt(meta["restore_restored"])
	unresolved, _ := numericMetaInt(meta["restore_unresolved"])
	view["mask_count"], view["restore_count"], view["restored_count"], view["unresolved_count"] = maskCount, count, restored, unresolved
	restoreStatus := "no_sensitive_data"
	if item.EndedAt.IsZero() {
		restoreStatus = "pending"
	} else if item.Error != "" {
		restoreStatus = "incomplete"
	} else if unresolved > 0 {
		restoreStatus = "unresolved"
	} else if restored > 0 {
		restoreStatus = "restored"
	} else if maskCount > 0 {
		restoreStatus = "no_placeholder_in_response"
	}
	view["restore_status"] = restoreStatus
	return view
}

func isRequestIDPath(path string) bool {
	id := strings.TrimPrefix(path, requestPrefix+"/")
	if len(id) != 32 || strings.Contains(id, "/") {
		return false
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return false
		}
	}
	return true
}

func (s *Server) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, requestPrefix+"/")
	if id == "" || strings.Contains(id, "/") {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request_id"})
		return
	}
	if s.store == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "request_not_found"})
		return
	}
	item, err := s.store.GetRequest(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "request_not_found"})
		return
	}
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	value := s.requestItem(item)
	events, eventErr := s.store.RequestEventSummaries(r.Context(), []string{item.ID})
	if eventErr != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	anomaly := classifyRequestAnomalies([]storage.Request{item}, events, s.logNoiseRules())[item.ID]
	value["anomaly"], value["abnormal"] = anomaly.kind, anomaly.kind != ""
	detailEnabled := s.recordDetailDataEnabled()
	value["detail_recorded"] = detailEnabled
	if !detailEnabled {
		value["body_unavailable"] = true
		s.writeJSON(w, http.StatusOK, map[string]any{"request": value})
		return
	}
	// Full bodies are fetched only for an authenticated detail view; statistics
	// and list responses do not read the persistent body files.
	s.attachFullLedgerBodies(value, item)
	var meta map[string]any
	if item.Meta != "" && json.Unmarshal([]byte(item.Meta), &meta) == nil {
		if items, ok := meta["items"]; ok {
			value["items"] = restoreDetailItems(items, meta)
			labels := make([]string, 0)
			seen := map[string]bool{}
			if list, ok := items.([]any); ok {
				for _, raw := range list {
					if entry, ok := raw.(map[string]any); ok {
						if label, ok := entry["label"].(string); ok && label != "" && !seen[label] {
							seen[label] = true
							labels = append(labels, label)
						}
					}
				}
			}
			if len(labels) > 0 {
				value["matched_rules"] = labels
			}
		}
	}
	if _, present := value["body_unavailable"]; !present {
		value["body_unavailable"] = len(item.Raw) == 0 && len(item.Masked) == 0 && len(item.Upstream) == 0 && len(item.Restored) == 0
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"request": value})
}

// recordDetailDataEnabled controls the opt-in request/response ledger. The
// control document is persisted separately from the SQL history so the user
// can enable it without restarting the gateway.
func (s *Server) recordDetailDataEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	enabled, _ := s.control["record_detail_data"].(bool)
	return enabled
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	switch path {
	case "status":
		s.handleStatus(w, r)
	case "proxy/start":
		s.setProxyRunning(w, r, true)
	case "proxy/stop":
		s.setProxyRunning(w, r, false)
	case "auto_recover/dismiss":
		s.handleCapability(w, r, "auto_recover")
	case "config/builtin_rules":
		s.handleConfigBuiltinRules(w, r)
	case "config/patch":
		s.handleConfigPatch(w, r)
	case "config/disable_origin_check":
		s.handleDisableOriginCheck(w, r)
	case "config/backups":
		s.handleConfigBackups(w, r)
	case "config/restore":
		s.handleConfigRestore(w, r)
	case "stats/today":
		s.handleStatsToday(w, r)
	case "stats/history":
		s.handleStatsHistory(w, r)
	case "stats/models":
		s.handleStatsModels(w, r)
	case "stats/today/restore-items":
		s.handleStatsRestoreItems(w, r)
	case "stats/highlights":
		s.handleStatsHighlights(w, r)
	case "prices/status":
		s.handlePricesStatus(w, r)
	case "prices/sync":
		s.handlePricesSync(w, r)
	case "prices/list":
		s.handlePricesList(w, r)
	case "upstream/test":
		s.handleUpstreamTest(w, r)
	case "demo/mask":
		s.handleDemoMask(w, r)
	case "cert":
		s.handlePostCapability(w, r, "certificate_management")
	case "restore":
		s.handlePostCapability(w, r, "network_restore")
	case "health":
		s.handleHealth(w, r)
	case "logs":
		s.handleLogs(w, r)
	case "logs/detail":
		s.handleLogDetail(w, r)
	case "logs/export":
		if r.Method != http.MethodGet {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		if s.store == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
			return
		}
		if r.URL.Query().Get("limit") == "" {
			query := r.URL.Query()
			query.Set("limit", strconv.Itoa(maxLogQueryEvents))
			r.URL.RawQuery = query.Encode()
		}
		events, hasMore, windowLimited, err := s.filteredLogEvents(r.Context(), r)
		if err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "total": len(events), "has_more": hasMore, "window_limited": windowLimited})
	case "logs/clear":
		if s.store == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
			return
		}
		if r.Method != http.MethodPost {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		if err := s.store.DeleteEvents(r.Context()); err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
			return
		}
		if reusable, ok := s.currentEngine().(ReusableMaskingEngine); ok {
			if err := reusable.ClearReusableMappings(); err != nil {
				s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "mapping_reset_unavailable"})
				return
			}
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "audit/events":
		s.handleAuditEvents(w, r)
	case "audit/job":
		s.handleAuditJob(w, r)
	case "audit/run":
		s.handleAuditRun(w, r)
	case "audit/cancel":
		s.handleAuditCancel(w, r)
	case "audit/clear":
		s.handleAuditClear(w, r)
	case "audit/report/latest":
		s.handleAuditReport(w, r)
	case "ext/ping":
		s.handleExtensionPing(w, r)
	case "ext/mask":
		s.handleExtensionMask(w, r)
	case "ext/restore":
		s.handleExtensionRestore(w, r)
	case "ext/rotate-token":
		s.handleExtensionRotateToken(w, r)
	case "clients/discover":
		s.handleClientDiscover(w, r)
	case "clients/history":
		s.handleClientHistory(w, r)
	case "clients/plan":
		s.handleClientPlan(w, r)
	case "clients/apply", "clients/rollback":
		if r.Method != http.MethodPost {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		var body map[string]any
		if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
			return
		}
		op := strings.TrimPrefix(path, "clients/")
		if resp, err := s.hostAgentCall(r.Context(), op, body); err == nil && resp.Available {
			s.writeHostAgentResponse(w, resp, false)
			return
		}
		s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "available": false, "capability": "host_agent_unavailable", "error": "client_configuration_requires_host_agent"})
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "endpoint_not_found"})
	}
}

func (s *Server) handleExtensionPing(w http.ResponseWriter, r *http.Request) {
	// The panel uses the normal management session to inspect capabilities. A
	// browser extension may also ping with its bridge token.
	if extensionTokenFromRequest(r) != "" {
		if !s.managementAuthorized(r) && !s.requireExtensionToken(w, r) {
			return
		}
	}
	s.configMu.RLock()
	enabled, _ := s.control["ext_bridge_enabled"].(bool)
	s.configMu.RUnlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": enabled, "enabled": enabled, "version": releaseVersion, "ext_protocol": 3, "block_when_down": true, "record_events": true})
}

func (s *Server) handleExtensionRotateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if r.Method == http.MethodGet {
			s.handleCapability(w, r, "extension_token_rotation")
			return
		}
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	// The bridge token cannot rotate itself. This endpoint is a management
	// operation and is called by the panel with its management credentials.
	if !s.managementAuthorized(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "management_token_required"})
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "token_generation_failed"})
		return
	}
	s.configMu.Lock()
	candidate := cloneMap(s.control)
	candidate["ext_token"] = hex.EncodeToString(raw)
	err := s.commitConfigLocked(s.cfg, candidate, false)
	token := s.control["ext_token"]
	s.configMu.Unlock()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "token_persist_failed"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": true, "ext_token": token})
}

func (s *Server) handleCapability(w http.ResponseWriter, r *http.Request, capability string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	// Read-only capability probes retain a 200 response so existing clients can
	// inspect `available`; mutating capabilities use handlePostCapability below.
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "capability": capability, "available": false, "error": "host_agent_unavailable"})
}

func (s *Server) handlePostCapability(w http.ResponseWriter, r *http.Request, capability string) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "capability": capability, "available": false, "error": "host_agent_unavailable"})
}

func (s *Server) handleClientDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if resp, err := s.hostAgentCall(r.Context(), "discover", nil); err == nil && resp.Available {
		s.writeHostAgentResponse(w, resp, true)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": false, "scope": "gateway", "clients": []any{}, "capability": "host_agent_unavailable"})
}

func (s *Server) handleClientHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if resp, err := s.hostAgentCall(r.Context(), "history", nil); err == nil && resp.Available {
		s.writeHostAgentResponse(w, resp, true)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": false, "changes": []any{}, "capability": "host_agent_unavailable"})
}

func (s *Server) handleClientPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var body map[string]any
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	if resp, err := s.hostAgentCall(r.Context(), "plan", body); err == nil && resp.Available {
		s.writeHostAgentResponse(w, resp, true)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": false, "plan": map[string]any{"status": "manual_required", "reason": "host_agent_unavailable"}, "capability": "host_agent_unavailable"})
}

func (s *Server) handleAuditJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	out := map[string]any{}
	for k, v := range s.auditJob {
		out[k] = v
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAuditRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var req struct {
		Confirm      bool   `json:"confirm"`
		UpstreamName string `json:"upstream_name"`
		Model        string `json:"model"`
		Profile      string `json:"profile"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !req.Confirm {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "confirmation_required"})
		return
	}
	s.auditMu.Lock()
	if running, _ := s.auditJob["running"].(bool); running {
		s.auditMu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "audit_already_running"})
		return
	}
	// Active probes are intentionally not part of this gateway build. Returning
	// success here would make the management UI report a completed audit that
	// never ran; passive audit remains available through request/response data.
	s.auditCancel = nil
	job := map[string]any{"running": false, "started_at": time.Now().Unix(), "done": 0, "total": 0, "phase": "capability_unavailable", "capability": "host_probe_unavailable"}
	if req.UpstreamName != "" {
		job["upstream_name"] = req.UpstreamName
	}
	if req.Model != "" {
		job["model"] = req.Model
	}
	if req.Profile != "" {
		job["profile"] = req.Profile
	}
	s.auditJob = job
	s.auditMu.Unlock()
	s.writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "host_probe_unavailable", "capability": "host_probe_unavailable"})
}

func (s *Server) handleAuditCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.auditMu.Lock()
	running, _ := s.auditJob["running"].(bool)
	if !running && s.auditCancel == nil {
		s.auditMu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "audit_not_running"})
		return
	}
	if s.auditCancel != nil {
		s.auditCancel()
		s.auditCancel = nil
	}
	if s.auditJob["running"] == true {
		s.auditJob["running"] = false
		s.auditJob["phase"] = "cancelled"
	}
	s.auditMu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuditClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.auditMu.Lock()
	running, _ := s.auditJob["running"].(bool)
	if running {
		s.auditMu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "audit_running"})
		return
	}
	if s.store == nil {
		s.auditMu.Unlock()
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	s.auditJob = map[string]any{"running": false, "capability": "host_probe_unavailable"}
	s.auditMu.Unlock()
	if err := s.store.DeleteAuditEvents(r.Context()); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuditReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.auditMu.Lock()
	job := cloneMap(s.auditJob)
	s.auditMu.Unlock()
	s.writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "report": "", "available": false, "job": job, "capability": "host_probe_unavailable", "error": "host_probe_unavailable"})
}

func (s *Server) handleLogDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if s.store == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		id = r.URL.Query().Get("seq")
	}
	event, err := s.getEventReference(r.Context(), id)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "event_not_found"})
		return
	}
	if events, listErr := s.store.RelatedLogSummaries(r.Context(), eventRequestKey(event), maxLogQueryEvents); listErr == nil {
		prepareReadMetadata(nil, events)
		event = normalizeRestoreEvent(event, sensitiveRequestIDs(events))
		masks := make(map[string]storage.Event)
		for _, item := range events {
			if eventRequestKey(item) == eventRequestKey(event) && strings.EqualFold(item.Type, "MASK") {
				masks[eventRequestKey(item)] = item
			}
		}
		event = normalizeLogOutcome(event, masks)
	}
	view := logEventView(s.projectLogNoise(event), true)
	// Event rows only carry bounded masked material by design. When the user
	// explicitly enables detail recording, enrich the event detail with the
	// associated request ledger so the four pipeline stages can be compared in
	// one place. List responses remain slim and never include these bodies.
	s.attachRequestDetailContext(r.Context(), view, eventRequestKey(event))
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "event": view})
}

func (s *Server) attachRequestDetail(view map[string]any, requestID string) {
	s.attachRequestDetailContext(context.Background(), view, requestID)
}

func (s *Server) attachRequestDetailContext(ctx context.Context, view map[string]any, requestID string) {
	if view == nil || !s.recordDetailDataEnabled() || strings.TrimSpace(requestID) == "" || s.store == nil {
		return
	}
	item, err := s.store.GetRequest(ctx, requestID)
	if err != nil {
		return
	}
	view["detail_recorded"] = true
	s.attachFullLedgerBodies(view, item)
	if _, present := view["body_unavailable"]; !present {
		view["body_unavailable"] = len(item.Raw) == 0 && len(item.Masked) == 0 && len(item.Upstream) == 0 && len(item.Restored) == 0
	}
	var meta map[string]any
	if item.Meta != "" && json.Unmarshal([]byte(item.Meta), &meta) == nil {
		if view["type"] == "RESTORE" {
			items := restoreDetailItems(meta["items"], meta)
			view["items"] = items
			labels := make([]string, 0)
			seen := map[string]bool{}
			for _, entry := range items {
				if entry["restore_status"] == "not_mentioned" {
					continue
				}
				if label, ok := entry["label"].(string); ok && label != "" && !seen[label] {
					labels = append(labels, label)
					seen[label] = true
				}
			}
			view["matched_rules"] = labels
			return
		}
		if items, ok := meta["items"]; ok {
			view["items"] = items
			labels := make([]string, 0)
			seen := map[string]bool{}
			if list, ok := items.([]any); ok {
				for _, raw := range list {
					if entry, ok := raw.(map[string]any); ok {
						if label, ok := entry["label"].(string); ok && label != "" && !seen[label] {
							seen[label] = true
							labels = append(labels, label)
						}
					}
				}
			}
			if len(labels) > 0 {
				view["matched_rules"] = labels
			}
		}
	}
}

// Return enough of the explicitly enabled ledger for normal conversations;
// mark both retention and presentation truncation instead of silently cutting
// an SSE frame in half and presenting it as a complete upstream response.
func attachLedgerBodies(view map[string]any, item storage.Request) {
	for _, stage := range []struct {
		key  string
		body []byte
	}{
		{"raw", item.Raw}, {"masked", item.Masked},
		{"upstream_response", item.Upstream}, {"restored_response", item.Restored},
	} {
		if len(stage.body) > 0 {
			view[stage.key] = string(stage.body)
		}
	}
	var meta map[string]any
	if json.Unmarshal([]byte(item.Meta), &meta) == nil {
		for _, key := range []string{"body_truncated", "upstream_truncated", "restored_truncated"} {
			if meta[key] == true {
				view["truncated"] = 1
			}
		}
	}
}

func restoreDetailItems(items any, meta map[string]any) []map[string]any {
	list, _ := items.([]any)
	evidence, _ := meta["restore_evidence"].(map[string]any)
	complete := meta["restore_evidence_complete"] == true
	result := make([]map[string]any, 0, len(list))
	known := map[string]bool{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		copy := make(map[string]any, len(item)+4)
		for key, value := range item {
			copy[key] = value
		}
		// Stored request mappings may carry legacy display fields. Only this
		// response's complete token evidence can establish a restoration result.
		delete(copy, "restored")
		delete(copy, "restored_count")
		delete(copy, "unresolved_count")
		token, _ := item["preview"].(string)
		known[token] = true
		copy["restore_status"] = "unknown"
		if complete {
			e, _ := evidence[token].(map[string]any)
			observed, _ := numericMetaInt(e["observed"])
			remaining, _ := numericMetaInt(e["remaining"])
			restored := observed - remaining
			if restored < 0 {
				restored = 0
			}
			copy["restored_count"], copy["unresolved_count"] = restored, remaining
			copy["restored"] = restored > 0
			switch {
			case remaining > 0:
				copy["restore_status"] = "unresolved"
			case restored > 0:
				copy["restore_status"] = "restored"
			default:
				copy["restore_status"] = "not_mentioned"
			}
		}
		result = append(result, copy)
	}
	// Show response tokens without a current-request mapping explicitly. They
	// have no trustworthy original, even if they resemble a neighbouring slot.
	tokens := make([]string, 0)
	for token := range evidence {
		if !known[token] && isCanonicalPlaceholder(token) {
			tokens = append(tokens, token)
		}
	}
	sort.Strings(tokens)
	for _, token := range tokens {
		e, _ := evidence[token].(map[string]any)
		remaining, _ := numericMetaInt(e["remaining"])
		if remaining == 0 {
			continue
		}
		label := "UNKNOWN"
		if match := maskedTokenPattern.FindStringSubmatch(token); len(match) == 2 {
			label = match[1]
		}
		status := "unknown"
		if complete {
			status = "unmapped"
		}
		result = append(result, map[string]any{"label": label, "preview": token, "restore_status": status, "restored": false, "unresolved_count": remaining})
	}
	return result
}

// getEventReference accepts either the durable event id (used by direct API
// clients) or the numeric cursor exposed by the React log feed. The storage
// schema intentionally keeps opaque ids; the cursor is derived from the
// persisted timestamp so it remains portable across SQLite and MySQL.
func (s *Server) getEventReference(ctx context.Context, reference string) (storage.Event, error) {
	if strings.TrimSpace(reference) == "" {
		return storage.Event{}, sql.ErrNoRows
	}
	if event, err := s.store.GetEvent(ctx, reference); err == nil {
		return event, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return storage.Event{}, err
	}
	if sequence, err := strconv.ParseInt(reference, 10, 64); err == nil {
		items, listErr := s.store.ListLogSummaries(ctx, 5000)
		if listErr != nil {
			return storage.Event{}, listErr
		}
		for _, event := range items {
			if eventSequence(event) == sequence {
				return s.store.GetEvent(ctx, event.ID)
			}
		}
		return storage.Event{}, sql.ErrNoRows
	}
	return s.store.GetEvent(ctx, reference)
}

func (s *Server) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if s.store == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	limit := queryInt(r, "limit", 500)
	if limit > 5000 {
		limit = 5000
	}
	floor := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("floor")))
	if floor != "" && auditSeverityRank(floor) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_severity_floor"})
		return
	}
	signal := strings.TrimSpace(r.URL.Query().Get("signal"))
	items, err := s.store.ListAuditEvents(r.Context(), limit)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	out := make([]any, 0, len(items))
	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			since = parsed
		}
	}
	for _, e := range items {
		if eventMeta(e)["log_expired"] == true {
			continue
		}
		if since > 0 && e.CreatedAt.Unix() < since {
			continue
		}
		view := auditEventView(e)
		if signal != "" && view["signal_type"] != signal {
			continue
		}
		if floor != "" {
			severity, _ := view["severity"].(string)
			if auditSeverityRank(severity) < auditSeverityRank(floor) {
				continue
			}
		}
		out = append(out, view)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"events": out, "count": len(out)})
}

func auditSeverityRank(value string) int {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "LOW":
		return 1
	case "MEDIUM":
		return 2
	case "HIGH":
		return 3
	case "CRITICAL":
		return 4
	default:
		return 0
	}
}

func auditEventView(event storage.Event) map[string]any {
	seq := eventSequence(event)
	view := map[string]any{
		"id": seq, "detail_id": event.ID, "seq": seq, "ts": event.CreatedAt.Unix(),
		"request_id": event.RequestID, "type": event.Type,
		"status": event.Status, "error": event.Error, "created_at": event.CreatedAt,
	}
	var meta map[string]any
	if event.Meta != "" && json.Unmarshal([]byte(event.Meta), &meta) == nil {
		for _, key := range []string{"sid", "host", "method", "path", "count", "restored", "http_status", "ingress", "evidence", "severity", "signal_type", "probe_id", "request_hash", "response_hash", "action", "tool_guard_rules", "confirmation_required"} {
			if value, ok := meta[key]; ok {
				view[key] = value
			}
		}
	}
	if _, ok := view["signal_type"]; !ok && strings.HasPrefix(event.Type, "AUDIT_") {
		view["signal_type"] = strings.TrimPrefix(event.Type, "AUDIT_")
	}
	return view
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.configMu.RLock()
	config := cloneMap(s.control)
	running, upstream := s.running, s.cfg.UpstreamURL
	s.configMu.RUnlock()
	config["version"] = releaseVersion
	config["proxy_running"] = running
	config["proxy_starting"] = false
	config["proxy_stopping"] = false
	config["proxy_pid"] = nil
	config["upstream"] = upstream
	config["upstream_ports"] = []any{}
	config["admin"] = true
	config["uptime"] = time.Since(s.started).Seconds()
	config["fail_closed"] = true
	s.writeJSON(w, http.StatusOK, config)
}

func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if r.URL.Path == "/api/auth/logout" {
		http.SetCookie(w, &http.Cookie{Name: "silencer_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"), SameSite: http.SameSiteLaxMode})
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if r.URL.Path == "/api/auth/change-password" {
		var body struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
			return
		}
		s.configMu.Lock()
		current := s.cfg.AuthToken
		valid := len(body.CurrentPassword) == len(current) && subtle.ConstantTimeCompare([]byte(body.CurrentPassword), []byte(current)) == 1
		if !valid {
			s.configMu.Unlock()
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid_credentials"})
			return
		}
		if len([]rune(body.NewPassword)) < 12 || strings.TrimSpace(body.NewPassword) == "" {
			s.configMu.Unlock()
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "password_too_short"})
			return
		}
		path := authPersistencePath()
		if _, _, _, err := configParticipantPaths(runtimeConfigPath(), s.configPath); err != nil {
			s.configMu.Unlock()
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "persist_auth_failed"})
			return
		}
		if err := persistSingleConfig(path, map[string]string{"auth_token": body.NewPassword}); err != nil {
			s.configMu.Unlock()
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "persist_auth_failed"})
			return
		}
		s.cfg.AuthToken = body.NewPassword
		s.configMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "silencer_session", Value: body.NewPassword, Path: "/", HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"), SameSite: http.SameSiteLaxMode})
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	s.configMu.RLock()
	authToken, authUsername := s.cfg.AuthToken, s.cfg.AuthUsername
	s.configMu.RUnlock()
	username := strings.TrimSpace(authUsername)
	if username == "" {
		username = "admin"
	}
	if authToken != "" && (body.Username != username || len(body.Password) != len(authToken) || subtle.ConstantTimeCompare([]byte(body.Password), []byte(authToken)) != 1) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid_credentials"})
		return
	}
	if authToken != "" {
		http.SetCookie(w, &http.Cookie{Name: "silencer_session", Value: authToken, Path: "/", HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"), SameSite: http.SameSiteLaxMode})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": username})
}

func (s *Server) setProxyRunning(w http.ResponseWriter, r *http.Request, running bool) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.configMu.Lock()
	s.running = running
	s.configMu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_running": running})
}

// Startup holds the common disk anchor across control read, migration and write.
func (s *Server) persistControlLocked() error {
	ensureBuiltinRuleInventory(s.control)
	prepared, err := prepareConfigFile(s.configPath, s.control)
	if err != nil {
		return err
	}
	return persistConfigCandidate(prepared)
}

func (s *Server) rebuildEngineLocked() error {
	cfg := s.cfg
	raw, err := engineConfigFromControl(s.control)
	if err != nil {
		return fmt.Errorf("encode engine rules: %w", err)
	}
	if raw != "" {
		cfg.EngineConfig = string(raw)
	}
	e, err := NewEngine(cfg)
	if err != nil {
		return err
	}
	if !s.replaceEngine(e) {
		return errors.New("gateway is shutting down")
	}
	return nil
}
func (s *Server) handleConfigBuiltinRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var changes map[string]any
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &changes) {
		return
	}
	known := defaultBuiltinRules()
	if len(changes) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "rule_changes_required"})
		return
	}
	for name, value := range changes {
		if _, ok := known[name]; !ok {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown_builtin_rule"})
			return
		}
		if _, ok := value.(bool); !ok {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "builtin_rule_must_be_boolean"})
			return
		}
	}
	s.configMu.Lock()
	candidate := cloneMap(s.control)
	ensureBuiltinRuleInventory(candidate)
	rules := candidate["builtin_rules"].(map[string]any)
	for name, value := range changes {
		rules[name] = value
	}
	err := s.commitConfigLocked(s.cfg, candidate, true)
	config := cloneMap(s.control)
	s.configMu.Unlock()
	if err == nil {
		s.wakeLogRetention()
	}
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": config, "warnings": []string{}, "proxy_restarted": false})
}

func (s *Server) handleConfigPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var patch struct {
		Key   string   `json:"key"`
		Op    string   `json:"op"`
		Path  []string `json:"path"`
		Value any      `json:"value"`
		Match string   `json:"match"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &patch) || patch.Key == "" {
		return
	}
	s.configMu.Lock()
	candidate := cloneMap(s.control)
	path := append([]string{patch.Key}, patch.Path...)
	if patch.Op == "create_if_absent" {
		if _, exists := getNested(candidate, path); exists {
			s.configMu.Unlock()
			s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "config_key_already_exists"})
			return
		}
		patch.Op = "set"
	}
	applyConfigPatch(candidate, path, patch.Op, patch.Value, patch.Match)
	if err := validateToolGuard(candidate); err != nil {
		s.configMu.Unlock()
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	err := s.commitConfigLocked(s.cfg, candidate, engineConfigChanged(patch.Key))
	config := cloneMap(s.control)
	s.configMu.Unlock()
	if err == nil {
		s.wakeLogRetention()
	}
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": config, "warnings": []string{}, "proxy_restarted": false})
}

func (s *Server) handleConfigRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var body struct {
		Config map[string]any `json:"config"`
		File   string         `json:"file"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	if body.File != "" {
		if !validBackupName(body.File) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_backup_file"})
			return
		}
		if s.configPath == "" || !strings.HasPrefix(body.File, filepath.Base(s.configPath)+".backup-") {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_backup_file"})
			return
		}
		path := filepath.Join(s.backupDir(), body.File)
		if filepath.Dir(path) != s.backupDir() {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_backup_file"})
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "backup_not_found"})
			return
		}
		if err = json.Unmarshal(data, &body.Config); err != nil || body.Config == nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_backup"})
			return
		}
	}
	if body.Config == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "config_required"})
		return
	}
	s.configMu.Lock()
	err := s.commitConfigLocked(s.cfg, cloneMap(body.Config), true, true)
	s.configMu.Unlock()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warnings": []string{}, "proxy_restarted": false})
}

func (s *Server) handleUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var body struct {
		Name       string `json:"name"`
		Mode       string `json:"mode"`
		APIKey     string `json:"api_key"`
		Model      string `json:"model"`
		PathPrefix string `json:"path_prefix"`
		Content    string `json:"content"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	if body.Mode == "" {
		body.Mode = "port"
	}
	if body.Mode != "port" && body.Mode != "models" && body.Mode != "chat" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_test_mode"})
		return
	}
	if body.Mode == "chat" && strings.TrimSpace(body.Model) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "model_required"})
		return
	}
	if body.Mode == "port" {
		ready := s.ready.get()
		result := map[string]any{"ok": ready, "mode": body.Mode, "probe_target": "gateway"}
		if !ready {
			result["error"] = "gateway_not_ready"
		}
		s.writeJSON(w, http.StatusOK, result)
		return
	}

	var configured configuredUpstreamTarget
	var configuredOK bool
	s.configMu.RLock()
	baseURL := s.cfg.UpstreamURL
	if body.Name != "" {
		configured, configuredOK = configuredUpstream(s.control, body.Name)
		if !configuredOK {
			s.configMu.RUnlock()
			s.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "upstream_not_found"})
			return
		}
		baseURL = configured.Target
		if body.PathPrefix == "" {
			body.PathPrefix = configured.BasePath
		}
		if body.APIKey == "" {
			body.APIKey = configured.APIKey
		}
	}
	s.configMu.RUnlock()
	if strings.TrimSpace(baseURL) == "" {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "upstream_not_configured"})
		return
	}
	target, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || target.Scheme == "" || target.Host == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_upstream_url"})
		return
	}
	target.Path = joinURLPath(target.Path, strings.TrimSpace(body.PathPrefix))
	switch body.Mode {
	case "models":
		target.Path = joinURLPath(target.Path, "models")
	case "chat":
		target.Path = joinURLPath(target.Path, "chat/completions")
	}
	target.RawQuery = ""
	var requestBody io.Reader
	if body.Mode == "chat" {
		payload, _ := json.Marshal(map[string]any{"model": body.Model, "messages": []map[string]string{{"role": "user", "content": body.Content}}})
		requestBody = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	method := http.MethodGet
	if body.Mode == "chat" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), requestBody)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_upstream_url"})
		return
	}
	if body.Mode == "chat" {
		req.Header.Set("Content-Type", "application/json")
	}
	applyUpstreamHeaders(req, configured.ExtraHeaders)
	if body.APIKey != "" {
		header := "Authorization"
		if configuredOK && configured.APIKeyHeader != "" {
			header = configured.APIKeyHeader
		}
		if strings.EqualFold(header, "Authorization") {
			req.Header.Set(header, "Bearer "+body.APIKey)
		} else {
			req.Header.Set(header, body.APIKey)
		}
	}
	stripGatewayCredentials(req)
	client := &http.Client{Timeout: 10 * time.Second, Transport: s.proxyTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "upstream_unavailable", "mode": body.Mode, "probe_target": "upstream"})
		return
	}
	defer resp.Body.Close()
	_, readErr := io.CopyN(io.Discard, resp.Body, 64<<10)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	result := map[string]any{"ok": ok, "status": resp.StatusCode, "mode": body.Mode, "probe_target": "upstream"}
	if !ok {
		result["error"] = "upstream_http_error"
	} else if readErr != nil && !errors.Is(readErr, io.EOF) {
		result["ok"] = false
		result["error"] = "upstream_read_failed"
	}
	s.writeJSON(w, http.StatusOK, result)
}

type configuredUpstreamTarget struct {
	Target       string
	BasePath     string
	APIKey       string
	APIKeyHeader string
	ExtraHeaders map[string]string
}

func configuredUpstream(control map[string]any, name string) (configuredUpstreamTarget, bool) {
	items, _ := control["upstreams"].([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemName, _ := item["name"].(string)
		if itemName != name {
			continue
		}
		target, _ := item["target"].(string)
		basePath, _ := item["base_path"].(string)
		apiKey, _ := item["api_key"].(string)
		apiKeyHeader, _ := item["api_key_header"].(string)
		extraHeaders := map[string]string{}
		if rawHeaders, ok := item["extra_headers"].(map[string]any); ok {
			for key, value := range rawHeaders {
				if text, ok := value.(string); ok && strings.TrimSpace(key) != "" {
					extraHeaders[key] = text
				}
			}
		}
		return configuredUpstreamTarget{Target: target, BasePath: basePath, APIKey: apiKey, APIKeyHeader: apiKeyHeader, ExtraHeaders: extraHeaders}, true
	}
	return configuredUpstreamTarget{}, false
}

func hasConfiguredUpstream(control map[string]any) bool {
	items, _ := control["upstreams"].([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := item["name"].(string)
		target, _ := item["target"].(string)
		if strings.TrimSpace(name) != "" && strings.TrimSpace(target) != "" {
			return true
		}
	}
	return false
}

func (s *Server) handleDisableOriginCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	s.configMu.Lock()
	candidate := cloneMap(s.control)
	candidate["origin_check"] = false
	err := s.commitConfigLocked(s.cfg, candidate, false)
	config := cloneMap(s.control)
	s.configMu.Unlock()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "origin check disabled", "config": config})
}

type statsDayAggregate struct {
	requests, masks, restored, alerts int64
	prompt, completion                int64
	cacheRead, cacheWrite             int64
	cacheObserved, cacheRateSamples   int64
	cacheInput                        int64
}

type cacheUsageAggregate struct {
	read, rateRead, write, observed, rateSamples, input int64
}

func (a *cacheUsageAggregate) add(usage usageSummary) {
	if a == nil || !usage.CacheFound {
		return
	}
	a.read += usage.CacheRead
	a.write += usage.CacheWrite
	a.observed++
	if usage.CacheRateFound {
		a.rateRead += usage.CacheRead
		a.rateSamples++
		a.input += usage.CacheTotal
	}
}

func (a cacheUsageAggregate) hitRate() any {
	var hitRate any
	if a.input > 0 {
		rate := float64(a.rateRead) / float64(a.input)
		if rate >= 0 && rate <= 1 {
			hitRate = rate
		}
	}
	return hitRate
}

func (a cacheUsageAggregate) response() map[string]any {
	return map[string]any{
		"read_tokens": a.read, "write_tokens": a.write,
		"observed_requests": a.observed, "rate_observed_requests": a.rateSamples,
		"input_tokens": a.input, "hit_rate": a.hitRate(), "source": "upstream_usage",
	}
}

// Match the native token formats, including historical six-character suffixes.
// Ordinary templates such as {{LABEL_TEMPLATE}} are not gateway identities.
var maskedTokenPattern = regexp.MustCompile(`(?i)\{\{([A-Z0-9_]{1,12})_(?:[a-f0-9]{16}|[a-f0-9]{6}|[bcdfghjkmnpqrstvwxz]{6})\}\}`)

type maskAggregate struct {
	byLabel map[string]int
	byWord  map[string]map[string]int
	total   int
}

func addMaskedTextStats(aggregate *maskAggregate, masked []byte) {
	if aggregate == nil || len(masked) == 0 {
		return
	}
	if aggregate.byLabel == nil {
		aggregate.byLabel = map[string]int{}
	}
	if aggregate.byWord == nil {
		aggregate.byWord = map[string]map[string]int{}
	}
	for _, match := range maskedTokenPattern.FindAllSubmatch(masked, -1) {
		if len(match) != 2 {
			continue
		}
		label := string(match[1])
		token := string(match[0])
		aggregate.byLabel[label]++
		aggregate.total++
		if aggregate.byWord[label] == nil {
			aggregate.byWord[label] = map[string]int{}
		}
		aggregate.byWord[label][token]++
	}
}

func addMaskedItemStats(aggregate *maskAggregate, meta map[string]any) {
	if aggregate == nil {
		return
	}
	if summary, ok := meta["mask_summary"].(map[string]any); ok {
		if aggregate.byLabel == nil {
			aggregate.byLabel = map[string]int{}
		}
		for label, value := range summary {
			count, _ := numericMetaInt(value)
			if count > 0 {
				aggregate.byLabel[label] += count
				aggregate.total += count
			}
		}
		return
	}
	items, _ := meta["items"].([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		label, _ := item["label"].(string)
		token, _ := item["original"].(string)
		if token == "" {
			token, _ = item["preview"].(string)
		}
		if label == "" || token == "" {
			continue
		}
		if aggregate.byLabel == nil {
			aggregate.byLabel = map[string]int{}
		}
		if aggregate.byWord == nil {
			aggregate.byWord = map[string]map[string]int{}
		}
		count := maskedItemOccurrences(item)
		aggregate.byLabel[label] += count
		aggregate.total += count
		if aggregate.byWord[label] == nil {
			aggregate.byWord[label] = map[string]int{}
		}
		aggregate.byWord[label][token] += count
	}
}

func addMaskedRequestStats(aggregate *maskAggregate, request storage.Request) {
	if aggregate == nil {
		return
	}
	meta := requestMeta(request)
	if _, ok := meta["mask_summary"]; ok {
		addMaskedItemStats(aggregate, meta)
		return
	}
	if complete, _ := meta["items_complete"].(bool); complete {
		addMaskedItemStats(aggregate, meta)
		return
	}

	// The request ledger is the authoritative source for the number of
	// matches.  Metadata items are only a bounded enrichment used to recover
	// the original word.  Older code switched entirely to meta.items whenever
	// the key existed, which silently dropped matches for large requests (the
	// comparison buffer is intentionally capped).
	matches := maskedTokenPattern.FindAllSubmatch(request.Masked, -1)
	if len(matches) == 0 {
		// Some old rows have no masked body but do retain per-token metadata.
		addMaskedItemStats(aggregate, meta)
		return
	}

	// Index the saved items by their placeholder.  Keep a queue per token so
	// repeated occurrences are assigned in request order without guessing.
	byToken := map[string][]string{}
	if rawItems, ok := meta["items"].([]any); ok {
		for _, raw := range rawItems {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			preview, _ := item["preview"].(string)
			if preview == "" {
				preview, _ = item["token"].(string)
			}
			if preview == "" || !maskedTokenPattern.MatchString(preview) {
				continue
			}
			word, _ := item["original"].(string)
			if word == "" {
				word = preview
			}
			byToken[preview] = append(byToken[preview], word)
		}
	}

	if aggregate.byLabel == nil {
		aggregate.byLabel = map[string]int{}
	}
	if aggregate.byWord == nil {
		aggregate.byWord = map[string]map[string]int{}
	}
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		label := string(match[1])
		token := string(match[0])
		word := token
		if queue := byToken[token]; len(queue) > 0 {
			word = queue[0]
			byToken[token] = queue[1:]
		}
		aggregate.byLabel[label]++
		aggregate.total++
		if aggregate.byWord[label] == nil {
			aggregate.byWord[label] = map[string]int{}
		}
		aggregate.byWord[label][word]++
	}
}

func maskAggregateResponse(aggregate maskAggregate, limit int) (map[string]int, []map[string]any) {
	byLabel := aggregate.byLabel
	if byLabel == nil {
		byLabel = map[string]int{}
	}
	type row struct {
		label string
		word  string
		count int
	}
	rows := make([]row, 0)
	for label, words := range aggregate.byWord {
		for word, count := range words {
			rows = append(rows, row{label: label, word: word, count: count})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		if rows[i].label != rows[j].label {
			return rows[i].label < rows[j].label
		}
		return rows[i].word < rows[j].word
	})
	if limit < 1 {
		limit = 10
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	top := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		display := "plaintext"
		if maskedTokenPattern.MatchString(item.word) {
			display = "placeholder"
		}
		top = append(top, map[string]any{"label": item.label, "word": item.word, "original": item.word, "display": display, "count": item.count})
	}
	return byLabel, top
}

func maskAggregateGroupResponse(aggregate *maskAggregate, limit int) map[string]any {
	if aggregate == nil {
		aggregate = &maskAggregate{}
	}
	byLabel, topWords := maskAggregateResponse(*aggregate, limit)
	byLabelWords := make(map[string][]map[string]any, len(aggregate.byWord))
	for label, words := range aggregate.byWord {
		rows := make([]map[string]any, 0, len(words))
		for word, count := range words {
			display := "plaintext"
			if maskedTokenPattern.MatchString(word) {
				display = "placeholder"
			}
			rows = append(rows, map[string]any{"word": word, "original": word, "display": display, "count": count})
		}
		sort.Slice(rows, func(i, j int) bool {
			left, _ := rows[i]["count"].(int)
			right, _ := rows[j]["count"].(int)
			if left != right {
				return left > right
			}
			lw, _ := rows[i]["word"].(string)
			rw, _ := rows[j]["word"].(string)
			return lw < rw
		})
		byLabelWords[label] = rows
	}
	return map[string]any{"by_label": byLabel, "by_label_words": byLabelWords, "top_words": topWords, "label_total": aggregate.total}
}

func ingressFromMeta(meta map[string]any) string {
	if value, ok := meta["ingress"].(string); ok && strings.EqualFold(strings.TrimSpace(value), "capture") {
		return "capture"
	}
	if value, ok := meta["ingress"].(string); ok && strings.EqualFold(strings.TrimSpace(value), "ext") {
		return "ext"
	}
	return "proxy"
}

// requestMeta is the request-ledger equivalent of eventMeta.  Request
// metadata is stored as JSON text so the stats path must parse it before
// assigning an ingress bucket.  Older rows have no ingress field and are
// intentionally treated as proxy traffic.
func requestMeta(request storage.Request) map[string]any {
	if request.StatsMetaCanonical {
		return request.StatsMeta
	}
	if request.StatsMeta != nil {
		return canonicalRestoreEvidence(request.StatsMeta)
	}
	var meta map[string]any
	if request.Meta != "" && json.Unmarshal([]byte(request.Meta), &meta) == nil {
		return canonicalRestoreEvidence(meta)
	}
	return map[string]any{}
}

func statsDate(t, location time.Time) string {
	return t.In(location.Location()).Format("2006-01-02")
}

func statsRequestUsage(item storage.Request) (usageSummary, string) {
	usage := usageFromMetaMap(requestMeta(item))
	if usage.Found || usage.CacheFound {
		return usage, usage.Model
	}
	return usageSummary{Model: usage.Model}, usage.Model
}

func statsRangeDays(r *http.Request) (int, error) {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	if value == "" || value == "today" {
		return 1, nil
	}
	value = strings.TrimSuffix(value, "d")
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 || days > 365 {
		return 0, errors.New("range must be today or 1-365 days")
	}
	return days, nil
}
func (s *Server) failedStatsRequest(item storage.Request) bool {
	return outcomeAnomaly(item.Status, item.Error, requestMeta(item), s.logNoiseRules()) != ""
}

func (s *Server) handleStatsToday(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.writeJSON(w, 200, emptyTodayStats())
		return
	}
	days, rangeErr := statsRangeDays(r)
	if rangeErr != nil {
		s.writeJSON(w, 400, map[string]any{"error": rangeErr.Error()})
		return
	}
	reqs, evs, e := s.store.DashboardStats(r.Context(), time.Now().AddDate(0, 0, -days))
	if e != nil {
		s.writeJSON(w, 503, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	prepareReadMetadata(reqs, evs)
	masks := 0
	anomalies := classifyRequestAnomalies(reqs, evs, s.logNoiseRules())
	byType := map[string]int{}
	maskedRequestIDs := make(map[string]struct{}, len(reqs))
	var promptTokens, completionTokens int64
	cacheUsage := cacheUsageAggregate{}
	maskStats := maskAggregate{}
	ingressStats := map[string]*maskAggregate{"proxy": {}, "ext": {}, "capture": {}}
	prefixMasks, prefixRewritten, prefixReused, prefixDiffSum, prefixDiffN := 0, 0, 0, 0, 0
	for _, v := range reqs {
		if requestHasMaskedItems(v) || maskedTokenCount(v.Masked) > 0 {
			masks++
			maskedRequestIDs[v.ID] = struct{}{}
		}
		var rowStats maskAggregate
		addMaskedRequestStats(&rowStats, v)
		mergeMaskStats(&maskStats, rowStats)
		mergeMaskStats(ingressStats[ingressFromMeta(requestMeta(v))], rowStats)
		usage, _ := statsRequestUsage(v)
		cacheUsage.add(usage)
		if usage.Found {
			promptTokens += usage.Prompt
			completionTokens += usage.Completion
		}
	}
	for _, v := range evs {
		t := strings.ToUpper(v.Type)
		byType[t]++
		if t == "MASK" || strings.Contains(t, "MASK") {
			if maskEventItemCount(v) > 0 {
				key := eventRequestKey(v)
				if _, duplicate := maskedRequestIDs[key]; !duplicate {
					masks++
					var rowStats maskAggregate
					if len(v.Masked) > 0 {
						addMaskedTextStats(&rowStats, v.Masked)
					} else {
						addMaskedItemStats(&rowStats, eventMeta(v))
					}
					mergeMaskStats(&maskStats, rowStats)
					mergeMaskStats(ingressStats[ingressFromMeta(eventMeta(v))], rowStats)
				}
				maskedRequestIDs[key] = struct{}{}
			}
			meta := eventMeta(v)
			if _, ok := meta["body_rewritten"]; ok {
				prefixMasks++
				if rewritten, ok := meta["body_rewritten"].(bool); ok && rewritten {
					prefixRewritten++
				}
				if reused, ok := meta["suffix_reused"].(bool); ok && reused {
					prefixReused++
				}
				if diff, ok := meta["first_diff_byte"].(float64); ok && diff >= 0 {
					prefixDiffSum += int(diff)
					prefixDiffN++
				}
			}
		}
	}
	restored, restoreByStatus := aggregateRestoreStats(reqs, evs, maskedRequestIDs)
	by, topWords := maskAggregateResponse(maskStats, 10)
	wordsByIngress := map[string]any{
		"capture": maskAggregateGroupResponse(ingressStats["capture"], 100),
		"proxy":   maskAggregateGroupResponse(ingressStats["proxy"], 100),
		"ext":     maskAggregateGroupResponse(ingressStats["ext"], 100),
	}
	prefix := prefixStats(prefixMasks, prefixRewritten, prefixReused, prefixDiffSum, prefixDiffN)
	restoreFailed := restoreByStatus["unresolved"] + restoreByStatus["restore_skipped"]
	s.writeJSON(w, 200, map[string]any{"requests": len(reqs), "alerts": len(anomalies), "alert_counts": anomalyCounts(anomalies), "mask_events": masks, "masked_items": maskStats.total, "restored": restored, "restore_ok": restored, "restore_failed": restoreFailed, "restore_by_status": restoreByStatus, "by_type": byType, "prefix": prefix, "tokens": map[string]int64{"prompt": promptTokens, "completion": completionTokens}, "cache": cacheUsage.response(), "by_label": by, "top_words": topWords, "words_by_ingress": wordsByIngress})
}

func eventRequestKey(event storage.Event) string {
	if event.RequestID != "" {
		return event.RequestID
	}
	if sid, ok := eventMeta(event)["sid"].(string); ok && sid != "" {
		return sid
	}
	return event.ID
}

func maskEventItemCount(event storage.Event) int {
	if count := maskedTokenCount(event.Masked); count > 0 {
		return count
	}
	meta := eventMeta(event)
	switch count := meta["count"].(type) {
	case float64:
		if count > 0 {
			return int(count)
		}
	case int:
		if count > 0 {
			return count
		}
	case int64:
		if count > 0 {
			return int(count)
		}
	}
	return 0
}

func inferredRestoreStatus(request storage.Request, hasMaskedItems bool) string {
	var restored, unresolved int
	meta := requestMeta(request)
	if _, ok := meta["restore_count"]; ok {
		restored, _ = numericMetaInt(meta["restore_restored"])
		unresolved, _ = numericMetaInt(meta["restore_unresolved"])
	} else {
		_, restored, unresolved = restorePlaceholderCounts(request.Upstream, request.Restored)
	}
	if unresolved > 0 {
		return "unresolved"
	}
	if restored > 0 {
		return "restored"
	}
	if hasMaskedItems {
		return "no_placeholder_in_response"
	}
	return "no_sensitive_data"
}

func requestHasMaskedItems(request storage.Request) bool {
	meta := requestMeta(request)
	if summary, ok := meta["mask_summary"].(map[string]any); ok {
		for _, value := range summary {
			if count, _ := numericMetaInt(value); count > 0 {
				return true
			}
		}
		return false
	}
	if meta["items_complete"] == true {
		return len(anySlice(meta["items"])) > 0
	}
	return maskedTokenCount(request.Masked) > 0 || len(anySlice(meta["items"])) > 0
}

func normalizeRestoreStatus(status string, hasMaskedItems bool) string {
	status = strings.TrimSpace(status)
	if status == "" || status == "no_sensitive_data" {
		if hasMaskedItems {
			return "no_placeholder_in_response"
		}
		return "no_sensitive_data"
	}
	return status
}

func aggregateRestoreStats(reqs []storage.Request, events []storage.Event, maskedRequestIDs map[string]struct{}) (int, map[string]int) {
	restored := 0
	byStatus := map[string]int{}
	seen := make(map[string]struct{}, len(reqs))
	latest := make(map[string]storage.Event, len(events))
	for _, event := range events {
		if !strings.Contains(strings.ToUpper(event.Type), "RESTORE") {
			continue
		}
		key := eventRequestKey(event)
		if previous, exists := latest[key]; exists {
			if event.CreatedAt.Before(previous.CreatedAt) ||
				(event.CreatedAt.Equal(previous.CreatedAt) && event.ID <= previous.ID) {
				continue
			}
		}
		latest[key] = event
	}
	for key, event := range latest {
		seen[key] = struct{}{}
		_, hasMaskedItems := maskedRequestIDs[key]
		status := normalizeRestoreStatus(canonicalRestoreStatus(event.Status, eventMeta(event)), hasMaskedItems)
		byStatus[status]++
		if status == "restored" {
			restored++
		}
	}
	for _, request := range reqs {
		if _, hasEvent := seen[request.ID]; hasEvent {
			continue
		}
		_, hasMaskedItems := maskedRequestIDs[request.ID]
		status := inferredRestoreStatus(request, hasMaskedItems)
		byStatus[status]++
		if status == "restored" {
			restored++
		}
	}
	return restored, byStatus
}

func emptyTodayStats() map[string]any {
	return map[string]any{"requests": 0, "alerts": 0, "alert_counts": anomalyCounts(nil), "mask_events": 0, "masked_items": 0, "restored": 0, "restore_ok": 0, "restore_failed": 0, "restore_by_status": map[string]int{}, "by_type": map[string]int{}, "prefix": nil, "tokens": map[string]int{"prompt": 0, "completion": 0}, "cache": cacheUsageAggregate{}.response(), "by_label": map[string]int{}, "top_words": []any{}, "words_by_ingress": map[string]any{"capture": maskAggregateGroupResponse(nil, 100), "proxy": maskAggregateGroupResponse(nil, 100), "ext": maskAggregateGroupResponse(nil, 100)}}
}

func prefixStats(masks, rewritten, reused, diffSum, diffN int) map[string]any {
	if masks <= 0 {
		return nil
	}
	if rewritten < 0 {
		rewritten = 0
	}
	if rewritten > masks {
		rewritten = masks
	}
	return map[string]any{"masks": masks, "rewritten": rewritten, "clean": masks - rewritten, "clean_rate": float64(masks-rewritten) / float64(masks), "suffix_reused": reused, "reuse_rate": func() any {
		if rewritten == 0 {
			return nil
		}
		return float64(reused) / float64(rewritten)
	}(), "avg_first_diff": func() any {
		if diffN == 0 {
			return nil
		}
		return float64(diffSum) / float64(diffN)
	}(), "diff_samples": diffN}
}

func (s *Server) handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	if days < 1 {
		days = 1
	}
	granularity := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("granularity")))
	if granularity != "hour" {
		granularity = "day"
	}
	now := time.Now()
	bucketCount := days
	step := 24 * time.Hour
	if granularity == "hour" {
		step = time.Hour
	}
	start := now.Add(-time.Duration(bucketCount-1) * step)
	if s.store == nil {
		data := make([]map[string]any, 0, bucketCount)
		for i := 0; i < bucketCount; i++ {
			d := start.Add(time.Duration(i) * step)
			label := d.Format("2006-01-02")
			if granularity == "hour" {
				label = d.Format("01-02 15:00")
			}
			data = append(data, map[string]any{"ts": d.Unix(), "label": label, "requests": 0, "mask_events": 0, "restored": 0, "alerts": 0, "tokens_prompt": 0, "tokens_completion": 0, "cache_read_tokens": 0, "cache_write_tokens": 0, "cache_observed_requests": 0, "cache_rate_observed_requests": 0, "cache_input_tokens": 0})
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "granularity": granularity, "days": days, "data": data})
		return
	}
	reqs, evs, e := s.store.HistoryStats(r.Context(), start.Add(-step))
	if e != nil {
		s.writeJSON(w, 503, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	prepareReadMetadata(reqs, evs)
	bucketKey := func(t time.Time) string {
		if granularity == "hour" {
			return t.In(now.Location()).Format("2006-01-02 15")
		}
		return statsDate(t, now)
	}
	byBucket := make(map[string]*statsDayAggregate, bucketCount)
	for i := 0; i < bucketCount; i++ {
		d := start.Add(time.Duration(i) * step)
		byBucket[bucketKey(d)] = &statsDayAggregate{}
	}
	maskedRequestIDs := make(map[string]struct{}, len(reqs))
	restoredRequestIDs := make(map[string]struct{}, len(reqs))
	for _, item := range reqs {
		key := bucketKey(item.CreatedAt)
		entry := byBucket[key]
		if entry == nil {
			continue
		}
		entry.requests++
		if requestHasMaskedItems(item) {
			entry.masks++
			maskedRequestIDs[item.ID] = struct{}{}
		}
		if inferredRestoreStatus(item, false) == "restored" {
			entry.restored++
			restoredRequestIDs[item.ID] = struct{}{}
		}
		usage, _ := statsRequestUsage(item)
		if usage.CacheFound {
			entry.cacheRead += usage.CacheRead
			entry.cacheWrite += usage.CacheWrite
			entry.cacheObserved++
			if usage.CacheRateFound {
				entry.cacheRateSamples++
				entry.cacheInput += usage.CacheTotal
			}
		}
		if usage.Found {
			entry.prompt += usage.Prompt
			entry.completion += usage.Completion
		}
	}
	for _, event := range evs {
		key := bucketKey(event.CreatedAt)
		entry := byBucket[key]
		if entry == nil {
			continue
		}
		typeName := strings.ToLower(event.Type)
		if strings.Contains(typeName, "mask") && maskEventItemCount(event) > 0 {
			if _, duplicate := maskedRequestIDs[event.RequestID]; !duplicate {
				entry.masks++
				maskedRequestIDs[event.RequestID] = struct{}{}
			}
		}
		if strings.Contains(typeName, "restore") && event.Status == "restored" {
			if _, duplicate := restoredRequestIDs[event.RequestID]; !duplicate {
				entry.restored++
				restoredRequestIDs[event.RequestID] = struct{}{}
			}
		}
	}
	for _, anomaly := range classifyRequestAnomalies(reqs, evs, s.logNoiseRules()) {
		if entry := byBucket[bucketKey(anomaly.at)]; entry != nil {
			entry.alerts++
		}
	}
	data := make([]map[string]any, 0, bucketCount)
	for i := 0; i < bucketCount; i++ {
		d := start.Add(time.Duration(i) * step)
		entry := byBucket[bucketKey(d)]
		label := d.Format("2006-01-02")
		if granularity == "hour" {
			label = d.Format("01-02 15:00")
		}
		data = append(data, map[string]any{"ts": d.Unix(), "label": label, "requests": entry.requests, "mask_events": entry.masks, "restored": entry.restored, "alerts": entry.alerts, "tokens_prompt": entry.prompt, "tokens_completion": entry.completion, "cache_read_tokens": entry.cacheRead, "cache_write_tokens": entry.cacheWrite, "cache_observed_requests": entry.cacheObserved, "cache_rate_observed_requests": entry.cacheRateSamples, "cache_input_tokens": entry.cacheInput})
	}
	s.writeJSON(w, 200, map[string]any{"ok": true, "granularity": granularity, "days": days, "data": data})
}
func (s *Server) handleStatsModels(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 7)
	if s.store == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "days": days, "models": []any{}})
		return
	}
	reqs, err := s.store.RequestStats(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	prepareReadMetadata(reqs, nil)
	noiseRules := s.logNoiseRules()
	type aggregate struct {
		requests, errors   int64
		prompt, completion int64
		cache              cacheUsageAggregate
		cost               float64
		priced             bool
	}
	models := map[string]*aggregate{}
	for _, item := range reqs {
		model := "unknown"
		usage, usageModel := statsRequestUsage(item)
		if usageModel != "" {
			model = usageModel
		}
		entry := models[model]
		if entry == nil {
			entry = &aggregate{}
			models[model] = entry
		}
		entry.requests++
		if outcomeAnomaly(item.Status, item.Error, requestMeta(item), noiseRules) != "" {
			entry.errors++
		}
		if usage.Found {
			entry.prompt += usage.Prompt
			entry.completion += usage.Completion
		}
		entry.cache.add(usage)
		if usage.Found {
			if cost, ok := usageCostUSD(model, usage.Prompt, usage.Completion); ok {
				entry.priced = true
				entry.cost += cost
			}
		}
	}
	out := make([]map[string]any, 0, len(models))
	modelNames := make([]string, 0, len(models))
	for model := range models {
		modelNames = append(modelNames, model)
	}
	sort.Strings(modelNames)
	for _, model := range modelNames {
		item := models[model]
		out = append(out, map[string]any{"model": model, "requests": item.requests, "prompt": item.prompt, "completion": item.completion, "cache_read_tokens": item.cache.read, "cache_write_tokens": item.cache.write, "cache_observed_requests": item.cache.observed, "cache_rate_observed_requests": item.cache.rateSamples, "cache_input_tokens": item.cache.input, "cache_hit_rate": item.cache.hitRate(), "cost_usd": item.cost, "priced": item.priced, "errors": item.errors})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "days": days, "models": out})
}

func (s *Server) handleStatsRestoreItems(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	limit := queryInt(r, "limit", 200)
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	days, rangeErr := statsRangeDays(r)
	if rangeErr != nil {
		s.writeJSON(w, 400, map[string]any{"error": rangeErr.Error()})
		return
	}
	reqs, events, err := s.store.RestoreBodyStats(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	items := make([]map[string]any, 0, limit)
	total := 0
	seen := make(map[string]struct{})
	for _, request := range reqs {
		if request.Bytes == 0 {
			continue
		}
		seen[request.ID] = struct{}{}
		total++
		if len(items) < limit {
			items = append(items, redactedRestoreSize("RESTORE", request.Bytes))
		}
	}
	for _, event := range events {
		if !strings.EqualFold(strings.TrimSpace(event.Type), "RESTORE") || event.Bytes == 0 {
			continue
		}
		if _, ok := seen[eventRequestKey(storage.Event{ID: event.ID, RequestID: event.RequestID, Meta: event.Meta})]; ok {
			continue
		}
		seen[eventRequestKey(storage.Event{ID: event.ID, RequestID: event.RequestID, Meta: event.Meta})] = struct{}{}
		total++
		if len(items) < limit {
			items = append(items, redactedRestoreSize(event.Type, event.Bytes))
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total_events": total, "items": items, "limit": limit})
}

func redactedRestoreItem(kind string, body []byte) map[string]any {
	return redactedRestoreSize(kind, len(body))
}

func redactedRestoreSize(kind string, size int) map[string]any {
	return map[string]any{"type": kind, "preview": "[redacted]", "bytes": size}
}

func rangeValueOrToday(value string) string {
	if value == "" {
		return "today"
	}
	return value
}
func (s *Server) handleStatsHighlights(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 7)
	if days < 1 {
		days = 1
	}
	response := map[string]any{
		"ok": true, "days": days,
		"total":        map[string]int64{"requests": 0, "mask_events": 0, "restored": 0, "tokens_prompt": 0, "tokens_completion": 0},
		"masked_items": 0,
		"labels":       map[string]int{},
	}
	if s.store == nil {
		s.writeJSON(w, http.StatusOK, response)
		return
	}
	reqs, err := s.store.RequestStats(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "storage_unavailable"})
		return
	}
	prepareReadMetadata(reqs, nil)
	total := response["total"].(map[string]int64)
	labels := response["labels"].(map[string]int)
	maskStats := maskAggregate{}
	for _, item := range reqs {
		total["requests"]++
		if requestHasMaskedItems(item) {
			total["mask_events"]++
		}
		if inferredRestoreStatus(item, false) == "restored" {
			total["restored"]++
		}
		usage, _ := statsRequestUsage(item)
		if usage.Found {
			total["tokens_prompt"] += usage.Prompt
			total["tokens_completion"] += usage.Completion
		}
		addMaskedRequestStats(&maskStats, item)
	}
	for label, count := range maskStats.byLabel {
		labels[label] = count
	}
	response["masked_items"] = maskStats.total
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDemoMask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	masker, ok := s.currentEngine().(MaskingEngine)
	if !ok {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "engine_unavailable"})
		return
	}
	masked, err := masker.Mask(r.Context(), []byte(body.Text))
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "engine_unavailable"})
		return
	}
	items := make([]map[string]any, 0)
	seen := map[string]bool{}
	matches := maskedTokenPattern.FindAllSubmatch(masked, -1)
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		token := string(match[0])
		if seen[token] {
			continue
		}
		seen[token] = true
		items = append(items, map[string]any{"label": string(match[1]), "token": token})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "masked": string(masked), "count": len(matches), "items": items})
}

// extensionRecordEventsEnabled controls the event records used by the
// dashboard's ingress statistics and unified request ledger. Body retention
// follows the same detail-recording and expiry settings as proxy requests.
func (s *Server) extensionRecordEventsEnabled() bool {
	s.configMu.RLock()
	raw, configured := s.control["ext_record_events"]
	s.configMu.RUnlock()
	if !configured {
		return true
	}
	enabled, ok := raw.(bool)
	return !ok || enabled
}

func (s *Server) appendExtensionEvent(requestID, eventType, status string, meta map[string]any) bool {
	// Logging is selected when the request starts, so an in-flight request
	// retains its complete MASK/RESTORE pair if the setting changes later.
	if s.store == nil {
		return false
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["ingress"] = "ext"
	meta["sid"] = requestID
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	// Event payload is intentionally just a route marker.  Never write the
	// extension's original or masked text to the event ledger.
	if err := s.store.AppendEvent(context.Background(), storage.Event{
		ID: newRequestID(), RequestID: requestID, Type: eventType,
		Payload: []byte("POST /api/ext/" + strings.ToLower(eventType)),
		Status:  status, Meta: string(rawMeta), CreatedAt: now, StartedAt: now, EndedAt: now,
	}); err != nil {
		s.metrics.storageErrors.Add(1)
		s.logger.Printf("extension event insert: %v", err)
		return false
	}
	return true
}

func (s *Server) handleExtensionMask(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensionToken(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var body struct {
		Text string `json:"text"`
		// Sid is a client-side context hint. The gateway intentionally does not
		// reuse its mapping; it issues a fresh request-scoped sid below.
		Sid    string `json:"sid"`
		Host   string `json:"host"`
		URL    string `json:"url"`
		Method string `json:"method"`
		// Scope is a random browser-install identity supplied only by the SW.
		// It is combined with the authenticated token and actual page host.
		Scope string `json:"scope"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	record, recordErr := s.newExtensionRecord(r.Context(), body.Text, body.Host, body.URL, body.Method)
	if recordErr != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "blocking": true, "error": "log_storage_unavailable"})
		return
	}
	transferred := false
	failureOutcome, failureStatus := "mask_failed", http.StatusBadGateway
	defer func() {
		if record != nil && !transferred {
			if failureOutcome == "mask_failed" {
				setExtensionRecordMeta(record, map[string]any{"mask_status": "failed", "response_status": "not_sent"})
			} else {
				setExtensionRecordMeta(record, map[string]any{"response_status": failureOutcome})
			}
			record.finish(failureStatus, extensionLogError(failureOutcome))
		}
	}()
	engine := s.currentEngine()
	streamer, ok := engine.(StreamingMaskingEngine)
	if !ok {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "blocking": true, "error": "engine_unavailable"})
		return
	}
	var session MaskingSession
	var sessionErr error
	if body.Scope != "" {
		identity, err := hex.DecodeString(body.Scope)
		if err != nil || len(identity) != 16 || strings.TrimSpace(body.Host) == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "blocking": true, "error": "invalid_browser_scope"})
			return
		}
		durable, ok := engine.(DurableMaskingEngine)
		if !ok || s.store == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "blocking": true, "error": "durable_browser_mapping_unavailable"})
			return
		}
		scope := sha256.Sum256([]byte("browser-v1\x00" + extensionTokenFromRequest(r) + "\x00" + strings.ToLower(body.Host) + "\x00" + body.Scope))
		// Restore RPCs outlive this mask HTTP request. Keep its mapping authority
		// independent of request cancellation; bound each SQL callback instead.
		session, sessionErr = durable.NewDurableMaskingSession(context.WithoutCancel(r.Context()), hex.EncodeToString(scope[:]), browserMappingStore{s.store})
	} else {
		session, sessionErr = streamer.NewMaskingSession(r.Context())
	}
	if sessionErr != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "blocking": true, "error": "engine_unavailable"})
		return
	}
	contextual, ok := session.(ContextualMaskingSession)
	if !ok {
		if aborter, ok := session.(AbortableMaskingSession); ok {
			aborter.Abort()
		}
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "blocking": true, "error": "context_restore_unavailable"})
		return
	}
	maskedHead, pushErr := session.Push(r.Context(), []byte(body.Text))
	maskedTail, finishErr := []byte(nil), error(nil)
	if pushErr == nil {
		maskedTail, finishErr = session.Finish(r.Context())
	}
	if pushErr != nil || finishErr != nil {
		contextual.Release()
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "blocking": true, "error": "engine_unavailable"})
		return
	}
	masked := append(maskedHead, maskedTail...)
	if body.Scope == "" && record == nil && bytes.Equal(masked, []byte(body.Text)) {
		contextual.Release()
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": string(masked), "sid": "", "request_id": ""})
		return
	}
	restore, restoreErr := contextual.NewRestoreSession(r.Context())
	if restoreErr != nil {
		contextual.Release()
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "blocking": true, "error": "engine_unavailable"})
		return
	}
	sid, idErr := newExtensionSessionID()
	if idErr != nil {
		if aborter, ok := restore.(AbortableRestoreSession); ok {
			aborter.Abort()
		}
		contextual.Release()
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "blocking": true, "error": "session_id_unavailable"})
		return
	}
	maskCount := maskedTokenCount(masked)
	items := maskedTokenItemsFromPair([]byte(body.Text), masked)
	if provider, ok := session.(MaskedItemsProvider); ok {
		if exact, itemsErr := provider.MaskedItems(); itemsErr == nil {
			items = exact
		}
	}
	s.recordExtensionMask(record, masked, items)
	now := time.Now()
	s.lifecycleMu.Lock()
	shuttingDown := s.shuttingDown
	s.lifecycleMu.Unlock()
	if shuttingDown {
		if aborter, ok := restore.(AbortableRestoreSession); ok {
			aborter.Abort()
		}
		contextual.Release()
		failureOutcome, failureStatus = "gateway_shutdown", http.StatusServiceUnavailable
		s.writeJSON(w, failureStatus, map[string]any{"ok": false, "blocking": true, "error": "gateway_shutting_down"})
		return
	}
	s.extMu.Lock()
	s.reapExpiredExtensionRestoresLocked(now)
	if len(s.extRestore) >= 1024 {
		s.extMu.Unlock()
		if aborter, ok := restore.(AbortableRestoreSession); ok {
			aborter.Abort()
		}
		contextual.Release()
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "blocking": true, "error": "restore_stream_limit"})
		return
	}
	requestID := ""
	if record != nil {
		requestID = record.item.ID
	}
	s.extRestore[sid] = &extensionRestoreEntry{session: restore, maskOwner: contextual, lastUsed: now, requestID: requestID, record: record, maskedCount: maskCount}
	transferred = true
	s.extMu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": string(masked), "sid": sid, "request_id": requestID})
}

func newExtensionSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ext:" + hex.EncodeToString(raw[:]), nil
}

func (s *Server) handleExtensionRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensionToken(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var body struct {
		Text           string `json:"text"`
		Sid            string `json:"sid"`
		StreamID       string `json:"stream_id"`
		Final          bool   `json:"final"`
		AwaitAck       bool   `json:"await_ack"`
		ContentType    string `json:"content_type"`
		Outcome        string `json:"outcome"`
		ResponseStatus int    `json:"response_status"`
		ValueComplete  bool   `json:"value_complete"`
	}
	if !decodeBody(w, r, s.cfg.MaxBodyBytes, &body) {
		return
	}
	if body.ValueComplete && (body.Final || body.AwaitAck || body.ContentType != "text/plain") {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_value_completion"})
		return
	}
	streamID := strings.TrimSpace(body.StreamID)
	if streamID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "stream_id_required"})
		return
	}
	sid := strings.TrimSpace(body.Sid)
	if !strings.HasPrefix(sid, "ext:") {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "restore_sid_required"})
		return
	}
	now := time.Now()
	s.extMu.Lock()
	s.reapExpiredExtensionRestoresLocked(now)
	entry := s.extRestore[sid]
	if entry == nil {
		s.extMu.Unlock()
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "restore_session_expired"})
		return
	}
	if entry.streamID == "" {
		entry.streamID = streamID
	} else if entry.streamID != streamID {
		s.extMu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_stream_mismatch"})
		return
	}
	// The entry can be reaped after lookup; its closed flag is checked once
	// we acquire it. Never hold the map lock while waiting on native work.
	s.extMu.Unlock()
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "restore_session_expired"})
		return
	}
	entry.lastUsed = now
	if body.ResponseStatus >= 100 && body.ResponseStatus <= 599 {
		entry.upstreamStatus = body.ResponseStatus
	}
	outcome := extensionOutcome(body.Outcome)
	var out []byte
	var err error
	wasFinished := entry.restoreFinished
	if entry.restoreFinished {
		// A flushed stream accepts only an empty terminal acknowledgement.
		// Never Push/Finish or count the same response a second time.
		if !body.Final || body.Text != "" || body.AwaitAck {
			entry.mu.Unlock()
			s.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_confirmation_required"})
			return
		}
	} else if body.Final && outcome != "completed" {
		if aborter, ok := entry.session.(AbortableRestoreSession); ok {
			aborter.Abort()
		}
	} else if body.ValueComplete {
		// A codec has already assembled this logical text value. Flush an
		// independent native channel before JSON re-encoding it; native Push
		// may retain a suffix even when the placeholder itself is complete.
		var valueSession RestoreSession
		valueSession, err = entry.maskOwner.NewRestoreSession(r.Context())
		if err == nil {
			if configurable, ok := valueSession.(ConfigurableRestoreSession); ok {
				err = configurable.SetJSONEscape(r.Context(), false)
			}
		}
		if err == nil {
			out, err = valueSession.Push(r.Context(), []byte(body.Text))
		}
		if err == nil {
			var tail []byte
			tail, err = valueSession.Finish(r.Context())
			out = append(out, tail...)
		}
		if err != nil && valueSession != nil {
			if aborter, ok := valueSession.(AbortableRestoreSession); ok {
				aborter.Abort()
			}
		}
	} else {
		if configurable, ok := entry.session.(ConfigurableRestoreSession); ok {
			ct := strings.ToLower(body.ContentType)
			err = configurable.SetJSONEscape(r.Context(), strings.Contains(ct, "json") || strings.Contains(ct, "event-stream"))
		}
		if err == nil {
			out, err = entry.session.Push(r.Context(), []byte(body.Text))
		}
		if err == nil && body.Final {
			var tail []byte
			tail, err = entry.session.Finish(r.Context())
			out = append(out, tail...)
		}
		if err == nil && body.Final {
			entry.restoreFinished = true
		}
	}
	if err != nil {
		// The extension delivers the original chunk when restoration fails.
		out = []byte(body.Text)
		outcome = "restore_failed"
		if aborter, ok := entry.session.(AbortableRestoreSession); ok {
			aborter.Abort()
		}
	}
	if !wasFinished {
		if body.ValueComplete {
			entry.restoreInputTail, entry.restoreOutputTail = "", ""
		}
		entry.observeResponse([]byte(body.Text), out)
		if body.ValueComplete {
			entry.restoreInputTail, entry.restoreOutputTail = "", ""
		}
	}
	terminal := (body.Final && (!body.AwaitAck || outcome != "completed")) || err != nil
	if terminal {
		status := 0
		if err != nil {
			status = http.StatusBadGateway
		}
		if outcome == "cancelled" {
			status = 499
		}
		entry.finishRecord(outcome, status)
	}
	// Never acquire the map lock while holding the stream lock.
	entry.mu.Unlock()
	if terminal {
		s.extMu.Lock()
		if s.extRestore[sid] == entry {
			delete(s.extRestore, sid)
		}
		s.extMu.Unlock()
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "engine_unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": string(out)})
}

// reapExpiredExtensionRestoresLocked bounds memory/native stream usage when a
// client opens a stream and never sends its final frame. The entry mutex is
// acquired while the global map lock is held, so a request cannot be removed
// between lookup and operation start.
func (s *Server) reapExpiredExtensionRestoresLocked(now time.Time) {
	ttl := s.extensionRestoreTTL()
	for id, entry := range s.extRestore {
		if !entry.mu.TryLock() {
			continue
		}
		if now.Sub(entry.lastUsed) < ttl {
			entry.mu.Unlock()
			continue
		}
		if aborter, ok := entry.session.(AbortableRestoreSession); ok {
			aborter.Abort()
		}
		entry.finishRecord("response_not_completed", 0)
		delete(s.extRestore, id)
		entry.mu.Unlock()
	}
}

// extensionRestoreTTL reads the control-plane value while preserving a
// bounded default for old control files and malformed values. The setting is
// expressed in seconds so it can be edited by the JSON management API.
func (s *Server) extensionRestoreTTL() time.Duration {
	ttl := extensionRestoreIdleTTL
	s.configMu.RLock()
	raw := s.control["session_ttl"]
	s.configMu.RUnlock()
	var seconds float64
	switch value := raw.(type) {
	case float64:
		seconds = value
	case float32:
		seconds = float64(value)
	case int:
		seconds = float64(value)
	case int64:
		seconds = float64(value)
	case json.Number:
		seconds, _ = value.Float64()
	}
	if seconds >= 30 && seconds <= 24*60*60 {
		ttl = time.Duration(seconds * float64(time.Second))
	}
	return ttl
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ok := true
	storageError := ""
	var storageSnapshot storage.Snapshot
	if s.store != nil {
		if err := s.store.Health(r.Context()); err != nil {
			ok = false
			storageError = "unavailable"
		}
		storageSnapshot = s.store.Snapshot()
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	response := map[string]any{"ok": ok}
	if s.store != nil {
		response["storage"] = storageHealthPayload(storageSnapshot)
		if storageError != "" {
			response["storage_error"] = storageError
		}
	}
	s.writeJSON(w, status, response)
}

func storageHealthPayload(snapshot storage.Snapshot) map[string]any {
	return map[string]any{
		"mode":             snapshot.Mode,
		"queue_depth":      snapshot.QueueDepth,
		"queue_capacity":   snapshot.QueueCapacity,
		"last_write_error": snapshot.LastWriteErr,
		"open_connections": snapshot.DB.OpenConnections,
		"in_use":           snapshot.DB.InUse,
		"idle":             snapshot.DB.Idle,
		"wait_count":       snapshot.DB.WaitCount,
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	if s.store == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	events, hasMore, windowLimited, err := s.filteredLogEvents(r.Context(), r)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
		return
	}
	nextSince := int64(0)
	if len(events) > 0 {
		nextSince, _ = events[len(events)-1]["seq"].(int64)
	}
	if nextSince == 0 && len(events) > 0 {
		// JSON encoders preserve int64, but this fallback keeps the response
		// useful if a future view implementation changes the concrete number type.
		if value, ok := events[len(events)-1]["seq"].(float64); ok {
			nextSince = int64(value)
		}
	}
	storageSnapshot := s.store.Snapshot()
	response := map[string]any{
		"events": events, "tail": []string{}, "retention_days": s.logRetentionDays(),
		"store": storageSnapshot.Mode, "db": "", "sensitive_only": r.URL.Query().Get("sensitive") == "1",
		"total": len(events), "has_more": hasMore, "window_limited": windowLimited, "next_since": nextSince, "reset": false,
	}
	if r.URL.Query().Get("events_only") != "1" {
		response["list"] = events
	}
	s.writeJSON(w, http.StatusOK, response)
}

const maxLogQueryEvents = 5000

// filteredLogEvents is deliberately implemented above the storage layer. The
// event metadata is JSON and differs between imported legacy rows and native
// rows, so applying these predicates in Go keeps SQLite/MySQL behavior equal.
func (s *Server) filteredLogEvents(ctx context.Context, r *http.Request) ([]map[string]any, bool, bool, error) {
	noiseRules := s.logNoiseRules()
	limit := queryInt(r, "limit", 100)
	if limit > maxLogQueryEvents {
		limit = maxLogQueryEvents
	}
	var items []storage.Event
	var err error
	if r.URL.Query().Get("fulltext") == "1" && strings.TrimSpace(r.URL.Query().Get("q")) != "" {
		items, err = s.store.ListAllEvents(ctx, maxLogQueryEvents+1)
	} else {
		items, err = s.store.ListLogSummaries(ctx, maxLogQueryEvents+1)
	}
	if err != nil {
		return nil, false, false, err
	}
	windowLimited := len(items) > maxLogQueryEvents
	if windowLimited {
		items = items[:maxLogQueryEvents]
	}
	prepareReadMetadata(nil, items)
	sensitiveRequestIDs := sensitiveRequestIDs(items)
	masks := make(map[string]storage.Event)
	for _, event := range items {
		if strings.EqualFold(event.Type, "MASK") {
			masks[eventRequestKey(event)] = event
		}
	}
	since := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			since = parsed
		}
	}
	typ := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("type")))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	fulltext := r.URL.Query().Get("fulltext") == "1"
	var bodyPattern *regexp.Regexp
	if fulltext && query != "" {
		bodyPattern = regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	}
	sensitive := r.URL.Query().Get("sensitive") == "1"
	ingress := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("ingress")))
	hideNoise := r.URL.Query().Get("hide_noise") == "1"
	chains := r.URL.Query().Get("chains") == "1"
	includeAudit := r.URL.Query().Get("include_audit") == "1"
	type logItem struct {
		event storage.Event
		seq   int64
	}
	filtered := make([]logItem, 0, len(items))
	matchedRequests := make(map[string]bool)
	var pendingText []logItem
	var bodyIDs []string
	requestedBodies := map[string]bool{}
	candidates := make([]logItem, 0, len(items))
	for _, rawEvent := range items {
		if err := ctx.Err(); err != nil {
			return nil, false, false, err
		}
		if eventMeta(rawEvent)["log_expired"] == true {
			continue
		}
		if strings.HasPrefix(rawEvent.Type, "AUDIT_") && (!includeAudit || ingress != "") {
			continue
		}
		event := projectLogNoise(normalizeLogOutcome(normalizeRestoreEvent(rawEvent, sensitiveRequestIDs), masks), noiseRules)
		seq := eventSequence(event)
		if seq <= since {
			continue
		}
		item := logItem{event: event, seq: seq}
		candidates = append(candidates, item)
		if !logEventMatches(event, typ, sensitive, "", false, ingress, sensitiveRequestIDs) ||
			(hideNoise && isNoiseLogType(displayLogEventType(event, sensitiveRequestIDs))) {
			continue
		}
		if !logEventMatches(event, "", false, query, fulltext, "", sensitiveRequestIDs) {
			key := eventRequestKey(event)
			if !fulltext {
				continue
			}
			pendingText = append(pendingText, item)
			if key != "" && !requestedBodies[key] {
				requestedBodies[key] = true
				bodyIDs = append(bodyIDs, key)
			}
			continue
		}
		filtered = append(filtered, item)
		matchedRequests[eventRequestKey(event)] = true
	}

	if len(pendingText) > 0 {
		bodyMatches := map[string]bool{}
		const searchBatch = 32
		for start := 0; start < len(bodyIDs); start += searchBatch {
			end := start + searchBatch
			if end > len(bodyIDs) {
				end = len(bodyIDs)
			}
			requests, readErr := s.store.RequestSearchBodies(ctx, bodyIDs[start:end])
			if readErr != nil {
				return nil, false, false, readErr
			}
			prepareReadMetadata(requests, nil)
			for _, request := range requests {
				if err := ctx.Err(); err != nil {
					return nil, false, false, err
				}
				bodyMatches[request.ID] = s.logRequestBodyMatches(ctx, request, query, bodyPattern)
				if err := ctx.Err(); err != nil {
					return nil, false, false, err
				}
			}
		}
		for _, item := range pendingText {
			key := eventRequestKey(item.event)
			if bodyMatches[key] {
				filtered = append(filtered, item)
				matchedRequests[key] = true
			}
		}
	}
	if chains {
		selected := make(map[string]bool, len(filtered))
		for _, item := range filtered {
			selected[item.event.ID] = true
		}
		for _, item := range candidates {
			if !selected[item.event.ID] && matchedRequests[eventRequestKey(item.event)] &&
				(item.event.Type == "MASK" || item.event.Type == "RESTORE") {
				filtered = append(filtered, item)
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].seq == filtered[j].seq {
			return filtered[i].event.ID < filtered[j].event.ID
		}
		return filtered[i].seq < filtered[j].seq
	})
	allCount := len(filtered)
	hasMore := allCount > limit
	if hasMore {
		if r.URL.Query().Get("latest") == "1" {
			filtered = filtered[len(filtered)-limit:]
		} else {
			// Legacy numeric cursors cannot distinguish equal timestamps. Return
			// the whole boundary group so advancing since cannot skip its peers.
			end := limit
			for end < len(filtered) && filtered[end].seq == filtered[end-1].seq {
				end++
			}
			hasMore = end < len(filtered)
			filtered = filtered[:end]
		}
	}
	if chains {
		// Paging selects a window, then keeps the selected request's complete
		// pair. A boundary may exceed limit rather than silently split a chain.
		selectedIDs := make(map[string]bool, len(filtered))
		selectedRequests := make(map[string]bool, len(filtered))
		window := append([]logItem(nil), filtered...)
		for _, item := range window {
			selectedIDs[item.event.ID] = true
			selectedRequests[eventRequestKey(item.event)] = true
		}
		for _, item := range candidates {
			if !selectedIDs[item.event.ID] && selectedRequests[eventRequestKey(item.event)] &&
				(item.event.Type == "MASK" || item.event.Type == "RESTORE") {
				window = append(window, item)
				selectedIDs[item.event.ID] = true
			}
		}
		filtered = window
		hasMore = len(filtered) < allCount
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].seq == filtered[j].seq {
				return filtered[i].event.ID < filtered[j].event.ID
			}
			return filtered[i].seq < filtered[j].seq
		})
	}
	out := make([]map[string]any, 0, len(filtered))
	for _, item := range filtered {
		view := logEventView(item.event, false)
		view["display_type"] = displayLogEventType(item.event, sensitiveRequestIDs)
		out = append(out, view)
	}
	return out, hasMore, windowLimited, nil
}

func eventSequence(event storage.Event) int64 {
	if event.CreatedAt.IsZero() {
		return 0
	}
	// Legacy timestamp cursor; it is not a unique event identity. Native MySQL
	// tables can round timestamps to seconds. Current clients use snapshots and
	// detail_id so ties and late completion writes remain visible.
	return event.CreatedAt.UnixMicro()
}

func eventMeta(event storage.Event) map[string]any {
	if event.StatsMetaCanonical {
		return event.StatsMeta
	}
	if event.StatsMeta != nil {
		return canonicalRestoreEventMeta(event.StatsMeta, event.Type)
	}
	var meta map[string]any
	if event.Meta != "" && json.Unmarshal([]byte(event.Meta), &meta) == nil {
		return canonicalRestoreEventMeta(meta, event.Type)
	}
	return map[string]any{}
}

func logEventMatches(event storage.Event, typ string, sensitive bool, query string, fulltext bool, ingress string, sensitiveRequestIDs map[string]struct{}) bool {
	meta := eventMeta(event)
	if typ == "MASK" || typ == "RESTORE" {
		if !eventHasSensitiveEvidence(event, sensitiveRequestIDs) {
			return false
		}
	}
	if typ != "" {
		upper := displayLogEventType(event, sensitiveRequestIDs)
		if typ == "MASK" || typ == "RESTORE" {
			upper = strings.ToUpper(event.Type)
		}
		if typ == "PASS_COMBINED" {
			if upper != "PASS" && upper != "BYPASS" && upper != "SKIP" {
				return false
			}
		} else if upper != typ {
			return false
		}
	}
	if sensitive && !eventHasSensitiveEvidence(event, sensitiveRequestIDs) {
		return false
	}
	if ingress != "" {
		if ingressFromMeta(meta) != ingress {
			return false
		}
	}
	if query == "" {
		return true
	}
	parts := []string{event.ID, event.RequestID, event.Type, event.Status, event.Error, event.Meta}
	if fulltext {
		parts = append(parts, string(event.Payload), string(event.Raw), string(event.Masked), string(event.Upstream), string(event.Restored))
	}
	for _, part := range parts {
		if strings.Contains(strings.ToLower(part), query) {
			return true
		}
	}
	return false
}

func sensitiveRequestIDs(events []storage.Event) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, event := range events {
		if strings.EqualFold(strings.TrimSpace(event.Type), "MASK") && maskEventItemCount(event) > 0 {
			ids[eventRequestKey(event)] = struct{}{}
		}
	}
	return ids
}

func normalizeRestoreEvent(event storage.Event, requestIDs map[string]struct{}) storage.Event {
	if !strings.EqualFold(strings.TrimSpace(event.Type), "RESTORE") {
		return event
	}
	if _, hasMaskedItems := requestIDs[eventRequestKey(event)]; !hasMaskedItems {
		return event
	}
	if event.Status != "" && event.Status != "no_sensitive_data" {
		return event
	}
	event.Status = "no_placeholder_in_response"
	meta := cloneReadMetadata(eventMeta(event))
	event.StatsMeta = meta
	event.StatsMetaCanonical = true
	meta["status"] = event.Status
	meta["restore_status"] = event.Status
	if raw, err := json.Marshal(meta); err == nil {
		event.Meta = string(raw)
	}
	return event
}

// eventHasSensitiveEvidence defines the contract behind the Logs page's
// "sensitive only" filter. Operational MASK/RESTORE rows are emitted for
// every proxied request so timing and cache diagnostics remain available, but
// a zero-count MASK (and its no-op RESTORE pair) is not a sensitive event.
// The requestIDs argument lets RESTORE inherit evidence from its matching MASK
// row, which is needed for older rows that recorded no_sensitive_data despite
// having masked input.
func eventHasSensitiveEvidence(event storage.Event, requestIDs map[string]struct{}) bool {
	typ := strings.ToUpper(strings.TrimSpace(event.Type))
	switch typ {
	case "MASK":
		return maskEventItemCount(event) > 0
	case "RESTORE":
		meta := eventMeta(event)
		for _, key := range []string{"mask_count", "restored", "unresolved", "degraded"} {
			if count, ok := numericMetaInt(meta[key]); ok && count > 0 {
				return true
			}
		}
		if restored, ok := numericMetaInt(meta["restored"]); ok && restored > 0 {
			return true
		}
		if requestIDs != nil {
			_, ok := requestIDs[eventRequestKey(event)]
			return ok
		}
		return event.Status == "restored" || event.Status == "no_placeholder_in_response"
	default:
		return false
	}
}

func numericMetaInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		parsed, err := n.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func logEventView(event storage.Event, detail bool) map[string]any {
	meta := eventMeta(event)
	seq := eventSequence(event)
	view := map[string]any{
		"id": seq, "detail_id": event.ID, "seq": seq, "ts": event.CreatedAt.Unix(),
		"request_id": event.RequestID, "type": event.Type, "status": canonicalRestoreStatus(event.Status, meta),
		"error": event.Error, "created_at": event.CreatedAt, "items": []any{},
	}
	view["display_type"] = displayLogEventType(event, nil)
	for _, key := range []string{
		"sid", "host", "method", "path", "count", "mask_count", "restored", "unresolved", "degraded",
		"http_status", "ingress", "evidence", "severity", "signal_type", "probe_id",
		"failure_stage", "upstream_status", "upstream_status_observed", "noise_whitelisted",
		"request_hash", "response_hash", "matched_rules", "stream_mode", "stream_actual",
		"mask_ms", "first_byte_ms", "upstream_ms", "total_ms", "cost_usd", "model", "client_app", "reason", "msg",
	} {
		if value, ok := meta[key]; ok {
			view[key] = value
		}
	}
	if _, ok := view["sid"]; !ok && event.RequestID != "" {
		view["sid"] = event.RequestID
	}
	if _, ok := view["path"]; !ok && len(event.Payload) > 0 {
		view["path"] = string(event.Payload)
	}
	if _, ok := view["signal_type"]; !ok && strings.HasPrefix(event.Type, "AUDIT_") {
		view["signal_type"] = strings.TrimPrefix(event.Type, "AUDIT_")
	}
	if detail {
		// Payload columns may include plaintext. Only expose bounded masked
		// request material; raw/upstream/restored columns remain internal.
		if len(event.Masked) > 0 {
			view["req_preview"] = boundedText(event.Masked, 4096)
		}
		view["body_unavailable"] = true
	}
	if rawItems, ok := meta["items"].([]any); ok {
		items := make([]map[string]any, 0, len(rawItems))
		for _, raw := range rawItems {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			safe := map[string]any{}
			// The user-facing audit view intentionally shows the original value
			// for every matched token, including credentials. This is per-token
			// metadata only; the full raw request/response remains unavailable.
			keys := []string{"label", "preview", "original", "display", "tok", "sha256", "length", "hash", "restored", "from_history"}
			for _, key := range keys {
				if value, exists := item[key]; exists {
					safe[key] = value
				}
			}
			items = append(items, safe)
		}
		view["items"] = items
	}
	return view
}

func boundedText(value []byte, max int) string {
	if len(value) > max {
		return string(value[:max])
	}
	return string(value)
}

func decodeBody(w http.ResponseWriter, r *http.Request, max int64, target any) bool {
	if r.Body == nil {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(target); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_json"})
		return false
	}
	return true
}

func queryInt(r *http.Request, key string, fallback int) int {
	if value, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && value > 0 {
		return value
	}
	return fallback
}

func setNested(root map[string]any, path []string, value any) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		root[path[0]] = value
		return
	}
	child, ok := root[path[0]].(map[string]any)
	if !ok {
		child = map[string]any{}
		root[path[0]] = child
	}
	setNested(child, path[1:], value)
}

func getNested(root map[string]any, path []string) (any, bool) {
	if len(path) == 0 {
		return root, true
	}
	var current any = root
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func applyConfigPatch(root map[string]any, path []string, op string, value any, match string) {
	if op == "set" || op == "" {
		setNested(root, path, value)
		return
	}
	current, _ := getNested(root, path)
	switch op {
	case "merge":
		target, ok := current.(map[string]any)
		incoming, incomingOK := value.(map[string]any)
		if !ok {
			target = map[string]any{}
			setNested(root, path, target)
		}
		if incomingOK {
			for key, item := range incoming {
				target[key] = item
			}
		}
	case "map_del":
		if target, ok := current.(map[string]any); ok {
			for _, key := range valuesAsStrings(value) {
				delete(target, key)
			}
		}
	case "list_add", "list_remove":
		items := anySlice(current)
		values := anySlice(value)
		if len(values) == 0 && value != nil {
			values = []any{value}
		}
		if op == "list_add" {
			for _, item := range values {
				found := false
				for _, existing := range items {
					if reflect.DeepEqual(existing, item) {
						found = true
						break
					}
				}
				if !found {
					items = append(items, item)
				}
			}
		} else {
			filtered := items[:0]
			for _, existing := range items {
				remove := false
				for _, item := range values {
					if reflect.DeepEqual(existing, item) {
						remove = true
						break
					}
				}
				if !remove {
					filtered = append(filtered, existing)
				}
			}
			items = filtered
		}
		setNested(root, path, items)
	case "list_upsert", "list_del":
		items := anySlice(current)
		name := match
		if object, ok := value.(map[string]any); ok && name == "" {
			if v, ok := object["name"].(string); ok {
				name = v
			}
		}
		index := -1
		for i, item := range items {
			if object, ok := item.(map[string]any); ok && fmt.Sprint(object["name"]) == name {
				index = i
				break
			}
		}
		if op == "list_del" {
			if index >= 0 {
				items = append(items[:index], items[index+1:]...)
			}
		} else if index >= 0 {
			items[index] = value
		} else {
			items = append(items, value)
		}
		setNested(root, path, items)
	}
}

func anySlice(value any) []any {
	if items, ok := value.([]any); ok {
		return append([]any(nil), items...)
	}
	return nil
}

func valuesAsStrings(value any) []string {
	items := anySlice(value)
	if len(items) == 0 && value != nil {
		items = []any{value}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
