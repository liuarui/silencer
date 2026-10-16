//go:build silencer_engine && cgo

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func reusableTestMask(t *testing.T, engine Engine, scope, text string) (ContextualMaskingSession, string) {
	t.Helper()
	session, err := engine.(ReusableMaskingEngine).NewReusableMaskingSession(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	a, err := session.Push(context.Background(), []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	b, err := session.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return session.(ContextualMaskingSession), string(append(a, b...))
}

func reusableTestEngine(t *testing.T) Engine {
	t.Helper()
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"203.0.113.42","label":"IP_PUBLIC"},{"needle":"198.51.100.7","label":"IP_PUBLIC"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.(CloseableEngine).Close)
	return engine
}

func TestNativeReusableEntitySurvivesRepeatedValuesAndHistoryWindow(t *testing.T) {
	engine := reusableTestEngine(t)
	scope := t.Name()
	first, masked := reusableTestMask(t, engine, scope, "203.0.113.42 203.0.113.42")
	tokens := strings.Fields(masked)
	if len(tokens) != 2 || tokens[0] != tokens[1] || len(tokens[0]) != 20 {
		t.Fatalf("unstable entity tokens: %q", masked)
	}
	items, err := first.(MaskedItemsProvider).MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || maskedItemOccurrences(items[0]) != 2 {
		t.Fatalf("occurrence evidence=%v", items)
	}
	first.Release()
	second, shifted := reusableTestMask(t, engine, scope, "198.51.100.7 then 203.0.113.42")
	if !strings.HasSuffix(shifted, tokens[0]) || strings.Fields(shifted)[0] == tokens[0] {
		t.Fatalf("identity changed after earlier matches: %q", shifted)
	}
	second.Release()
	for _, kind := range []string{"text", "bare", "tool"} {
		t.Run(kind, func(t *testing.T) {
			// All original IPs have slid out of this new request's input.
			owner, _ := reusableTestMask(t, engine, scope, "continue the previous answer")
			defer owner.Release()
			session := newProtocolRestoreSession("sse", owner.NewRestoreSession)
			defer session.Abort()
			text := tokens[0]
			if kind == "bare" {
				text = strings.TrimSuffix(strings.TrimPrefix(text, "{{"), "}}")
			}
			if kind == "tool" {
				text = `{"host":"` + text + `"}`
			}
			var wire []byte
			for _, char := range text {
				delta := map[string]any{"content": string(char)}
				if kind == "tool" {
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": string(char)}}}}
				}
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
				out, err := session.Push(context.Background(), []byte("data: "+string(frame)+"\n\n"))
				if err != nil {
					t.Fatal(err)
				}
				wire = append(wire, out...)
			}
			out, err := session.Push(context.Background(), []byte("data: [DONE]\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			wire = append(wire, out...)
			out, err = session.Finish(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wire = append(wire, out...)
			var got strings.Builder
			for _, line := range bytes.Split(wire, []byte("\n")) {
				if !bytes.HasPrefix(line, []byte("data: {")) {
					continue
				}
				obj, err := decodeProtocolJSON(bytes.TrimPrefix(line, []byte("data: ")))
				if err != nil {
					t.Fatal(err)
				}
				path := []any{"choices", 0, "delta", "content"}
				if kind == "tool" {
					path = []any{"choices", 0, "delta", "tool_calls", 0, "function", "arguments"}
				}
				value, _ := protocolGet(obj, path).(string)
				got.WriteString(value)
			}
			want := "203.0.113.42"
			if kind == "tool" {
				want = `{"host":"203.0.113.42"}`
			}
			if got.String() != want || (kind != "bare" && session.RestoreCounts().Restored != 1) || session.RestoreCounts().Unresolved != 0 {
				t.Fatalf("%s history restore=%q counts=%+v", kind, got.String(), session.RestoreCounts())
			}
			refreshed, err := owner.(MaskedItemsProvider).MaskedItems()
			if err != nil {
				t.Fatal(err)
			}
			if len(refreshed) != 1 || maskedItemOccurrences(refreshed[0]) != 0 {
				t.Fatalf("historical reference counted as newly masked: %v", refreshed)
			}
		})
	}
}

func TestNativeReusableRejectsExtendedSuffixAndExpiredSeed(t *testing.T) {
	engine := reusableTestEngine(t)
	scope := t.Name()
	owner, token := reusableTestMask(t, engine, scope, "203.0.113.42")
	defer owner.Release()
	for _, unknown := range []string{strings.TrimSuffix(token, "}}") + "x}}", strings.TrimSuffix(token, "}}") + "_more}}"} {
		session, err := owner.NewRestoreSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var result []byte
		for _, char := range unknown {
			out, err := session.Push(context.Background(), []byte(string(char)))
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, out...)
		}
		out, err := session.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, out...)
		if string(result) != unknown {
			t.Fatalf("unknown suffix matched a known prefix: %q", result)
		}
	}
	items, err := owner.(MaskedItemsProvider).MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		item["used_at"] = time.Now().Add(-25 * time.Hour).Unix()
	}
	if err := engine.(ReusableMaskingEngine).ImportReusableMappings(context.Background(), scope+"-expired", items); err != nil {
		t.Fatal(err)
	}
	other, _ := reusableTestMask(t, engine, scope+"-expired", "no source")
	defer other.Release()
	session, err := other.NewRestoreSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, err := session.Push(context.Background(), []byte(token))
	if err != nil {
		t.Fatal(err)
	}
	b, err := session.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(append(a, b...)) != token {
		t.Fatal("expired mapping was imported")
	}
}

func TestNativeReusableWarmupReadsRetainedEntityMetadata(t *testing.T) {
	engine := reusableTestEngine(t)
	scope := strings.Repeat("a", 64)
	owner, token := reusableTestMask(t, engine, scope, "203.0.113.42")
	items, err := owner.(MaskedItemsProvider).MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	owner.Release()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "reuse.db"))
	t.Setenv("SILENCER_LOG_DIR", t.TempDir())
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta, _ := json.Marshal(map[string]any{"mapping_mode": "entity-v1", "mapping_scope": scope, "items": items, "items_complete": true})
	if err := store.AppendEvent(context.Background(), storage.Event{ID: "retained-entity", Type: "MASK", Meta: string(meta), CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// New structural/no-match rows cannot crowd a valid map out before LIMIT.
	for i := 0; i < 1001; i++ {
		if err := store.AppendEvent(context.Background(), storage.Event{ID: fmt.Sprint("empty-", i), Type: "MASK", Meta: `{"items":[]}`, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.RecentMappingMetadata(context.Background(), time.Now().Add(-time.Hour), 1)
	if err != nil || len(rows) != 1 || rows[0].Meta != string(meta) {
		t.Fatalf("entity prefilter failed: rows=%d err=%v", len(rows), err)
	}
	if err := engine.(ReusableMaskingEngine).ClearReusableMappings(); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{Engine: "cpp"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	server.SetStorage(store)
	defer server.Shutdown(context.Background())
	after, masked := reusableTestMask(t, engine, scope, "203.0.113.42")
	defer after.Release()
	if masked != token {
		t.Fatalf("restart warmup changed retained identity: %q -> %q", token, masked)
	}
}

func TestNativeReusableScopeAndWarmup(t *testing.T) {
	engine := reusableTestEngine(t)
	scope := t.Name()
	owner, token := reusableTestMask(t, engine, scope, "203.0.113.42")
	items, err := owner.(MaskedItemsProvider).MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	owner.Release()
	other, _ := reusableTestMask(t, engine, scope+"-other", "no source value")
	defer other.Release()
	s := newProtocolRestoreSession("json", other.NewRestoreSession)
	defer s.Abort()
	s.Push(context.Background(), []byte(fmt.Sprintf(`{"content":%q}`, token)))
	out, err := s.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(token)) || bytes.Contains(out, []byte("203.0.113.42")) {
		t.Fatal("mapping crossed a route/credential scope")
	}
	if err := engine.(ReusableMaskingEngine).ClearReusableMappings(); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		item["used_at"] = time.Now().Unix()
	}
	if err := engine.(ReusableMaskingEngine).ImportReusableMappings(context.Background(), scope, items); err != nil {
		t.Fatal(err)
	}
	restarted, again := reusableTestMask(t, engine, scope, "203.0.113.42")
	defer restarted.Release()
	if again != token {
		t.Fatalf("warmup changed entity identity: %q -> %q", token, again)
	}
	// Unregistered historical structural IDs are never guessed or rebound.
	stream, err := restarted.NewRestoreSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unknown := []byte("{{IP_PUBLIC_ffffffffffffffff}}")
	a, err := stream.Push(context.Background(), unknown)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stream.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(a, b...), unknown) {
		t.Fatal("unknown ID was assigned an unrelated IP")
	}
}
