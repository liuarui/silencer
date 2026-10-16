//go:build silencer_engine && cgo

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silencer/gateway/internal/storage"
)

func TestDurableMappingMultiTurnRestartAndResponseOnlyTools(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "conversation.db"))
	t.Setenv("SILENCER_LOG_DIR", t.TempDir())
	const original = "198.51.100.8"
	var token string
	var previousPrefix []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"reply": token})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(original)) {
			t.Error("raw IP sent upstream")
		}
		var request struct {
			Mode     string           `json:"mode"`
			Messages []map[string]any `json:"messages"`
		}
		if json.Unmarshal(body, &request) != nil || len(request.Messages) == 0 || request.Messages[0]["content"] != "Hermes instructions" {
			t.Error("system message changed")
		}
		if request.Mode == "first" {
			token = request.Messages[1]["content"].(string)
			previousPrefix = append([]byte(nil), body[:bytes.Index(body, []byte(`],"mode"`))]...)
		}
		if request.Mode == "append" && !bytes.HasPrefix(body, previousPrefix) {
			t.Error("multi-turn masked prefix changed")
		}
		if request.Mode == "tool" {
			w.Header().Set("Content-Type", "text/event-stream")
			arguments, _ := json.Marshal(map[string]string{"host": token})
			for _, c := range string(arguments) {
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": string(c)}}}}}}})
				fmt.Fprintf(w, "data: %s\n\n", frame)
			}
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": token}}}})
	}))
	defer up.Close()
	var engine Engine
	var server *Server
	var store *storage.Storage
	start := func() {
		var err error
		store, err = storage.OpenFromEnv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		engine, err = newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"198.51.100.8","label":"IP_PUBLIC"}]}`})
		if err != nil {
			t.Fatal(err)
		}
		server, err = NewServer(Config{Engine: "cpp", UpstreamURL: up.URL}, engine)
		if err != nil {
			t.Fatal(err)
		}
		server.SetStorage(store)
	}
	stop := func() {
		if server != nil {
			server.Shutdown(ctx)
			store.Close()
			server = nil
		}
	}
	defer stop()
	start()
	call := func(body string, method string) string {
		req := httptest.NewRequest(method, "/api/requests/v1/chat/completions", strings.NewReader(body))
		req.Host = "conversation.example"
		req.Header.Set("Authorization", "Bearer test-provider")
		req.Header.Set("Content-Type", "application/json")
		if method == "GET" {
			req.Body = http.NoBody
		}
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status=%d: %s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	first := `{"messages":[{"role":"system","content":"Hermes instructions"},{"role":"user","content":"198.51.100.8"}],"mode":"first"}`
	if !strings.Contains(call(first, "POST"), original) {
		t.Fatal("first reply not restored")
	}
	if err := store.DeleteEvents(ctx); err != nil {
		t.Fatal(err)
	}
	engine.(ReusableMaskingEngine).ClearReusableMappings()
	appended := `{"messages":[{"role":"system","content":"Hermes instructions"},{"role":"user","content":"198.51.100.8"},{"role":"assistant","content":"ok"},{"role":"user","content":"continue"}],"mode":"append"}`
	if !strings.Contains(call(appended, "POST"), original) {
		t.Fatal("multi-turn reply not restored")
	}
	stop()
	start()
	engine.(ReusableMaskingEngine).ClearReusableMappings()
	// Neither original nor token occurs in this request; only provider history
	// references it. Restoration must query the durable reverse index on demand.
	later := `{"messages":[{"role":"system","content":"Hermes instructions"},{"role":"user","content":"continue"}],"mode":"later"}`
	if !strings.Contains(call("", "GET"), original) {
		t.Fatal("bodyless historical reference not restored")
	}
	if !strings.Contains(call(later, "POST"), original) {
		t.Fatal("response-only historical mapping lost on restart")
	}
	wire := call(strings.Replace(later, `"later"`, `"tool"`, 1), "POST")
	var arguments strings.Builder
	for _, line := range strings.Split(wire, "\n") {
		if strings.HasPrefix(line, "data: {") {
			var event map[string]any
			json.Unmarshal([]byte(line[6:]), &event)
			choices, _ := event["choices"].([]any)
			for _, choice := range choices {
				delta := choice.(map[string]any)["delta"].(map[string]any)
				tools, _ := delta["tool_calls"].([]any)
				for _, tool := range tools {
					fn, _ := tool.(map[string]any)["function"].(map[string]any)
					value, _ := fn["arguments"].(string)
					arguments.WriteString(value)
				}
			}
		}
	}
	var args map[string]string
	if json.Unmarshal([]byte(arguments.String()), &args) != nil || args["host"] != original {
		t.Fatal("historical tool arguments not restored")
	}
}

// A failed authority must never make the engine emit unprotected text.
type failingMappingAuthority struct{ failIssue, failResolve, panicIssue bool }

func (f failingMappingAuthority) IssueEntityMapping(context.Context, string, string, string) (storage.EntityMapping, error) {
	if f.panicIssue {
		panic("injected authority panic")
	}
	if f.failIssue {
		return storage.EntityMapping{}, errors.New("injected authority failure")
	}
	return storage.EntityMapping{}, errors.New("unexpected issuance")
}
func (f failingMappingAuthority) ResolveEntityMapping(context.Context, string, string) (storage.EntityMapping, bool, error) {
	if f.failResolve {
		return storage.EntityMapping{}, false, errors.New("injected authority failure")
	}
	return storage.EntityMapping{}, false, nil
}
func TestDurableMappingAuthorityFailureAndSharedChannelLifetime(t *testing.T) {
	ctx := context.Background()
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"198.51.100.8","label":"IP_PUBLIC"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	durable := engine.(DurableMaskingEngine)
	scope := strings.Repeat("b", 64)
	for _, authority := range []failingMappingAuthority{{failIssue: true}, {panicIssue: true}} {
		owner, err := durable.NewDurableMaskingSession(ctx, scope, authority)
		if err != nil {
			t.Fatal(err)
		}
		out, pushErr := owner.Push(ctx, []byte("198.51.100.8 "))
		if pushErr == nil {
			tail, finishErr := owner.Finish(ctx)
			out = append(out, tail...)
			pushErr = finishErr
		}
		owner.Release()
		if pushErr == nil || bytes.Contains(out, []byte("198.51.100.8")) {
			t.Fatal("authority failure exposed original")
		}
	}
	owner, err := durable.NewDurableMaskingSession(ctx, scope, failingMappingAuthority{failResolve: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	channel, err := owner.NewRestoreSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner.Release()
	out, err := channel.Push(ctx, []byte("{{IP_PUBLIC_1234567890abcdef}} "))
	if err == nil {
		tail, finishErr := channel.Finish(ctx)
		out = append(out, tail...)
		err = finishErr
	}
	if err == nil || bytes.Contains(out, []byte("{{IP_PUBLIC_")) {
		t.Fatal("reverse authority failure exposed placeholder")
	}

	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "channels.db"))
	store, err := storage.OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mapping, err := store.IssueEntityMapping(ctx, scope, "198.51.100.8", "IP_PUBLIC")
	if err != nil {
		t.Fatal(err)
	}
	owner, err = durable.NewDurableMaskingSession(ctx, scope, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	channels := make([]RestoreSession, 3)
	for i := range channels {
		channels[i], err = owner.NewRestoreSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	// SQL callback handle must remain alive even after the mask owner is gone.
	owner.Release()
	for i, channel := range channels {
		token := mapping.Token
		if i == 1 {
			token = strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}")
		}
		if i == 2 {
			token = strings.ReplaceAll(token, "IP_PUBLIC", "IPPUBLIC")
		}
		var got []byte
		for _, c := range []byte(token) {
			part, err := channel.Push(ctx, []byte{c})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, part...)
		}
		tail, err := channel.Finish(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, tail...)
		if string(got) != "198.51.100.8" {
			t.Fatalf("historical split channel %d not restored", i)
		}
	}
}

func TestDurableMappingLongConversationPreservesEveryTurn(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "long.db"))
	store, err := storage.OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"198.51.100.8","label":"IP_PUBLIC"},{"needle":"long@example.com","label":"EMAIL"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	scope := strings.Repeat("c", 64)
	messages := []map[string]string{{"role": "system", "content": "Hermes instructions stay unchanged"}}
	var previous, firstToken string
	for turn := 0; turn < 80; turn++ {
		messages = append(messages, map[string]string{"role": "user", "content": strings.Repeat("history ", 2048) + "198.51.100.8 long@example.com"})
		input, _ := json.Marshal(map[string]any{"messages": messages})
		owner, err := engine.(DurableMaskingEngine).NewDurableMaskingSession(ctx, scope, store)
		if err != nil {
			t.Fatal(err)
		}
		var masked []byte
		for pos := 0; pos < len(input); pos += 4093 {
			end := pos + 4093
			if end > len(input) {
				end = len(input)
			}
			part, err := owner.Push(ctx, input[pos:end])
			if err != nil {
				t.Fatal(err)
			}
			masked = append(masked, part...)
		}
		tail, err := owner.Finish(ctx)
		if err != nil {
			t.Fatal(err)
		}
		masked = append(masked, tail...)
		if bytes.Contains(masked, []byte("198.51.100.8")) || bytes.Contains(masked, []byte("long@example.com")) {
			t.Fatal("long history leaked original")
		}
		prefix := string(masked[:len(masked)-2])
		if previous != "" && !strings.HasPrefix(prefix, previous) {
			t.Fatal("append-only history prefix changed")
		}
		previous = prefix
		var envelope struct {
			Messages []map[string]string `json:"messages"`
		}
		if json.Unmarshal(masked, &envelope) != nil {
			t.Fatal("masked long conversation invalid JSON")
		}
		value := envelope.Messages[1]["content"]
		tokenStart := strings.Index(value, "{{")
		token := value[tokenStart:]
		if firstToken == "" {
			firstToken = token
		}
		if token != firstToken {
			t.Fatal("historical identity changed")
		}
		restore, err := owner.NewRestoreSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out, err := restore.Push(ctx, []byte(token))
		if err != nil {
			t.Fatal(err)
		}
		tail, err = restore.Finish(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tail...)
		owner.Release()
		if string(out) != "198.51.100.8 long@example.com" {
			t.Fatal("long conversation reply not restored")
		}
	}
}
