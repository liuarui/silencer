package gateway

import "testing"

func TestExtractUsageProviderFormats(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		model      string
		prompt     int64
		completion int64
		cacheRead  int64
		cacheWrite int64
		cacheTotal int64
	}{
		{name: "openai", body: `{"model":"gpt-4o","usage":{"prompt_tokens":12,"completion_tokens":7}}`, model: "gpt-4o", prompt: 12, completion: 7},
		{name: "anthropic", body: `{"model":"claude-3-haiku","usage":{"input_tokens":10,"output_tokens":5}}`, model: "claude-3-haiku", prompt: 10, completion: 5},
		{name: "responses nested", body: `{"response":{"model":"gpt-4o","usage":{"input_tokens":4,"output_tokens":8}}}`, model: "gpt-4o", prompt: 4, completion: 8},
		{name: "ollama", body: `{"model":"llama3","prompt_eval_count":11,"eval_count":6}`, model: "llama3", prompt: 11, completion: 6},
		{name: "numeric strings", body: `{"model":"gpt-4o","usage":{"prompt_tokens":"3","completion_tokens":"2"}}`, model: "gpt-4o", prompt: 3, completion: 2},
		{name: "openai cache details", body: `{"model":"gpt-4o","usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":60},"completion_tokens":4}}`, model: "gpt-4o", prompt: 100, completion: 4, cacheRead: 60, cacheTotal: 100},
		{name: "responses cache details", body: `{"response":{"model":"gpt-4o","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80},"output_tokens":12}}}`, model: "gpt-4o", prompt: 100, completion: 12, cacheRead: 80, cacheTotal: 100},
		{name: "anthropic cache", body: `{"model":"claude-3-5-sonnet","usage":{"input_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":10,"output_tokens":5}}`, model: "claude-3-5-sonnet", prompt: 20, completion: 5, cacheRead: 80, cacheWrite: 10, cacheTotal: 110},
		{name: "anthropic cache details", body: `{"model":"claude-3-5-sonnet","usage":{"input_tokens":20,"input_tokens_details":{"cache_read_input_tokens":80,"cache_creation_input_tokens":10},"output_tokens":5}}`, model: "claude-3-5-sonnet", prompt: 20, completion: 5, cacheRead: 80, cacheWrite: 10, cacheTotal: 110},
		{name: "gemini cache metadata", body: `{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":90,"candidatesTokenCount":14,"totalTokenCount":134}}`, model: "gemini-2.5-pro", prompt: 120, completion: 14, cacheRead: 90, cacheTotal: 120},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := extractUsage([]byte(test.body))
			if !got.Found || got.Model != test.model || got.Prompt != test.prompt || got.Completion != test.completion || got.CacheRead != test.cacheRead || got.CacheWrite != test.cacheWrite || (test.cacheRead+test.cacheWrite > 0 && !got.CacheFound) {
				t.Fatalf("usage=%+v", got)
			}
			if test.cacheTotal > 0 && (!got.CacheRateFound || got.CacheTotal != test.cacheTotal) {
				t.Fatalf("cache denominator: usage=%+v want total=%d", got, test.cacheTotal)
			}
		})
	}
}

func TestExtractUsageGeminiStreamingSnapshots(t *testing.T) {
	body := "data: {\"modelVersion\":\"gemini-2.5-pro\",\"usageMetadata\":{\"promptTokenCount\":80,\"cachedContentTokenCount\":60}}\n" +
		"data: {\"modelVersion\":\"gemini-2.5-pro\",\"usageMetadata\":{\"promptTokenCount\":80,\"cachedContentTokenCount\":60,\"candidatesTokenCount\":9}}\n"
	got := extractUsage([]byte(body))
	if got.Prompt != 80 || got.Completion != 9 || got.CacheRead != 60 || !got.CacheFound || !got.CacheRateFound || got.CacheTotal != 80 {
		t.Fatalf("gemini usage=%+v", got)
	}
}

