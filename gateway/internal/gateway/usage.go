package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// usageSummary is deliberately limited to numeric accounting fields. The
// response body itself is never copied into control-plane metadata.
type usageSummary struct {
	Model               string
	Prompt              int64
	Completion          int64
	CacheRead           int64
	CacheWrite          int64
	CacheTotal          int64
	CacheFound          bool
	CacheRateFound      bool
	PromptFound         bool
	CompletionFound     bool
	AnthropicCacheShape bool
	Found               bool
}

type usageCandidate struct {
	summary usageSummary
	kind    string
}

func mergeUsage(primary, secondary usageSummary) usageSummary {
	if primary.Model == "" {
		primary.Model = secondary.Model
	}
	if secondary.PromptFound && (!primary.PromptFound || secondary.Prompt > primary.Prompt) {
		primary.Prompt = secondary.Prompt
		primary.PromptFound = true
	}
	if secondary.CompletionFound && (!primary.CompletionFound || secondary.Completion > primary.Completion) {
		primary.Completion = secondary.Completion
		primary.CompletionFound = true
	}
	if secondary.CacheFound {
		if secondary.CacheRead > primary.CacheRead {
			primary.CacheRead = secondary.CacheRead
		}
		if secondary.CacheWrite > primary.CacheWrite {
			primary.CacheWrite = secondary.CacheWrite
		}
		primary.CacheFound = true
		primary.CacheRateFound = primary.CacheRateFound || secondary.CacheRateFound
		if secondary.CacheTotal > primary.CacheTotal {
			primary.CacheTotal = secondary.CacheTotal
		}
		primary.AnthropicCacheShape = primary.AnthropicCacheShape || secondary.AnthropicCacheShape
	}
	primary.Found = primary.Found || secondary.Found
	if !primary.CacheRateFound && primary.CacheFound && primary.PromptFound {
		primary.CacheRateFound = true
		primary.CacheTotal = primary.Prompt
		if primary.AnthropicCacheShape {
			primary.CacheTotal += primary.CacheRead + primary.CacheWrite
		}
	}
	return primary
}

// extractUsage accepts ordinary JSON, SSE data frames, and newline-delimited
// JSON. It only reads usage objects from provider-defined envelope keys. This
// prevents unrelated payload fields with names such as input_tokens from being
// mistaken for model accounting data.
func extractUsage(body []byte) usageSummary {
	if len(body) == 0 {
		return usageSummary{}
	}
	var candidates []usageCandidate
	var model string
	if parsed, foundModel, ok := decodeUsageValue(body); ok {
		if candidate, valid := selectUsageCandidate(parsed); valid {
			candidates = append(candidates, candidate)
		}
		model = foundModel
		out := combineUsageCandidates(candidates)
		if out.Model == "" {
			out.Model = model
		}
		finalizeUsage(&out)
		return out
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024), len(body)+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		parsed, foundModel, ok := decodeUsageValue(line)
		if !ok {
			continue
		}
		if model == "" {
			model = foundModel
		}
		if candidate, valid := selectUsageCandidate(parsed); valid {
			candidates = append(candidates, candidate)
		}
	}
	out := combineUsageCandidates(candidates)
	if out.Model == "" {
		out.Model = model
	}
	finalizeUsage(&out)
	return out
}

func decodeUsageValue(raw []byte) ([]usageCandidate, string, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, "", false
	}
	var candidates []usageCandidate
	var model string
	knownUsageEnvelopes(value, &candidates, &model, "", 0)
	return candidates, model, true
}

func knownUsageEnvelopes(value any, candidates *[]usageCandidate, model *string, event string, depth int) {
	if depth > 12 {
		return
	}
	current, ok := value.(map[string]any)
	if !ok {
		if items, ok := value.([]any); ok {
			for _, item := range items {
				knownUsageEnvelopes(item, candidates, model, event, depth+1)
			}
		}
		return
	}

	localModel, _ := current["model"].(string)
	if localModel == "" {
		localModel, _ = current["modelVersion"].(string)
	}
	if *model == "" && localModel != "" {
		*model = localModel
	}
	localEvent, _ := current["type"].(string)
	if localEvent == "" {
		localEvent = event
	}
	if usage, ok := current["usage"]; ok {
		if summary, valid := collectUsageObject(usage); valid {
			if summary.Model == "" {
				summary.Model = localModel
			}
			*candidates = append(*candidates, usageCandidate{summary: summary, kind: usageKind(localEvent, summary)})
		}
	}
	// Gemini names the response accounting envelope `usageMetadata` and the
	// cache read field `cachedContentTokenCount`. Keep it provider-specific so
	// ordinary business metadata cannot create a synthetic cache sample.
	if usage, ok := current["usageMetadata"]; ok {
		if summary, valid := collectUsageObject(usage); valid {
			if summary.Model == "" {
				summary.Model = localModel
			}
			*candidates = append(*candidates, usageCandidate{summary: summary, kind: "gemini"})
		}
	}
	// Ollama reports its final accounting directly on the response object.
	if summary, valid := collectOllamaUsage(current); valid {
		if summary.Model == "" {
			summary.Model = localModel
		}
		*candidates = append(*candidates, usageCandidate{summary: summary, kind: "ollama"})
	}

	// These are the provider envelope keys that may contain another usage
	// object. Do not recursively inspect arbitrary business payload fields.
	for _, key := range []string{"response", "message", "choices", "output"} {
		if child, ok := current[key]; ok {
			knownUsageEnvelopes(child, candidates, model, localEvent, depth+1)
		}
	}
}

