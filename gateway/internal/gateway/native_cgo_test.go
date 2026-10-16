//go:build silencer_engine && cgo

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestNativeExtensionContextCacheStableAndRestoresRequestScoped(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"field_paths":["user.email"]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", MaxBodyBytes: 1 << 20}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	post := func(path string, body map[string]any, output any) {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := gateway.Client().Post(gateway.URL+path, "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			response, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, response)
		}
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		Text string `json:"text"`
		SID  string `json:"sid"`
	}
	inputs := []string{
		`{"model":"m","user":{"email":"alice@example.com"},"note":"alice@example.com bob@example.com"}`,
		`{"model":"m","user":{"email":"carol@example.com"},"note":"dave@example.com dave@example.com"}`,
	}
	masked := make([]result, 2)
	for i, input := range inputs {
		post("/api/ext/mask", map[string]any{"text": input, "sid": "ignored-client-sid"}, &masked[i])
	}
	if masked[0].Text != masked[1].Text {
		t.Fatalf("extension masked bytes changed across sensitive-value/duplicate changes:\nfirst:  %s\nsecond: %s", masked[0].Text, masked[1].Text)
	}
	if masked[0].SID == "" || masked[1].SID == "" || masked[0].SID == masked[1].SID {
		t.Fatalf("extension mask sessions are not request-scoped: %#v", masked)
	}
	for i := range masked {
		var restored result
		post("/api/ext/restore", map[string]any{
			"sid": masked[i].SID, "stream_id": fmt.Sprintf("stream-%d", i),
			"text": masked[i].Text, "final": true,
		}, &restored)
		if restored.Text != inputs[i] {
			t.Errorf("request %d restored=%q want=%q", i, restored.Text, inputs[i])
		}
	}
}

func TestNativeMaskingSessionReturnsExactItemsForLargeRequest(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"alice@example.com","label":"EMAIL"},{"needle":"sk-live-example-key","label":"API_KEY"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	session, err := engine.(StreamingMaskingEngine).NewMaskingSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const body = "alice@example.com sk-live-example-key"
	input := append(bytes.Repeat([]byte("x"), 70<<10), []byte(" "+body)...)
	var masked []byte
	for start := 0; start < len(input); start += 4096 {
		end := start + 4096
		if end > len(input) {
			end = len(input)
		}
		chunk, pushErr := session.Push(context.Background(), input[start:end])
		if pushErr != nil {
			t.Fatal(pushErr)
		}
		masked = append(masked, chunk...)
	}
	tail, err := session.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	masked = append(masked, tail...)
	if bytes.Contains(masked, []byte("alice@example.com")) || bytes.Contains(masked, []byte("sk-live-example-key")) {
		t.Fatalf("sensitive text was not masked: %q", masked[len(masked)-(len(body)+128):])
	}
	provider, ok := session.(MaskedItemsProvider)
	if !ok {
		t.Fatal("native mask session does not provide exact masked items")
	}
	items, err := provider.MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	byOriginal := map[string]string{}
	for _, item := range items {
		original, _ := item["original"].(string)
		label, _ := item["label"].(string)
		byOriginal[original] = label
	}
	if len(items) != 2 || byOriginal["alice@example.com"] != "EMAIL" || byOriginal["sk-live-example-key"] != "API_KEY" {
		t.Fatalf("exact items=%+v", items)
	}
}

