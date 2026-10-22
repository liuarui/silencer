package gateway

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

type extensionLogError string

// A browser response arrives through later HTTP calls. Its mapping authority
// cannot retain the already-cancelled mask request context. Each lookup/write
// still has its own bounded database operation.
type browserMappingStore struct{ DurableMappingStore }

func (s browserMappingStore) IssueEntityMapping(ctx context.Context, scope, original, label string) (storage.EntityMapping, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.DurableMappingStore.IssueEntityMapping(ctx, scope, original, label)
}

func (s browserMappingStore) ResolveEntityMapping(ctx context.Context, scope, token string) (storage.EntityMapping, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.DurableMappingStore.ResolveEntityMapping(ctx, scope, token)
}

func (e extensionLogError) Error() string { return string(e) }

// Browser bodies are the text sent through the bridge, not a capture of the
// browser's complete HTTP exchange (multipart and binary parts may be absent).
func (s *Server) newExtensionRecord(ctx context.Context, text, host, target, method string) (*requestRecord, error) {
	if s.store == nil || !s.extensionRecordEventsEnabled() {
		return nil, nil
	}
	path := "/api/ext/mask"
	if parsed, err := url.Parse(target); err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" {
		path = parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
	}
	if method != "POST" && method != "PUT" && method != "PATCH" {
		method = "POST"
	}
	now := time.Now().UTC()
	detail := s.cfg.StoreRawBody || s.recordDetailDataEnabled()
	s.configMu.RLock()
	limit := s.cfg.MaxResponseBytes
	s.configMu.RUnlock()
	if detail && limit > 64<<10 {
		limit = 64 << 10
	}
	meta, _ := json.Marshal(map[string]any{
		"ingress": "ext", "host": host, "client": "browser", "path": path,
		"method": method, "body_scope": "bridge_text", "mask_status": "pending", "response_status": "pending",
	})
	record := &requestRecord{store: s.store, metrics: &s.metrics, tokenReuse: s.tokenReuse, maxBytes: limit, storeRawBody: detail,
		item: storage.Request{ID: newRequestID(), Payload: []byte(method + " " + path), Status: "running", Meta: string(meta), StartedAt: now, CreatedAt: now}}
	record.capture = newRequestCapture(s.logDirectory(), record.item.ID, detail)
	if record.capture.err != nil {
		record.capture.discard()
		return nil, extensionLogError("log_storage_unavailable")
	}
	record.appendRequest(false, []byte(text))
	record.item.Raw = append([]byte(nil), record.requestRaw...)
	if err := s.store.AppendRequest(ctx, record.item); err != nil {
		record.capture.discard()
		return nil, extensionLogError("storage_unavailable")
	}
	return record, nil
}

func setExtensionRecordMeta(record *requestRecord, values map[string]any) {
	if record == nil {
		return
	}
	meta := requestMeta(record.item)
	for key, value := range values {
		meta[key] = value
	}
	raw, _ := json.Marshal(meta)
	record.item.Meta = string(raw)
}

func (s *Server) recordExtensionMask(record *requestRecord, masked []byte, items []map[string]any) {
	if record == nil {
		return
	}
	record.appendRequest(true, masked)
	record.maskedItems, record.maskedItemsComplete = items, true
	record.item.Masked = append([]byte(nil), record.requestMasked...)
	record.item.Status = "awaiting_response"
	status := "masked"
	if record.maskedTokenCount == 0 {
		status = "no_sensitive_data"
	}
	setExtensionRecordMeta(record, map[string]any{"mask_status": status, "mask_token_count": record.maskedTokenCount, "items": items, "items_complete": true, "body_truncated": record.requestRawTruncated || record.requestMaskedTruncated})
	if err := s.store.UpdateRequest(context.Background(), record.item); err != nil {
		s.metrics.storageErrors.Add(1)
		s.logger.Print("extension request ledger update failed")
	}
	meta := requestMeta(record.item)
	meta["count"], meta["mask_count"] = record.maskedTokenCount, record.maskedTokenCount
	record.maskEventRecorded = s.appendExtensionEvent(record.item.ID, "MASK", status, meta)
}

func (entry *extensionRestoreEntry) observeResponse(input, output []byte) {
	if entry.evidence == nil {
		entry.evidence = map[string]restoreTokenEvidence{}
	}
	entry.restorePlaceholders += observeMaskedTokens(&entry.restoreInputTail, input, func(token string) {
		value := entry.evidence[token]
		value.Observed++
		entry.evidence[token] = value
	})
	entry.restoreUnresolved += observeMaskedTokens(&entry.restoreOutputTail, output, func(token string) {
		value := entry.evidence[token]
		value.Remaining++
		entry.evidence[token] = value
	})
	if entry.record != nil {
		entry.record.appendResponse(false, input)
		entry.record.appendResponse(true, output)
	}
}

// Called with entry.mu held. The caller removes the entry without inverting
// the map/entry lock order. A failed or abandoned stream never claims success.
func (entry *extensionRestoreEntry) finishRecord(outcome string, status int) {
	if entry.closed {
		return
	}
	entry.closed = true
	if entry.maskOwner != nil {
		entry.maskOwner.Release()
	}
	if entry.record == nil {
		return
	}
	complete := outcome == "completed"
	restored := entry.restorePlaceholders - entry.restoreUnresolved
	if restored < 0 {
		restored = 0
	}
	entry.record.protocolCounts = &restoreCounts{Total: entry.restorePlaceholders, Restored: restored, Unresolved: entry.restoreUnresolved, Tokens: entry.evidence, Complete: complete}
	setExtensionRecordMeta(entry.record, map[string]any{"response_status": outcome, "upstream_status": entry.upstreamStatus, "upstream_status_observed": entry.upstreamStatus > 0})
	if outcome == "mask_not_applied" {
		setExtensionRecordMeta(entry.record, map[string]any{"mask_status": "not_applied"})
	}
	var err error
	if !complete {
		err = extensionLogError(outcome)
	}
	if complete && entry.upstreamStatus > 0 {
		status = entry.upstreamStatus
	}
	entry.record.finish(status, err)
}

func extensionOutcome(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "cancelled", "network_error", "restore_failed", "unsupported_response", "response_stream_error", "mask_not_applied":
		return value
	default:
		return "completed"
	}
}