func collectUsageObject(value any) (usageSummary, bool) {
	current, ok := value.(map[string]any)
	if !ok {
		return usageSummary{}, false
	}
	var out usageSummary
	for key, child := range current {
		switch strings.ToLower(key) {
		case "prompt_tokens", "input_tokens":
			if count, valid := usageNumber(child); valid {
				if !out.PromptFound || count > out.Prompt {
					out.Prompt = count
				}
				out.PromptFound = true
				out.Found = true
			}
		case "completion_tokens", "output_tokens":
			if count, valid := usageNumber(child); valid {
				if !out.CompletionFound || count > out.Completion {
					out.Completion = count
				}
				out.CompletionFound = true
				out.Found = true
			}
		case "cached_tokens", "cache_read_input_tokens", "cached_input_tokens":
			if count, valid := usageNumber(child); valid {
				if count > out.CacheRead {
					out.CacheRead = count
				}
				out.CacheFound = true
				if strings.EqualFold(key, "cache_read_input_tokens") {
					out.AnthropicCacheShape = true
				}
			}
		case "cache_creation_input_tokens", "cache_creation_tokens":
			if count, valid := usageNumber(child); valid {
				if count > out.CacheWrite {
					out.CacheWrite = count
				}
				out.CacheFound = true
				if strings.EqualFold(key, "cache_creation_input_tokens") {
					out.AnthropicCacheShape = true
				}
			}
		case "prompt_tokens_details", "input_tokens_details":
			collectCacheDetails(child, &out)
		case "prompttokencount":
			if count, valid := usageNumber(child); valid {
				out.Prompt = count
				out.PromptFound = true
				out.Found = true
			}
		case "candidatestokencount":
			if count, valid := usageNumber(child); valid {
				out.Completion = count
				out.CompletionFound = true
				out.Found = true
			}
		case "cachedcontenttokencount":
			if count, valid := usageNumber(child); valid {
				out.CacheRead = count
				out.CacheFound = true
			}
		}
	}
	return out, out.Found || out.CacheFound
}

func collectCacheDetails(value any, out *usageSummary) {
	current, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range current {
		switch strings.ToLower(key) {
		case "cached_tokens", "cache_read_input_tokens", "cached_input_tokens":
			if count, valid := usageNumber(child); valid {
				if count > out.CacheRead {
					out.CacheRead = count
				}
				out.CacheFound = true
				if strings.EqualFold(key, "cache_read_input_tokens") {
					out.AnthropicCacheShape = true
				}
			}
		case "cache_creation_input_tokens", "cache_creation_tokens":
			if count, valid := usageNumber(child); valid {
				if count > out.CacheWrite {
					out.CacheWrite = count
				}
				out.CacheFound = true
				if strings.EqualFold(key, "cache_creation_input_tokens") {
					out.AnthropicCacheShape = true
				}
			}
		}
	}
}

func collectOllamaUsage(value map[string]any) (usageSummary, bool) {
	var out usageSummary
	if count, valid := usageNumber(value["prompt_eval_count"]); valid {
		out.Prompt = count
		out.PromptFound = true
		out.Found = true
	}
	if count, valid := usageNumber(value["eval_count"]); valid {
		out.Completion = count
		out.CompletionFound = true
		out.Found = true
	}
	return out, out.Found
}

func usageKind(event string, summary usageSummary) string {
	switch strings.ToLower(event) {
	case "message_start":
		return "anthropic_start"
	case "message_delta", "message_stop":
		return "anthropic_delta"
	}
	if summary.AnthropicCacheShape {
		return "anthropic"
	}
	return "generic"
}

func usageShape(summary usageSummary) [4]bool {
	return [4]bool{summary.PromptFound, summary.CompletionFound, summary.CacheFound, summary.AnthropicCacheShape}
}

func usageScore(summary usageSummary) int {
	score := 0
	if summary.PromptFound {
		score++
	}
	if summary.CompletionFound {
		score++
	}
	if summary.CacheFound {
		score++
	}
	return score
}