func TestNativeRestoreMatchesLegacyFormsAndJSONEscapes(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"alice@example.com","label":"EMAIL"},{"needle":"a\"b","label":"TERM"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()

	masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(string(masked), "{{EMAIL_"), "}}")
	if token == string(masked) {
		t.Fatalf("unexpected masked token %q", masked)
	}
	suffix := token
	for _, form := range []string{
		"{{ email_" + suffix + " }}",
		"EMAIL_" + suffix,
		"\\{\\{EMAIL_" + suffix + "\\}\\}",
	} {
		session, err := engine.(StreamingEngine).NewRestoreSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := session.Push(context.Background(), []byte(form))
		if err != nil {
			t.Fatal(err)
		}
		tail, err := session.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if string(append(got, tail...)) != "alice@example.com" {
			t.Errorf("restore form %q = %q", form, append(got, tail...))
		}
	}

	masking, err := engine.(StreamingMaskingEngine).NewMaskingSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	contextualMask, ok := masking.(ContextualMaskingSession)
	if !ok {
		t.Fatal("native masking session is not contextual")
	}
	jsonInput := []byte(`{"value":"a\"b"}`)
	maskedHead, err := masking.Push(context.Background(), jsonInput)
	if err != nil {
		t.Fatal(err)
	}
	maskedTail, err := masking.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	jsonMasked := append(maskedHead, maskedTail...)
	restore, err := contextualMask.NewRestoreSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configured := restore.(ConfigurableRestoreSession)
	if err := configured.SetJSONEscape(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	jsonResponse := jsonMasked
	// The masked JSON contains a request-scoped token. Restore it inside its
	// JSON string slot and confirm the resulting document parses to the source.
	restored, err := restore.Push(context.Background(), jsonResponse)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := restore.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(append(restored, tail...), &decoded); err != nil {
		t.Fatalf("restored JSON is invalid: %q: %v", append(restored, tail...), err)
	}
	if decoded["value"] != `a"b` {
		t.Fatalf("restored JSON value=%q", decoded["value"])
	}
}

