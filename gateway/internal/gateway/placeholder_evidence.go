package gateway

import "strings"

func isCanonicalPlaceholder(token string) bool {
	return token != "" && maskedTokenPattern.FindString(token) == token
}

// Older completed responses counted ordinary template strings as identities.
// Project their complete per-token evidence with the current format rules,
// without rewriting history or hiding a real unresolved gateway identity.
// Incomplete evidence cannot establish a successful restoration.
func canonicalRestoreEvidence(meta map[string]any) map[string]any {
	if meta["restore_evidence_complete"] != true || meta["restore_examples_ignored"] == true {
		return meta
	}
	evidence, ok := meta["restore_evidence"].(map[string]any)
	if !ok {
		return meta
	}
	invalid := false
	for token := range evidence {
		if !isCanonicalPlaceholder(token) {
			invalid = true
			break
		}
	}
	if !invalid {
		return meta
	}
	filtered := make(map[string]any, len(evidence))
	total, remaining := 0, 0
	for token, value := range evidence {
		if !isCanonicalPlaceholder(token) {
			continue
		}
		e, ok := value.(map[string]any)
		if !ok {
			return meta
		}
		observed, a := numericMetaInt(e["observed"])
		unresolved, b := numericMetaInt(e["remaining"])
		if !a || !b || observed < 0 || unresolved < 0 {
			return meta
		}
		filtered[token] = value
		total += observed
		remaining += unresolved
	}
	if remaining > total {
		remaining = total
	}
	result := cloneReadMetadata(meta)
	result["restore_evidence"] = filtered
	result["restore_count"] = total
	result["restore_restored"] = total - remaining
	result["restore_unresolved"] = remaining
	if _, exists := meta["unresolved"]; exists {
		result["unresolved"] = remaining
	}
	if _, exists := meta["restored"]; exists {
		result["restored"] = total - remaining
	}
	result["restore_examples_ignored"] = true
	status := "no_sensitive_data"
	masked, _ := numericMetaInt(meta["mask_token_count"])
	if masked == 0 {
		masked, _ = numericMetaInt(meta["mask_count"])
	}
	switch {
	case remaining > 0:
		status = "unresolved"
	case total > 0:
		status = "restored"
	case masked > 0:
		status = "no_placeholder_in_response"
	}
	result["restore_status"] = status
	return result
}

func canonicalRestoreEventMeta(meta map[string]any, typ string) map[string]any {
	result := canonicalRestoreEvidence(meta)
	if !strings.EqualFold(typ, "RESTORE") || result["restore_examples_ignored"] != true {
		return result
	}
	result = cloneReadMetadata(result)
	result["count"] = result["restore_count"]
	result["restored"] = result["restore_restored"]
	result["unresolved"] = result["restore_unresolved"]
	result["status"] = result["restore_status"]
	return result
}

func canonicalRestoreStatus(status string, meta map[string]any) string {
	if status == "unresolved" && meta["restore_examples_ignored"] == true {
		if corrected, ok := meta["restore_status"].(string); ok {
			return corrected
		}
	}
	return status
}