func canMergeUsageCandidates(primary, secondary usageCandidate) bool {
	if primary.kind == secondary.kind {
		// A streaming provider may emit prompt/cache accounting first and the
		// completion count in a later cumulative snapshot. Each NDJSON/SSE line
		// has already selected one provider envelope, so merging same-kind lines
		// cannot combine unrelated nested objects from a single JSON response.
		if primary.kind == "gemini" || primary.kind == "ollama" || primary.kind == "generic" {
			return true
		}
		return usageShape(primary.summary) == usageShape(secondary.summary)
	}
	if (primary.kind == "anthropic_start" && secondary.kind == "anthropic_delta") ||
		(primary.kind == "anthropic_delta" && secondary.kind == "anthropic_start") {
		return primary.summary.PromptFound != secondary.summary.PromptFound ||
			primary.summary.CompletionFound != secondary.summary.CompletionFound
	}
	return false
}

func combineUsageCandidates(candidates []usageCandidate) usageSummary {
	var selected usageCandidate
	selectedValid := false
	for _, candidate := range candidates {
		if !candidate.summary.Found && !candidate.summary.CacheFound {
			continue
		}
		if !selectedValid {
			selected = candidate
			selectedValid = true
			continue
		}
		if canMergeUsageCandidates(selected, candidate) {
			selected.summary = mergeUsage(selected.summary, candidate.summary)
			continue
		}
		if usageScore(candidate.summary) >= usageScore(selected.summary) {
			selected = candidate
		}
	}
	if !selectedValid {
		return usageSummary{}
	}
	return selected.summary
}

func selectUsageCandidate(candidates []usageCandidate) (usageCandidate, bool) {
	var selected usageCandidate
	valid := false
	for _, candidate := range candidates {
		if !candidate.summary.Found && !candidate.summary.CacheFound {
			continue
		}
		if !valid || usageScore(candidate.summary) > usageScore(selected.summary) {
			selected = candidate
			valid = true
		}
	}
	return selected, valid
}

func finalizeUsage(out *usageSummary) {
	if !out.CacheFound || !out.PromptFound {
		return
	}
	// A provider can report cache counters without a usable total (or return
	// inconsistent counters while a stream is still in flight). Keep the raw
	// read/write observation, but do not publish a fabricated or >100% rate.
	out.CacheRateFound = false
	out.CacheTotal = 0
	maxInt64 := int64(^uint64(0) >> 1)
	total := out.Prompt
	if out.AnthropicCacheShape {
		if out.CacheRead > maxInt64-total {
			return
		}
		total += out.CacheRead
		if out.CacheWrite > maxInt64-total {
			return
		}
		total += out.CacheWrite
	}
	out.CacheTotal = total
	if out.CacheTotal <= 0 || out.CacheRead > out.CacheTotal {
		out.CacheTotal = 0
		return
	}
	out.CacheRateFound = true
}

func usageNumber(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		if number >= 0 && number <= float64(^uint64(0)>>1) && number == float64(int64(number)) {
			return int64(number), true
		}
	case json.Number:
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if err == nil && parsed >= 0 {
			return parsed, true
		}
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(number), 10, 64)
		if err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

func usageFromMeta(meta string) usageSummary {
	if strings.TrimSpace(meta) == "" {
		return usageSummary{}
	}
	var value map[string]any
	if json.Unmarshal([]byte(meta), &value) != nil {
		return usageSummary{}
	}
	return usageFromMetaMap(value)
}

func usageFromMetaMap(value map[string]any) usageSummary {
	promptPresent, ok := value["usage_prompt_observed"].(bool)
	if !ok {
		_, promptPresent = value["usage_prompt_tokens"]
	}
	usage := usageSummary{
		Model:               stringValue(value["model"]),
		Prompt:              intValue(value["usage_prompt_tokens"]),
		Completion:          intValue(value["usage_completion_tokens"]),
		CacheRead:           intValue(value["usage_cache_read_tokens"]),
		CacheWrite:          intValue(value["usage_cache_write_tokens"]),
		CacheTotal:          intValue(value["usage_cache_input_tokens"]),
		CacheFound:          value["usage_cache_observed"] == true,
		CacheRateFound:      value["usage_cache_rate_observed"] == true,
		AnthropicCacheShape: value["usage_cache_anthropic_shape"] == true,
		PromptFound:         promptPresent,
		CompletionFound:     hasKey(value, "usage_completion_tokens"),
		Found:               value["usage_found"] == true,
	}
	if usage.CacheRateFound && (usage.CacheTotal <= 0 || usage.CacheRead > usage.CacheTotal) {
		usage.CacheRateFound = false
		usage.CacheTotal = 0
	}
	return usage
}

func hasKey(values map[string]any, key string) bool {
	_, ok := values[key]
	return ok
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intValue(value any) int64 {
	parsed, _ := usageNumber(value)
	return parsed
}