func TestNativeJSONMaskingSkipsWhitespace(t *testing.T) {
	engine, err := newNativeEngine(Config{
		Engine:       "cpp",
		EngineConfig: `{"field_paths":["user.email"],"rules":[{"needle":"secret","label":"SECRET"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()

	masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte("\n  {\"user\":{\"email\":\"alice@example.com\"},\"note\":\"secret\"}"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(masked)
	if strings.Contains(got, "alice@example.com") || strings.Contains(got, "secret") {
		t.Fatalf("native JSON masking leaked plaintext: %q", got)
	}
	if !strings.Contains(got, "{{JSON_") || !strings.Contains(got, "{{SECRET_") {
		t.Fatalf("native JSON masking missed configured rules: %q", got)
	}
}

func TestNativeOffsetMapBinding(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	native := engine.(*nativeEngine)
	masked, offsets, err := native.MaskWithMap(context.Background(), []byte("prefix alice@example.com suffix"))
	if err != nil {
		t.Fatal(err)
	}
	if offsets == nil {
		t.Fatal("expected offset map")
	}
	defer offsets.Close()
	if !strings.Contains(string(masked), "{{EMAIL_") {
		t.Fatalf("email was not masked: %q", masked)
	}
	original, err := offsets.Lookup(len("prefix "))
	if err != nil || original != len("prefix ") {
		t.Fatalf("prefix offset=%d err=%v", original, err)
	}
	if _, err := offsets.Lookup(len(masked) + 1); err == nil {
		t.Fatal("out-of-range lookup unexpectedly succeeded")
	}

	jsonEngine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"field_paths":["user.email"]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer jsonEngine.(CloseableEngine).Close()
	jsonMasked, jsonOffsets, err := jsonEngine.(*nativeEngine).MaskWithMap(context.Background(), []byte(`{"user":{"email":"alice@example.com"},"note":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer jsonOffsets.Close()
	if strings.Contains(string(jsonMasked), "alice@example.com") || !strings.Contains(string(jsonMasked), "{{JSON_") {
		t.Fatalf("JSON offset-map masking failed: %q", jsonMasked)
	}
	if end, err := jsonOffsets.Lookup(len(jsonMasked)); err != nil || end != len(`{"user":{"email":"alice@example.com"},"note":"ok"}`) {
		t.Fatalf("JSON end offset=%d err=%v", end, err)
	}
}

func TestNativeStreamingMaskingRequest(t *testing.T) {
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 1024}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()
	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/api/requests/chat", strings.NewReader("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if strings.Contains(upstreamBody, "alice@example.com") || !strings.Contains(upstreamBody, "{{EMAIL_") {
		t.Fatalf("upstream received unmasked request: %q", upstreamBody)
	}
}

func TestNativeProxyKeepsRepeatedMaskedPromptBytesStable(t *testing.T) {
	var mu sync.Mutex
	var received [][]byte
	var receivedHeaders []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		received = append(received, append([]byte(nil), body...))
		receivedHeaders = append(receivedHeaders, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{
		Engine:       "cpp",
		EngineConfig: `{"field_paths":["user.email"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	requestBody := []byte("{\n  \"model\" : \"test\", \"prompt_cache_key\":\"stable-session\", \"system\":[{\"type\":\"text\",\"text\":\"unchanged prefix\",\"cache_control\":{\"type\":\"ephemeral\"}}], \"user\" : { \"email\" : \"alice@example.com\" }, \"messages\" : [{\"content\":\"unchanged prefix\"}]\n}")
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/api/requests/v1/chat/completions", bytes.NewReader(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("OpenAI-Beta", "prompt-caching-2024-07-31")
		req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status=%d", i, resp.StatusCode)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("upstream received %d requests", len(received))
	}
	if !bytes.Equal(received[0], received[1]) {
		t.Fatalf("masked prompt changed across identical requests:\nfirst:  %s\nsecond: %s", received[0], received[1])
	}
	if bytes.Contains(received[0], []byte("alice@example.com")) || !bytes.Contains(received[0], []byte("{{JSON_")) {
		t.Fatalf("upstream body is not safely masked: %s", received[0])
	}
	if !bytes.Contains(received[0], []byte("\"model\" : \"test\"")) || !bytes.Contains(received[0], []byte("\"messages\" : [{\"content\":\"unchanged prefix\"}]")) {
		t.Fatalf("unmatched JSON bytes were rewritten: %s", received[0])
	}
	if !bytes.Contains(received[0], []byte(`"prompt_cache_key":"stable-session"`)) || !bytes.Contains(received[0], []byte(`"cache_control":{"type":"ephemeral"}`)) {
		t.Fatalf("provider context-cache controls were rewritten: %s", received[0])
	}
	if len(receivedHeaders) != 2 || receivedHeaders[0].Get("OpenAI-Beta") != "prompt-caching-2024-07-31" || receivedHeaders[0].Get("Anthropic-Beta") != "prompt-caching-2024-07-31" {
		t.Fatalf("provider cache headers were not preserved: %#v", receivedHeaders)
	}
}

func TestNativeProxyKeepsEntityIdentityDistinctAcrossSensitiveValues(t *testing.T) {
	var mu sync.Mutex
	var received [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		received = append(received, append([]byte(nil), body...))
		mu.Unlock()
		start := bytes.Index(body, []byte("{{JSON_"))
		if start < 0 {
			http.Error(w, "missing placeholder", http.StatusBadGateway)
			return
		}
		end := bytes.Index(body[start:], []byte("}}"))
		if end < 0 {
			http.Error(w, "malformed placeholder", http.StatusBadGateway)
			return
		}
		token := body[start : start+end+2]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":"` + string(token) + `"}`))
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"field_paths":["user.email"]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	values := []string{"alice@example.com", "bob@example.com"}
	var requests sync.WaitGroup
	for _, value := range values {
		requests.Add(1)
		go func(value string) {
			defer requests.Done()
			body := []byte(`{"model":"test","system":"stable prefix","user":{"email":"` + value + `"}}`)
			resp, err := gateway.Client().Post(gateway.URL+"/api/requests/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Errorf("value=%s request: %v", value, err)
				return
			}
			responseBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK {
				t.Errorf("value=%s status=%d read=%v body=%s", value, resp.StatusCode, readErr, responseBody)
				return
			}
			if !bytes.Contains(responseBody, []byte(value)) || bytes.Contains(responseBody, []byte("{{JSON_")) {
				t.Errorf("response was not restored for %s: %s", value, responseBody)
			}
		}(value)
	}
	requests.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || bytes.Equal(received[0], received[1]) {
		t.Fatal("different sensitive values shared an entity identity")
	}
	if bytes.Contains(received[0], []byte("alice@example.com")) || bytes.Contains(received[0], []byte("bob@example.com")) {
		t.Fatalf("upstream received plaintext sensitive value: %s", received[0])
	}
}

func TestNativeProxyKeepsEntityIdentityDistinctAcrossTextRuleValues(t *testing.T) {
	var mu sync.Mutex
	var received [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		received = append(received, append([]byte(nil), body...))
		mu.Unlock()
		start := bytes.Index(body, []byte("{{ACCOUNT_"))
		if start < 0 {
			http.Error(w, "missing placeholder", http.StatusBadGateway)
			return
		}
		end := bytes.Index(body[start:], []byte("}}"))
		if end < 0 {
			http.Error(w, "malformed placeholder", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":"` + string(body[start:start+end+2]) + `"}`))
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{
		Engine:       "cpp",
		EngineConfig: `{"rules":[{"pattern":"account-[0-9]{6}","label":"ACCOUNT"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	values := []string{"account-123456", "account-987654"}
	for _, value := range values {
		body := []byte(`{"model":"test","system":"stable prefix","messages":[{"role":"user","content":"` + value + `"}]}`)
		resp, err := gateway.Client().Post(gateway.URL+"/api/requests/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("value=%s status=%d read=%v body=%s", value, resp.StatusCode, readErr, responseBody)
		}
		if !bytes.Contains(responseBody, []byte(value)) || bytes.Contains(responseBody, []byte("{{ACCOUNT_")) {
			t.Errorf("response was not restored for %s: %s", value, responseBody)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || bytes.Equal(received[0], received[1]) {
		t.Fatal("different text-rule values shared an entity identity")
	}
	if bytes.Contains(received[0], []byte("account-123456")) || bytes.Contains(received[0], []byte("account-987654")) {
		t.Fatalf("upstream received plaintext sensitive value: %s", received[0])
	}
}

func TestNativeProxyKeepsContextCachePrefixStableAcrossTurns(t *testing.T) {
	type capturedRequest struct {
		body    []byte
		headers http.Header
	}
	var mu sync.Mutex
	var received []capturedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		received = append(received, capturedRequest{body: append([]byte(nil), body...), headers: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	const contextPrefix = `{"model":"test","prompt_cache_key":"stable-session","system":[{"type":"text","text":"Stable instructions for alice@example.com","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"My account is alice@example.com"}`
	turns := []string{
		contextPrefix + `]}`,
		contextPrefix + `,{"role":"assistant","content":"Understood."},{"role":"user","content":"Also contact bob@example.com"}]}`,
	}
	for i, body := range turns {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/api/requests/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("OpenAI-Beta", "prompt-caching-2024-07-31")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %d status=%d", i, resp.StatusCode)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("upstream received %d requests", len(received))
	}
	for i, request := range received {
		if bytes.Contains(request.body, []byte("alice@example.com")) || bytes.Contains(request.body, []byte("bob@example.com")) {
			t.Errorf("turn %d leaked a sensitive value upstream: %s", i, request.body)
		}
		if !bytes.Contains(request.body, []byte(`"prompt_cache_key":"stable-session"`)) || !bytes.Contains(request.body, []byte(`"cache_control":{"type":"ephemeral"}`)) {
			t.Errorf("turn %d lost provider cache controls: %s", i, request.body)
		}
		if request.headers.Get("OpenAI-Beta") != "prompt-caching-2024-07-31" {
			t.Errorf("turn %d lost cache header: %#v", i, request.headers)
		}
	}
	appendedTurn := bytes.Index(received[1].body, []byte(`,{"role":"assistant"`))
	if appendedTurn < 0 {
		t.Fatalf("second request is missing the appended turn: %s", received[1].body)
	}
	firstTurnEnd := len(received[0].body) - 2
	if firstTurnEnd < 0 || !bytes.Equal(received[0].body[:firstTurnEnd], received[1].body[:appendedTurn]) {
		t.Fatalf("masking changed the prior context prefix across turns:\nfirst:  %s\nsecond: %s", received[0].body, received[1].body)
	}
}

func TestNativeProxyCacheStablePrefixFeedsUpstreamUsageStats(t *testing.T) {
	var mu sync.Mutex
	var firstPrefix []byte
	var received [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		received = append(received, append([]byte(nil), body...))
		cached := int64(0)
		if marker := bytes.Index(body, []byte(`,{"role":"assistant"`)); marker >= 0 {
			if bytes.Equal(firstPrefix, body[:marker]) {
				cached = 80
			}
		} else {
			firstPrefix = append(firstPrefix[:0], body[:len(body)-2]...)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":"gpt-4o","usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens":4}}`, cached)
	}))
	defer upstream.Close()

	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "cache-usage.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	server.SetStorage(store)
	defer server.Shutdown(context.Background())
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	turns := []string{
		`{"model":"gpt-4o","prompt_cache_key":"stable-session","messages":[{"role":"user","content":"Contact alice@example.com"}]}`,
		`{"model":"gpt-4o","prompt_cache_key":"stable-session","messages":[{"role":"user","content":"Contact alice@example.com"},{"role":"assistant","content":"Understood."},{"role":"user","content":"Continue."}]}`,
	}
	for _, body := range turns {
		resp, requestErr := gateway.Client().Post(gateway.URL+"/api/requests/v1/chat/completions", "application/json", strings.NewReader(body))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status=%d", resp.StatusCode)
		}
	}

	var items []storage.Request
	for attempt := 0; attempt < 100; attempt++ {
		items, err = store.ListRequests(context.Background(), 10)
		if err == nil && len(items) == 2 && items[0].EndedAt.After(items[0].StartedAt) && items[1].EndedAt.After(items[1].StartedAt) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(items) != 2 {
		t.Fatalf("request ledger items=%d", len(items))
	}
	mu.Lock()
	forwarded := append([][]byte(nil), received...)
	mu.Unlock()
	if len(forwarded) != 2 {
		t.Fatalf("upstream received %d requests", len(forwarded))
	}
	if bytes.Contains(forwarded[0], []byte("alice@example.com")) || bytes.Contains(forwarded[1], []byte("bob@example.com")) {
		t.Fatalf("upstream received cleartext sensitive data: %q / %q", forwarded[0], forwarded[1])
	}
	secondMarker := bytes.Index(forwarded[1], []byte(`,{"role":"assistant"`))
	if secondMarker < 0 || !bytes.Equal(forwarded[0][:len(forwarded[0])-2], forwarded[1][:secondMarker]) {
		t.Fatalf("masked cache prefix changed: first=%s second=%s", forwarded[0], forwarded[1])
	}
	foundCache := false
	for _, item := range items {
		usage := usageFromMeta(item.Meta)
		if usage.CacheFound && usage.CacheRead == 80 && usage.CacheRateFound {
			foundCache = true
			break
		}
	}
	if !foundCache {
		t.Fatalf("simulated upstream cache usage was not recorded: items=%+v", items)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/stats/today", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", response.Code, response.Body.String())
	}
	var stats struct {
		Cache struct {
			Read        int64    `json:"read_tokens"`
			Observed    int64    `json:"observed_requests"`
			RateSamples int64    `json:"rate_observed_requests"`
			HitRate     *float64 `json:"hit_rate"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Cache.Read != 80 || stats.Cache.Observed != 2 || stats.Cache.RateSamples != 2 || stats.Cache.HitRate == nil || *stats.Cache.HitRate != 0.4 {
		t.Fatalf("cache stats=%+v body=%s", stats.Cache, response.Body.String())
	}
}

func TestNativeAnthropicProxyPreservesContextCachePrefix(t *testing.T) {
	type capturedRequest struct {
		body    []byte
		headers http.Header
	}
	var mu sync.Mutex
	var received []capturedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		received = append(received, capturedRequest{body: append([]byte(nil), body...), headers: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[],"usage":{"input_tokens":1}}`))
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	const anthropicPrefix = `{"model":"claude-test","max_tokens":32,"system":[{"type":"text","text":"Stable system context for alice@example.com","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"My account is alice@example.com"}`
	turns := []string{
		anthropicPrefix + `]}`,
		anthropicPrefix + `,{"role":"assistant","content":"Understood."},{"role":"user","content":"Also contact bob@example.com"}]}`,
	}
	for i, body := range turns {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/api/requests/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %d status=%d", i, resp.StatusCode)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("upstream received %d requests", len(received))
	}
	for i, request := range received {
		if bytes.Contains(request.body, []byte("alice@example.com")) || bytes.Contains(request.body, []byte("bob@example.com")) {
			t.Errorf("turn %d leaked a sensitive value upstream: %s", i, request.body)
		}
		if !bytes.Contains(request.body, []byte(`"cache_control":{"type":"ephemeral"}`)) {
			t.Errorf("turn %d lost Anthropic cache_control: %s", i, request.body)
		}
		if request.headers.Get("Anthropic-Version") != "2023-06-01" || request.headers.Get("Anthropic-Beta") != "prompt-caching-2024-07-31" {
			t.Errorf("turn %d lost Anthropic cache headers: %#v", i, request.headers)
		}
	}
	appendedTurn := bytes.Index(received[1].body, []byte(`,{"role":"assistant"`))
	if appendedTurn < 0 {
		t.Fatalf("second request is missing the appended turn: %s", received[1].body)
	}
	if !bytes.Equal(received[0].body[:len(received[0].body)-2], received[1].body[:appendedTurn]) {
		t.Fatalf("Anthropic masked context prefix changed across turns:\nfirst:  %s\nsecond: %s", received[0].body, received[1].body)
	}
}

func TestNativeProxyPreservesContextCacheInputsForResponsesGeminiAndOllama(t *testing.T) {
	type providerCase struct {
		name         string
		path         string
		header       string
		headerValue  string
		firstBody    string
		secondBody   string
		appendMarker string
		cacheFields  []string
		query        string
	}
	cases := []providerCase{
		{
			name:         "responses",
			path:         "/api/requests/v1/responses",
			header:       "OpenAI-Beta",
			headerValue:  "prompt-caching-2024-07-31",
			firstBody:    `{"model":"gpt-4.1","prompt_cache_key":"stable-session","input":[{"role":"user","content":"Contact alice@example.com"}]}`,
			secondBody:   `{"model":"gpt-4.1","prompt_cache_key":"stable-session","input":[{"role":"user","content":"Contact alice@example.com"},{"role":"assistant","content":"Understood."},{"role":"user","content":"Contact bob@example.com"}]}`,
			appendMarker: `,{"role":"assistant"`,
			cacheFields:  []string{`"prompt_cache_key":"stable-session"`},
		},
		{
			name:         "gemini",
			path:         "/api/requests/v1beta/models/gemini-2.5-pro:generateContent?key=gemini-cache-key",
			header:       "x-goog-api-key",
			headerValue:  "gemini-cache-key",
			firstBody:    `{"cachedContent":"cachedContents/stable","systemInstruction":{"parts":[{"text":"Instructions for alice@example.com"}]},"contents":[{"role":"user","parts":[{"text":"Contact alice@example.com"}]}]}`,
			secondBody:   `{"cachedContent":"cachedContents/stable","systemInstruction":{"parts":[{"text":"Instructions for alice@example.com"}]},"contents":[{"role":"user","parts":[{"text":"Contact alice@example.com"}]},{"role":"model","parts":[{"text":"Understood."}]},{"role":"user","parts":[{"text":"Contact bob@example.com"}]}]}`,
			appendMarker: `,{"role":"model"`,
			cacheFields:  []string{`"cachedContent":"cachedContents/stable"`},
			query:        "key=gemini-cache-key",
		},
		{
			name:         "ollama",
			path:         "/api/requests/api/chat",
			firstBody:    `{"model":"llama3.2","stream":false,"messages":[{"role":"system","content":"Instructions for alice@example.com"},{"role":"user","content":"Contact alice@example.com"}]}`,
			secondBody:   `{"model":"llama3.2","stream":false,"messages":[{"role":"system","content":"Instructions for alice@example.com"},{"role":"user","content":"Contact alice@example.com"},{"role":"assistant","content":"Understood."},{"role":"user","content":"Contact bob@example.com"}]}`,
			appendMarker: `,{"role":"assistant"`,
			cacheFields:  []string{`"model":"llama3.2"`, `"stream":false`},
		},
	}
	type capturedRequest struct {
		path    string
		query   string
		body    []byte
		headers http.Header
	}
	var mu sync.Mutex
	var received []capturedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		received = append(received, capturedRequest{path: r.URL.Path, query: r.URL.RawQuery, body: append([]byte(nil), body...), headers: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			start := len(received)
			mu.Unlock()
			for _, body := range []string{test.firstBody, test.secondBody} {
				req, err := http.NewRequest(http.MethodPost, gateway.URL+test.path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if test.header != "" {
					req.Header.Set(test.header, test.headerValue)
				}
				resp, err := gateway.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d", resp.StatusCode)
				}
			}

			mu.Lock()
			requests := append([]capturedRequest(nil), received[start:]...)
			mu.Unlock()
			if len(requests) != 2 {
				t.Fatalf("upstream received %d requests, want 2", len(requests))
			}
			for i, request := range requests {
				if bytes.Contains(request.body, []byte("alice@example.com")) || bytes.Contains(request.body, []byte("bob@example.com")) {
					t.Errorf("request %d leaked a sensitive value: %s", i, request.body)
				}
				for _, field := range test.cacheFields {
					if !bytes.Contains(request.body, []byte(field)) {
						t.Errorf("request %d lost cache field %s: %s", i, field, request.body)
					}
				}
				if test.header != "" && request.headers.Get(test.header) != test.headerValue {
					t.Errorf("request %d lost cache header %s: %#v", i, test.header, request.headers)
				}
				if test.query != "" && request.query != test.query {
					t.Errorf("request %d query=%q want=%q", i, request.query, test.query)
				}
			}
			appendAt := bytes.Index(requests[1].body, []byte(test.appendMarker))
			if appendAt < 0 {
				t.Fatalf("second request is missing appended turn %q: %s", test.appendMarker, requests[1].body)
			}
			prefixEnd := bytes.LastIndex(requests[0].body, []byte("]}"))
			if prefixEnd < 0 || !bytes.Equal(requests[0].body[:prefixEnd], requests[1].body[:appendAt]) {
				t.Fatalf("masked context prefix changed across turns:\nfirst:  %s\nsecond: %s", requests[0].body, requests[1].body)
			}
		})
	}
}

func TestNativeControlSensitiveRulesAreApplied(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{Engine: "cpp", MaxConcurrency: 1}, engine)
	if err != nil {
		engine.(CloseableEngine).Close()
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	server.configMu.Lock()
	server.control["sensitive"] = map[string]any{"CUSTOM": []any{"alpha"}}
	err = server.rebuildEngineLocked()
	server.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	masked, err := server.currentEngine().(MaskingEngine).Mask(context.Background(), []byte("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(masked), "alpha") || !strings.Contains(string(masked), "{{CUSTOM_") {
		t.Fatalf("custom rule was not applied: %q", masked)
	}
}

func TestNativeDefaultBuiltinRuleSetLoads(t *testing.T) {
	raw, err := engineConfigFromControl(defaultControlConfig())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: raw})
	if err != nil {
		t.Fatalf("default enabled built-in rules did not load: %v", err)
	}
	defer engine.(CloseableEngine).Close()

	masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(masked), "alice@example.com") {
		t.Fatalf("default EMAIL protection was not applied: %s", masked)
	}
}

func TestNativeExplicitlyDisabledBuiltinRulesDoNotFallBackToDefaults(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"sensitive":     map[string]any{},
		"builtin_rules": map[string]any{"EMAIL": false, "TOKEN": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: raw})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()

	const email = "alice@example.com"
	masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte(email))
	if err != nil {
		t.Fatal(err)
	}
	if string(masked) != email {
		t.Fatalf("explicitly disabled rules fell back to defaults: %s", masked)
	}
}

func TestNativeExpandedBuiltinPatternsCompileAndMask(t *testing.T) {
	testCases := []struct {
		label   string
		pattern string
		value   string
	}{
		{"API_KEY", `(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{20,}`, "gho_12345678901234567890"},
		{"API_KEY", `github_pat_[A-Za-z0-9_]{50,}`, "github_pat_12345678901234567890123456789012345678901234567890"},
		{"API_KEY", `AIza[0-9A-Za-z_-]{35,38}`, "AIza12345678901234567890123456789012345"},
		{"API_KEY", `xox[baprs]-[0-9A-Za-z-]{10,}`, "xoxb-1234567890"},
		{"API_KEY", `[sr]k_(live|test)_[0-9A-Za-z]{20,}`, "sk_live_12345678901234567890"},
		{"API_KEY", `cli_[a-z0-9]{16,}`, "cli_abcdefghijklmnop"},
		{"API_KEY", `ding[a-z0-9]{6,}`, "dingabcdef"},
		{"CARD", `[3-6][0-9]{12,18}`, "4111111111111"},
		{"CONNSTR", `[a-z][a-z0-9+.-]{0,63}://[^\s:@/]+:[^\s@/]{4,}@`, "postgres://user:password@"},
		{"EMAIL", `([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`, "alice@example.com"},
		{"IDCARD", `((1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{13}|(1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{15}[0-9Xx])`, "110101199001011234"},
		{"LANDLINE", `0(10|2[0-9]|[3-9][0-9]{2})[- ]?[2-9][0-9]{6,7}`, "010-23456789"},
		{"PHONE", `1[3-9][0-9]{9}`, "13812345678"},
		{"PHONE", `\+861[3-9][0-9]{9}`, "+8613812345678"},
		{"PHONE", `00861[3-9][0-9]{9}`, "008613812345678"},
		{"PHONE", `1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4}`, "138 1234 5678"},
		{"PHONE", `(\+86|0086)[ -]?1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4}`, "+86 138-1234-5678"},
		{"BEARER", `[Bb][Ee][Aa][Rr][Ee][Rr]\s+[A-Za-z0-9._~+/-]{20,}=*`, "bearer ABCDEFGHIJKLMNOPQRST"},
		{"ACCESS_KEY", `((aws_secret_access_key|AWS_SECRET_ACCESS_KEY)["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{40})`, "aws_secret_access_key=abcdefghijklmnopqrstuvwxyz1234567890ABCD"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.label+"/"+testCase.pattern, func(t *testing.T) {
			config := map[string]any{"rules": []any{
				map[string]any{"pattern": testCase.pattern, "label": testCase.label},
			}}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: string(raw)})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.(CloseableEngine).Close()
			masked, err := engine.(MaskingEngine).Mask(context.Background(), []byte(testCase.value))
			if err != nil {
				t.Fatal(err)
			}
			if string(masked) == testCase.value || !strings.HasPrefix(string(masked), "{{"+testCase.label+"_") {
				t.Fatalf("expanded built-in pattern did not mask %q: %s", testCase.value, masked)
			}
		})
	}
}

func TestNativeLegacySemanticRulesRejectInvalidCandidates(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"sensitive": map[string]any{},
		"builtin_rules": map[string]any{
			"EMAIL": true, "PHONE": true, "IBAN": true, "JWT": true,
			"IPV6_PRIVATE": true, "PLATE": true, "USCC": true, "SECRET": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: raw})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	cases := []struct {
		label string
		good  string
		bad   string
	}{
		{"EMAIL", "张三@qq.com", "a@b.c"},
		{"PHONE", "(86)13800138000", "12345678901"},
		{"IBAN", "GB82WEST12345698765432", "GB00WEST12345698765432"},
		{"JWT", "eyJhbGciOiJIUzI1NiJ9.abcdefgh.abcdefgh", "eyJub3RfanNvbiI6MX0.abcdefgh.abcdefgh"},
		{"IPV6_PRIVATE", "fe80::1", "2001:db8::1"},
		{"PLATE", "京A12345", "新README"},
		{"USCC", "91100000100003962T", "91100000100003962A"},
		{"SECRET", "password=Hn8x!qW2zLm9pR", "mypassword=Hn8x!qW2zLm9pR"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			good, err := engine.(MaskingEngine).Mask(context.Background(), []byte(tc.good))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(good), tc.good) {
				t.Fatalf("valid %s candidate was not masked: %q", tc.label, good)
			}
			bad, err := engine.(MaskingEngine).Mask(context.Background(), []byte(tc.bad))
			if err != nil {
				t.Fatal(err)
			}
			if string(bad) != tc.bad {
				t.Fatalf("invalid %s candidate was masked: %q", tc.label, bad)
			}
		})
	}
}