func TestExtractUsageUsesTheSelectedEnvelopeWithoutFabricatingTotals(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","usage":{"prompt_tokens":100},"response":{"usage":{"output_tokens":9}}}`)
	for i := 0; i < 100; i++ {
		got := extractUsage(body)
		if got.Model != "gpt-4o" || got.Prompt != 100 || !got.PromptFound || got.CompletionFound {
			t.Fatalf("iteration %d fabricated usage fields: %+v", i, got)
		}
	}
}

func TestExtractUsageMergesRepeatedSnapshots(t *testing.T) {
	body := "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7}}\n" +
		"data: {\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":9}}\n"
	got := extractUsage([]byte(body))
	if got.Prompt != 120 || got.Completion != 9 || !got.PromptFound || !got.CompletionFound {
		t.Fatalf("usage=%+v", got)
	}
}

func TestExtractUsageMergesAnthropicStreamingPhases(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":80,\"cache_creation_input_tokens\":10}}}\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n"
	got := extractUsage([]byte(body))
	if got.Model != "claude-3-5-sonnet" || got.Prompt != 20 || got.Completion != 5 || got.CacheRead != 80 || got.CacheWrite != 10 || got.CacheTotal != 110 || !got.CacheRateFound {
		t.Fatalf("anthropic usage=%+v", got)
	}
}

func TestExtractUsageCountsExplicitZeroCacheSample(t *testing.T) {
	got := extractUsage([]byte(`{"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0}}}`))
	if !got.Found || !got.CacheFound || !got.CacheRateFound || got.CacheRead != 0 || got.CacheTotal != 100 {
		t.Fatalf("zero-cache usage=%+v", got)
	}
}

func TestExtractUsageRejectsImpossibleCacheRate(t *testing.T) {
	got := extractUsage([]byte(`{"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":101}}}`))
	if !got.CacheFound || got.CacheRead != 101 {
		t.Fatalf("raw cache observation was lost: %+v", got)
	}
	if got.CacheRateFound || got.CacheTotal != 0 {
		t.Fatalf("impossible cache rate was published: %+v", got)
	}
}

func TestExtractUsageCountsFullyCachedAnthropicInput(t *testing.T) {
	got := extractUsage([]byte(`{"usage":{"input_tokens":0,"cache_read_input_tokens":50,"output_tokens":3}}`))
	if !got.CacheFound || !got.CacheRateFound || got.CacheRead != 50 || got.CacheTotal != 50 {
		t.Fatalf("fully cached Anthropic usage=%+v", got)
	}
}

func TestUsageFromMetaRejectsImpossibleCacheRate(t *testing.T) {
	got := usageFromMeta(`{"usage_found":true,"usage_prompt_tokens":100,"usage_cache_observed":true,"usage_cache_read_tokens":101,"usage_cache_rate_observed":true,"usage_cache_input_tokens":100}`)
	if !got.CacheFound || got.CacheRead != 101 {
		t.Fatalf("raw cache observation was lost: %+v", got)
	}
	if got.CacheRateFound || got.CacheTotal != 0 {
		t.Fatalf("impossible metadata cache rate was accepted: %+v", got)
	}
}

func TestCacheUsageAggregateExcludesUncomputableSamplesFromRate(t *testing.T) {
	var aggregate cacheUsageAggregate
	aggregate.add(usageSummary{CacheFound: true, CacheRateFound: true, CacheRead: 60, CacheTotal: 100})
	aggregate.add(usageSummary{CacheFound: true, CacheRead: 101})
	response := aggregate.response()
	if response["read_tokens"] != int64(161) || response["observed_requests"] != int64(2) || response["rate_observed_requests"] != int64(1) || response["input_tokens"] != int64(100) {
		t.Fatalf("raw cache accounting=%+v", response)
	}
	if got, ok := response["hit_rate"].(float64); !ok || got != 0.6 {
		t.Fatalf("mixed-sample cache rate=%v", response["hit_rate"])
	}
	if got := (cacheUsageAggregate{rateRead: 101, input: 100}).hitRate(); got != nil {
		t.Fatalf("invalid rate=%v", got)
	}
}

