//go:build silencer_engine && cgo

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNativeMultipleIPCopyAndFabricatedSuffixEvidence(t *testing.T) {
	var values []string
	var rules []map[string]string
	for i := 1; i <= 10; i++ {
		value := fmt.Sprintf("203.0.113.%d", i)
		values = append(values, value)
		rules = append(rules, map[string]string{"needle": value, "label": "IP_PUBLIC"})
	}
	config, _ := json.Marshal(map[string]any{"rules": rules})
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: string(config)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	mask, err := engine.(StreamingMaskingEngine).NewMaskingSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := mask.(ContextualMaskingSession)
	defer owner.Release()
	raw, _ := json.Marshal(values)
	a, err := mask.Push(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mask.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	if err := json.Unmarshal(append(a, b...), &tokens); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 10 {
		t.Fatal("missing tokens")
	}
	// A model should copy opaque identifiers, not see a numerical series.
	for i := 1; i < len(tokens); i++ {
		if tokens[i][12:20] == tokens[i-1][12:20] {
			t.Fatal("adjacent tokens still share a sequential-looking suffix")
		}
	}
	items, err := mask.(MaskedItemsProvider).MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, text                  string
		total, restored, unresolved int
	}{
		{"ten IPs", strings.Join(tokens, " "), 10, 10, 0},
		{"one copied, nine fabricated", tokens[0] + " " + strings.Repeat("{{IP_PUBLIC_ffffffffffffffff}} ", 9), 10, 1, 9},
		{"template and reasoning aliases", tokens[0] + " {{IP_PUBLIC_...}} {{IP_PUBLIC_TEST}} {{LABEL_TEMPLATE}} {{TERM_token}} {{TERM_example}} {{TERM_placeholder}}", 2, 2, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newProtocolRestoreSession("sse", owner.NewRestoreSession)
			defer s.Abort()
			var output []byte
			fields := []string{"content"}
			if scenario.name == "template and reasoning aliases" {
				fields = []string{"reasoning", "reasoning_content"}
			}
			// Split every token across distinct protocol events and transport reads.
			for _, char := range scenario.text {
				delta := map[string]any{}
				for _, field := range fields {
					delta[field] = string(char)
				}
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
				wire := append(append([]byte("data: "), frame...), '\n', '\n')
				for _, chunk := range [][]byte{wire[:7], wire[7:]} {
					out, err := s.Push(context.Background(), chunk)
					if err != nil {
						t.Fatal(err)
					}
					output = append(output, out...)
				}
			}
			out, err := s.Push(context.Background(), []byte("data: [DONE]\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, out...)
			out, err = s.Finish(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, out...)
			c := s.RestoreCounts()
			if c.Total != scenario.total || c.Restored != scenario.restored || c.Unresolved != scenario.unresolved || !c.Complete {
				t.Fatalf("counts=%+v", c)
			}
			// Compare every value in each logical response field, including order.
			// Counts alone would also accept a token silently dropped or replaced
			// with an unrelated IP.
			expected := scenario.text
			for i, token := range tokens {
				expected = strings.ReplaceAll(expected, token, values[i])
			}
			texts := map[string]string{}
			for _, line := range bytes.Split(output, []byte("\n")) {
				if !bytes.HasPrefix(line, []byte("data:")) {
					continue
				}
				payload := bytes.TrimSpace(line[len("data:"):])
				if bytes.Equal(payload, []byte("[DONE]")) {
					continue
				}
				var frame struct {
					Choices []struct {
						Delta map[string]string `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal(payload, &frame); err != nil {
					t.Fatal(err)
				}
				for _, choice := range frame.Choices {
					for _, field := range fields {
						texts[field] += choice.Delta[field]
					}
				}
			}
			for _, field := range fields {
				if texts[field] != expected {
					t.Fatalf("%s text=%q want=%q", field, texts[field], expected)
				}
			}
			metaBytes, _ := json.Marshal(map[string]any{"items": items, "restore_evidence": c.Tokens, "restore_evidence_complete": c.Complete})
			var meta map[string]any
			_ = json.Unmarshal(metaBytes, &meta)
			detail := restoreDetailItems(meta["items"], meta)
			restored := 0
			for _, item := range detail {
				if item["restored"] == true {
					restored++
				}
			}
			want := 1
			if scenario.name == "ten IPs" {
				want = 10
			}
			if restored != want {
				t.Fatalf("marked restored=%d want=%d", restored, want)
			}
		})
	}
}
