//go:build silencer_engine && cgo

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func contextualCredentialPlaceholder(label string) *regexp.Regexp {
	// Native placeholders retain at most 12 characters of the rule label.
	if len(label) > 12 {
		label = label[:12]
	}
	return regexp.MustCompile(`\{\{` + label + `_[^}]+\}\}`)
}

func TestNativeContextualCredentialRulesMaskAndRestore(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"builtin_rules": map[string]any{"COOKIE_SESSION": true, "BASIC_AUTH": true, "URL_QUERY_SECRET": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: raw})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	basic := base64.StdEncoding.EncodeToString([]byte("demo:basic-test-password"))
	cases := []struct {
		name, label, input, want string
	}{
		{"cookie header", "COOKIE_SESSION", "Cookie: theme=dark; sid=demo-session-12345;lang=zh", "Cookie: theme=dark; sid=<masked>;lang=zh"},
		{"adjacent cookies", "COOKIE_SESSION", "Cookie: sessionid=first-session-123;JSESSIONID=second-session-456", "Cookie: sessionid=<masked>;JSESSIONID=<masked>"},
		{"cookie value punctuation", "COOKIE_SESSION", "Cookie: sid=demo-session&part#end; theme=dark", "Cookie: sid=<masked>; theme=dark"},
		{"cookie value short prefix", "COOKIE_SESSION", "Cookie: sessionid=abc&#demo-session-12345; lang=zh", "Cookie: sessionid=<masked>; lang=zh"},
		{"cookie JSON field", "COOKIE_SESSION", `{"session_id":"demo-session-12345","theme":"dark"}`, `{"session_id":"<masked>","theme":"dark"}`},
		{"signed cookie value", "COOKIE_SESSION", "Cookie: connect.sid=s%3Ademo-session-12345.sig; theme=dark", "Cookie: connect.sid=<masked>; theme=dark"},
		{"framework cookie", "COOKIE_SESSION", "Cookie: __Secure-next-auth.session-token=demo-session-12345; theme=dark", "Cookie: __Secure-next-auth.session-token=<masked>; theme=dark"},
		{"mixed case session", "COOKIE_SESSION", "SeSsIoN_Id=demo-session-12345", "SeSsIoN_Id=<masked>"},
		{"basic header", "BASIC_AUTH", "Authorization: Basic " + basic, "Authorization: Basic <masked>"},
		{"basic JSON header", "BASIC_AUTH", `{"Authorization":"Basic ` + basic + `","Accept":"application/json"}`, `{"Authorization":"Basic <masked>","Accept":"application/json"}`},
		{"basic proxy header", "BASIC_AUTH", "pRoXy-AuThOrIzAtIoN: bAsIc " + basic + "\r\nAccept: text/plain", "pRoXy-AuThOrIzAtIoN: bAsIc <masked>\r\nAccept: text/plain"},
		{"adjacent basic headers", "BASIC_AUTH", "Authorization: Basic " + basic + "\nProxy-Authorization: Basic " + basic, "Authorization: Basic <masked>\nProxy-Authorization: Basic <masked>"},
		{"query token", "URL_QUERY_SECRET", "https://example.invalid/path?access_token=demo-token-12345&lang=zh#details", "https://example.invalid/path?access_token=<masked>&lang=zh#details"},
		{"adjacent query credentials", "URL_QUERY_SECRET", "https://example.invalid/?api_key=demo%2Bkey%3D&password=demo-pass+123&lang=zh", "https://example.invalid/?api_key=<masked>&password=<masked>&lang=zh"},
		{"mixed case query", "URL_QUERY_SECRET", "https://example.invalid/?AcCeSs_ToKeN=demo-token-12345&id=42", "https://example.invalid/?AcCeSs_ToKeN=<masked>&id=42"},
		{"query signature", "URL_QUERY_SECRET", "https://example.invalid/?X-Amz-Signature=demo-signature-123&id=42", "https://example.invalid/?X-Amz-Signature=<masked>&id=42"},
		{"query JSON value", "URL_QUERY_SECRET", `{"url":"https://example.invalid/?api_key=demo%3Dkey%2Bvalue&lang=zh"}`, `{"url":"https://example.invalid/?api_key=<masked>&lang=zh"}`},
		{"cookie preferences", "", "Cookie: theme=dark1234; locale=en", "Cookie: theme=dark1234; locale=en"},
		{"short session field", "", "session_id=short", "session_id=short"},
		{"unrelated session field", "", "session_count=12345678; not_session_id=demo-session-12345", "session_count=12345678; not_session_id=demo-session-12345"},
		{"ordinary Basic text", "", "Basic education Tm90SGVhZGVy", "Basic education Tm90SGVhZGVy"},
		{"other auth scheme", "", "Authorization: Bearer demo-token-12345", "Authorization: Bearer demo-token-12345"},
		{"non-base64 auth value", "", "Authorization: Basic not_base64", "Authorization: Basic not_base64"},
		{"ordinary query parameters", "", "https://example.invalid/?page=token&password_hint=example", "https://example.invalid/?page=token&password_hint=example"},
		{"query session outside cookie context", "", "https://example.invalid/?sid=demo-session-12345&lang=zh#details", "https://example.invalid/?sid=demo-session-12345&lang=zh#details"},
		{"empty query credential", "", "https://example.invalid/?token=&lang=zh", "https://example.invalid/?token=&lang=zh"},
		{"ordinary token field", "", `{"token":"demo-token-12345"}`, `{"token":"demo-token-12345"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			masking, err := engine.(StreamingMaskingEngine).NewMaskingSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			owner := masking.(ContextualMaskingSession)
			defer owner.Release()
			mid := len(tc.input) / 2
			first, err := masking.Push(ctx, []byte(tc.input[:mid]))
			if err != nil {
				t.Fatal(err)
			}
			second, err := masking.Push(ctx, []byte(tc.input[mid:]))
			if err != nil {
				t.Fatal(err)
			}
			tail, err := masking.Finish(ctx)
			if err != nil {
				t.Fatal(err)
			}
			masked := append(append(first, second...), tail...)
			normalized := string(masked)
			if tc.label != "" {
				placeholder := contextualCredentialPlaceholder(tc.label)
				normalized = placeholder.ReplaceAllString(normalized, "<masked>")
			}
			if normalized != tc.want {
				t.Fatalf("masked=%q, want %q", normalized, tc.want)
			}
			if json.Valid([]byte(tc.input)) && !json.Valid(masked) {
				t.Fatalf("masked JSON is invalid: %q", masked)
			}
			restore, err := owner.NewRestoreSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if json.Valid([]byte(tc.input)) {
				if err := restore.(ConfigurableRestoreSession).SetJSONEscape(ctx, true); err != nil {
					t.Fatal(err)
				}
			}
			restored, err := restore.Push(ctx, masked)
			if err != nil {
				t.Fatal(err)
			}
			finish, err := restore.Finish(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(append(restored, finish...)); got != tc.input {
				t.Fatalf("restored=%q, want %q", got, tc.input)
			}
		})
	}
}

func TestNativeContextualCredentialsPrecedeFragmentRules(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("demo:basic-test-password"))
	control := defaultControlConfig()
	builtin := control["builtin_rules"].(map[string]any)
	for _, label := range []string{"COOKIE_SESSION", "BASIC_AUTH", "URL_QUERY_SECRET", "SECRET"} {
		builtin[label] = true
	}
	control["sensitive"] = map[string]any{"CUSTOM": []any{"demo", basic[:4]}}
	raw, err := engineConfigFromControl(control)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: raw})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	for _, tc := range []struct{ label, input, want string }{
		{"COOKIE_SESSION", "Cookie: sid=demo-session-12345", "Cookie: sid=<masked>"},
		{"BASIC_AUTH", "Authorization: Basic " + basic, "Authorization: Basic <masked>"},
		{"URL_QUERY_SECRET", "https://example.invalid/?token=" + strings.Repeat("demo1234", 12) + "&page=1", "https://example.invalid/?token=<masked>&page=1"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			placeholder := contextualCredentialPlaceholder(tc.label)
			if got := placeholder.ReplaceAllString(string(masked), "<masked>"); got != tc.want {
				t.Fatalf("credential was split by a fragment rule: %q", got)
			}
		})
	}
}

func TestNativeContextualCredentialRulesCanBeDisabled(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"builtin_rules": map[string]any{"COOKIE_SESSION": false, "BASIC_AUTH": false, "URL_QUERY_SECRET": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: raw})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	input := "Cookie: sid=demo-session-12345\nAuthorization: Basic " + base64.StdEncoding.EncodeToString([]byte("demo:password")) + "\nhttps://example.invalid/?token=demo-token-12345"
	masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if string(masked) != input {
		t.Fatalf("disabled rules masked the input: %q", masked)
	}
}