func TestExtractUsageResponsesAndOllamaStreamSnapshots(t *testing.T) {
	responses := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-4o\",\"usage\":{\"input_tokens\":40,\"output_tokens\":12,\"input_tokens_details\":{\"cached_tokens\":30}}}}\n\n"
	got := extractUsage([]byte(responses))
	if got.Model != "gpt-4o" || got.Prompt != 40 || got.Completion != 12 || got.CacheRead != 30 || !got.CacheRateFound {
		t.Fatalf("responses usage=%+v", got)
	}

	ollama := "{\"model\":\"llama3\",\"prompt_eval_count\":20,\"eval_count\":4,\"done\":false}\n" +
		"{\"model\":\"llama3\",\"prompt_eval_count\":25,\"eval_count\":6,\"done\":true}\n"
	got = extractUsage([]byte(ollama))
	if got.Model != "llama3" || got.Prompt != 25 || got.Completion != 6 {
		t.Fatalf("ollama usage=%+v", got)
	}
}

func TestExtractUsageIgnoresUnrelatedTokenFields(t *testing.T) {
	body := []byte(`{"metadata":{"input_tokens":999,"output_tokens":888},"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	got := extractUsage(body)
	if got.Prompt != 10 || got.Completion != 2 {
		t.Fatalf("unrelated fields were counted: %+v", got)
	}
}

func TestMergeUsagePrefersMoreCompleteCacheCounts(t *testing.T) {
	got := mergeUsage(
		usageSummary{Prompt: 100, PromptFound: true, CacheFound: true},
		usageSummary{Prompt: 100, PromptFound: true, CacheRead: 60, CacheFound: true, CacheRateFound: true, CacheTotal: 100},
	)
	if got.CacheRead != 60 || !got.CacheRateFound || got.CacheTotal != 100 {
		t.Fatalf("merged usage=%+v", got)
	}
}

func TestExtractUsageStreamingFrames(t *testing.T) {
	body := "data: {\"model\":\"gpt-4o\",\"choices\":[]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":9}}\n" +
		"data: [DONE]\n"
	got := extractUsage([]byte(body))
	if !got.Found || got.Model != "gpt-4o" || got.Prompt != 21 || got.Completion != 9 {
		t.Fatalf("usage=%+v", got)
	}

	ndjson := "{\"model\":\"claude-3-haiku\"}\n{\"usage\":{\"input_tokens\":8,\"output_tokens\":13}}\n"
	got = extractUsage([]byte(ndjson))
	if !got.Found || got.Model != "claude-3-haiku" || got.Prompt != 8 || got.Completion != 13 {
		t.Fatalf("ndjson usage=%+v", got)
	}
}

func TestExtractUsageMalformedAndUnsafeNumbers(t *testing.T) {
	for _, body := range []string{
		"not json",
		`{"usage":{"prompt_tokens":-1,"completion_tokens":"wat"}}`,
		`{"usage":{"prompt_tokens":1.5,"completion_tokens":2.5}}`,
	} {
		got := extractUsage([]byte(body))
		if got.Found {
			t.Fatalf("malformed body produced usage: body=%q usage=%+v", body, got)
		}
	}
}

func TestUsageFromMeta(t *testing.T) {
	got := usageFromMeta(`{"model":"gpt-4o","usage_found":true,"usage_prompt_tokens":12,"usage_completion_tokens":7,"usage_cache_observed":true,"usage_cache_read_tokens":5,"usage_cache_write_tokens":2,"usage_cache_rate_observed":true,"usage_cache_input_tokens":12}`)
	if !got.Found || got.Model != "gpt-4o" || got.Prompt != 12 || got.Completion != 7 {
		t.Fatalf("usage=%+v", got)
	}
	if !got.CacheFound || got.CacheRead != 5 || got.CacheWrite != 2 || !got.CacheRateFound || got.CacheTotal != 12 {
		t.Fatalf("cache metadata=%+v", got)
	}
	if got := usageFromMeta("not json"); got.Found || got.Model != "" {
		t.Fatalf("invalid metadata usage=%+v", got)
	}
}
