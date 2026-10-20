package gateway

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/silencer/gateway/internal/hostagent"
	"github.com/silencer/gateway/internal/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const requestPrefix = "/api/requests"

const extensionRestoreIdleTTL = 10 * time.Minute
const maxUsageTailBytes = 64 << 10

func isDataPlanePath(path string) bool {
	return strings.HasPrefix(path, requestPrefix+"/") || strings.HasPrefix(path, "/r/")
}

func (s *Server) hostAgentCall(ctx context.Context, op string, payload any) (hostagent.Response, error) {
	return (hostagent.Client{Path: s.cfg.HostAgentSocket}).Call(ctx, op, payload)
}

func hostAgentBody(resp hostagent.Response) map[string]any {
	body := map[string]any{"ok": resp.OK, "available": resp.Available}
	if fields, ok := resp.Data.(map[string]any); ok {
		for key, value := range fields {
			body[key] = value
		}
	} else if resp.Data != nil {
		body["data"] = resp.Data
	}
	if resp.Error != "" {
		body["error"] = resp.Error
	}
	return body
}

func (s *Server) writeHostAgentResponse(w http.ResponseWriter, resp hostagent.Response, allowSoftFailure bool) {
	status := http.StatusOK
	if !resp.OK && !allowSoftFailure {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, hostAgentBody(resp))
}

type Server struct {
	capture  *captureController
	cfg      Config
	engine   Engine
	engineMu sync.RWMutex
	// lifecycleMu serializes engine replacement with Shutdown. Once shutdown
	// begins, a control-plane rebuild must not install an engine that nobody
	// owns or closes.
	lifecycleMu       sync.Mutex
	shuttingDown      bool
	retiredMu         sync.Mutex
	retired           []*retiredEngine
	configPath        string
	proxy             *httputil.ReverseProxy
	proxyTransport    http.RoundTripper
	limiter           chan struct{}
	limiterMu         sync.RWMutex
	metrics           metrics
	logger            *log.Logger
	ready             atomicReady
	tokenReuse        *tokenReuseTracker
	configMu          sync.RWMutex
	store             *storage.Storage
	requests          requestDrain
	started           time.Time
	control           map[string]any
	running           bool
	extMu             sync.Mutex
	extRestore        map[string]*extensionRestoreEntry
	auditMu           sync.Mutex
	auditJob          map[string]any
	auditCancel       context.CancelFunc
	logRetentionMu    sync.Mutex
	logWakeMu         sync.Mutex
	logCancel         context.CancelFunc
	logWake           chan struct{}
	logDone           chan struct{}
	extensionDisabled bool
	logSweepMu        sync.Mutex
	logOrphanDir      *os.File
}

const (
	tokenReuseTTL = 24 * time.Hour
	tokenReuseMax = 100000
)

// tokenReuseTracker records only placeholder strings, never their originals.
// It exists for cache diagnostics: the C++ engine makes tokens deterministic,
// while this bounded window tells the dashboard whether a later request reused
// a token issued by an earlier request.
type tokenReuseTracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newTokenReuseTracker() *tokenReuseTracker {
	return &tokenReuseTracker{seen: make(map[string]time.Time)}
}

func (t *tokenReuseTracker) observe(body []byte) bool {
	if t == nil || len(body) == 0 {
		return false
	}
	matches := maskedTokenPattern.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return false
	}
	now := time.Now()
	current := make(map[string]struct{}, len(matches))
	t.mu.Lock()
	defer t.mu.Unlock()
	reused := false
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		token := string(match[0])
		if _, duplicate := current[token]; duplicate {
			continue
		}
		current[token] = struct{}{}
		if issued, ok := t.seen[token]; ok && now.Sub(issued) <= tokenReuseTTL {
			reused = true
		}
	}
	for token := range current {
		t.seen[token] = now
	}
	if len(t.seen) > tokenReuseMax {
		cutoff := now.Add(-tokenReuseTTL)
		for token, issued := range t.seen {
			if issued.Before(cutoff) {
				delete(t.seen, token)
			}
		}
		if len(t.seen) > tokenReuseMax {
			// A full sort would add latency to the request that crosses the
			// bound. Keep the diagnostic map bounded with a deterministic scan.
			for token := range t.seen {
				if len(t.seen) <= tokenReuseMax {
					break
				}
				delete(t.seen, token)
			}
		}
	}
	return reused
}

// retiredEngine gives a comparable identity without comparing the dynamic
// value held by CloseableEngine. Engine implementations are allowed to be
// non-comparable values, so interface equality would be unsafe here.
type retiredEngine struct{ closeable CloseableEngine }

type extensionRestoreEntry struct {
	mu                  sync.Mutex
	session             RestoreSession
	maskOwner           ContextualMaskingSession
	streamID            string
	lastUsed            time.Time
	requestID           string
	record              *requestRecord
	closed              bool
	restoreFinished     bool
	upstreamStatus      int
	evidence            map[string]restoreTokenEvidence
	maskedCount         int
	restorePlaceholders int
	restoreUnresolved   int
	restoreInputTail    string
	restoreOutputTail   string
}

// restoreTokenTailLimit bounds the small rolling window used to count tokens
// that are split across extension frames. We never retain response text here;
// only the tail needed to recognize a placeholder crossing a frame boundary.
const restoreTokenTailLimit = 256

func observeMaskedTokenCount(tail *string, body []byte) int {
	return observeMaskedTokens(tail, body, nil)
}

func observeMaskedTokens(tail *string, body []byte, observe func(string)) int {
	if tail == nil || len(body) == 0 {
		return 0
	}
	previous := []byte(*tail)
	combined := make([]byte, 0, len(previous)+len(body))
	combined = append(combined, previous...)
	combined = append(combined, body...)
	boundary := len(previous)
	count := 0
	for _, match := range maskedTokenPattern.FindAllIndex(combined, -1) {
		// Matches wholly inside the prior tail were already counted. A match
		// ending after the boundary is new, including a token split over frames.
		if match[1] > boundary {
			count++
			if observe != nil {
				observe(string(combined[match[0]:match[1]]))
			}
		}
	}
	if len(combined) > restoreTokenTailLimit {
		*tail = string(combined[len(combined)-restoreTokenTailLimit:])
	} else {
		*tail = string(combined)
	}
	return count
}

type requestRecordContextKey struct{}
type contextualMaskingSessionContextKey struct{}
type maskingBodyContextKey struct{}

type requestRecord struct {
	store                  *storage.Storage
	metrics                *metrics
	tokenReuse             *tokenReuseTracker
	maxBytes               int64
	capture                *bodyCapture
	auditEnabled           bool
	auditPassive           bool
	auditErrorLeak         bool
	auditSeverityFloor     string
	storeRawBody           bool
	item                   storage.Request
	mu                     sync.Mutex
	upstream               []byte
	restored               []byte
	upstreamUsageTail      usageTail
	restoredUsageTail      usageTail
	requestRaw             []byte
	requestMasked          []byte
	requestRawTruncated    bool
	requestMaskedTruncated bool
	requestBodyIncomplete  bool
	maskInputBytes         int64
	maskOutputBytes        int64
	maskCompareRaw         []byte
	maskCompareMasked      []byte
	maskCompareTruncated   bool
	maskedItems            []map[string]any
	maskedItemsComplete    bool
	mappingProvider        MaskedItemsProvider
	maskedTokenCount       int
	maskedTokenTail        string
	protocolCounts         *restoreCounts
	responseWriteErr       error
	failureStage           string
	upstreamTruncated      bool
	restoredTruncated      bool
	done                   bool
	maskEventRecorded      bool
}

type usageTail struct {
	buffer []byte
	next   int
	size   int
}

func (t *usageTail) append(data []byte) {
	if len(data) == 0 {
		return
	}
	if t.buffer == nil {
		t.buffer = make([]byte, maxUsageTailBytes)
	}
	if len(data) >= len(t.buffer) {
		copy(t.buffer, data[len(data)-len(t.buffer):])
		t.next = 0
		t.size = len(t.buffer)
		return
	}
	first := len(data)
	if remaining := len(t.buffer) - t.next; first > remaining {
		first = remaining
	}
	copy(t.buffer[t.next:t.next+first], data[:first])
	copy(t.buffer[:len(data)-first], data[first:])
	t.next = (t.next + len(data)) % len(t.buffer)
	t.size += len(data)
	if t.size > len(t.buffer) {
		t.size = len(t.buffer)
	}
}

func (t *usageTail) bytes() []byte {
	if t == nil || t.size == 0 {
		return nil
	}
	start := (t.next - t.size + len(t.buffer)) % len(t.buffer)
	if start+t.size <= len(t.buffer) {
		return append([]byte(nil), t.buffer[start:start+t.size]...)
	}
	result := make([]byte, t.size)
	first := len(t.buffer) - start
	copy(result, t.buffer[start:])
	copy(result[first:], t.buffer[:t.size-first])
	return result
}

func (r *requestRecord) append(dst *[]byte, data []byte) {
	if len(data) == 0 {
		return
	}
	maxBytes := r.maxBytes
	if maxBytes < 1 {
		maxBytes = 8 << 20
	}
	if int64(len(*dst)) >= maxBytes {
		return
	}
	left := int(maxBytes) - len(*dst)
	if len(data) > left {
		data = data[:left]
	}
	*dst = append(*dst, data...)
}

func (r *requestRecord) appendResponse(restored bool, data []byte) {
	stage := "upstream_response"
	if restored {
		stage = "restored_response"
	}
	r.capture.append(stage, data)
	if len(data) == 0 {
		return
	}
	maxBytes := r.maxBytes
	if maxBytes < 1 {
		maxBytes = 8 << 20
	}
	if restored {
		if int64(len(r.restored))+int64(len(data)) > maxBytes {
			if !r.restoredTruncated {
				r.restoredUsageTail.append(r.restored)
			}
			r.restoredTruncated = true
			r.restoredUsageTail.append(data)
		}
		r.append(&r.restored, data)
		return
	}
	if int64(len(r.upstream))+int64(len(data)) > maxBytes {
		if !r.upstreamTruncated {
			r.upstreamUsageTail.append(r.upstream)
		}
		r.upstreamTruncated = true
		r.upstreamUsageTail.append(data)
	}
	r.append(&r.upstream, data)
}

func (r *requestRecord) responseUsage() usageSummary {
	usage := mergeUsage(extractUsage(r.restored), extractUsage(r.restoredUsageTail.bytes()))
	usage = mergeUsage(usage, extractUsage(r.upstream))
	usage = mergeUsage(usage, extractUsage(r.upstreamUsageTail.bytes()))
	// The response may combine a bounded body and its tail. Recompute the
	// denominator from the final counters so a partial or inconsistent stream
	// cannot leave a stale cache rate in request metadata.
	finalizeUsage(&usage)
	return usage
}

func (r *requestRecord) appendRequest(masked bool, data []byte) {
	stage := "raw"
	if masked {
		stage = "masked"
	}
	r.capture.append(stage, data)
	if masked {
		r.maskedTokenCount += observeMaskedTokenCount(&r.maskedTokenTail, data)
	}
	if len(data) == 0 {
		return
	}
	const compareLimit = 64 << 10
	// The comparison window must not shorten the separately retained ledger.
	comparison := data
	if masked {
		r.maskOutputBytes += int64(len(data))
		if len(r.maskCompareMasked) < compareLimit {
			remaining := compareLimit - len(r.maskCompareMasked)
			if len(comparison) > remaining {
				r.maskCompareTruncated = true
				comparison = comparison[:remaining]
			}
			r.maskCompareMasked = append(r.maskCompareMasked, comparison...)
		} else if len(data) > 0 {
			r.maskCompareTruncated = true
		}
	} else {
		r.maskInputBytes += int64(len(data))
		if len(r.maskCompareRaw) < compareLimit {
			remaining := compareLimit - len(r.maskCompareRaw)
			if len(comparison) > remaining {
				r.maskCompareTruncated = true
				comparison = comparison[:remaining]
			}
			r.maskCompareRaw = append(r.maskCompareRaw, comparison...)
		} else if len(data) > 0 {
			r.maskCompareTruncated = true
		}
	}
	if !masked && !r.storeRawBody {
		return
	}
	if masked {
		if int64(len(r.requestMasked))+int64(len(data)) > r.maxBytes {
			r.requestMaskedTruncated = true
		}
		r.append(&r.requestMasked, data)
		return
	}
	if int64(len(r.requestRaw))+int64(len(data)) > r.maxBytes {
		r.requestRawTruncated = true
	}
	r.append(&r.requestRaw, data)
}

func boundedLedgerBytes(data []byte, max int64) ([]byte, bool) {
	if max < 1 || int64(len(data)) <= max {
		return append([]byte(nil), data...), false
	}
	return append([]byte(nil), data[:int(max)]...), true
}

func (r *requestRecord) finish(status int, err error) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	if r.mappingProvider != nil {
		if items, itemsErr := r.mappingProvider.MaskedItems(); itemsErr == nil {
			r.maskedItems = items
			r.maskedItemsComplete = true
		}
	}
	captureComplete := r.capture.close()
	defer r.capture.release()
	r.item.Status = strconv.Itoa(status)
	if status == 0 {
		r.item.Status = "completed"
	}
	r.item.Error = ""
	if err != nil {
		r.item.Error = ledgerErrorCategory(err)
		if status == 499 {
			r.item.Status = "499"
		} else {
			r.item.Status = "error"
		}
	}
	r.item.Upstream = append([]byte(nil), r.upstream...)
	r.item.Restored = append([]byte(nil), r.restored...)
	if len(r.requestRaw) > 0 || len(r.requestMasked) > 0 || r.requestRawTruncated || r.requestMaskedTruncated {
		if r.storeRawBody {
			r.item.Raw = append([]byte(nil), r.requestRaw...)
		}
		r.item.Masked = append([]byte(nil), r.requestMasked...)
	}
	var meta map[string]any
	if r.item.Meta != "" {
		_ = json.Unmarshal([]byte(r.item.Meta), &meta)
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["http_status"] = status
	if err != nil && r.failureStage != "" {
		meta["failure_stage"] = r.failureStage
	}
	if r.requestBodyIncomplete {
		meta["request_body_incomplete"] = true
	}
	// Persist exact per-token originals for the user's masking audit view, never
	// the full raw request body. Native streaming sessions provide the exact
	// request-scoped map; the bounded comparison is only a conservative fallback.
	items := r.maskedItems
	if captureComplete && !r.maskedItemsComplete && r.maskedTokenCount > 0 {
		raw, rawErr := os.ReadFile(filepath.Join(r.capture.dir, "raw"))
		masked, maskedErr := os.ReadFile(filepath.Join(r.capture.dir, "masked"))
		if rawErr == nil && maskedErr == nil {
			items = maskedTokenItemsFromPair(raw, masked)
			r.maskedItemsComplete = true
		}
	}
	if len(items) == 0 {
		items = maskedTokenItemsFromPair(r.maskCompareRaw, r.maskCompareMasked, r.maskCompareTruncated)
	}
	if len(items) > 0 {
		meta["items"] = items
	}
	meta["mask_token_count"] = r.maskedTokenCount
	if r.maskedItemsComplete {
		meta["items_complete"] = true
		if meta["mapping_mode"] == "entity-v1" {
			r.maskedTokenCount = 0
			for _, item := range items {
				r.maskedTokenCount += maskedItemOccurrences(item)
			}
			meta["mask_token_count"] = r.maskedTokenCount
		}
	}
	if r.upstreamTruncated {
		meta["upstream_truncated"] = true
	}
	if r.restoredTruncated {
		meta["restored_truncated"] = true
	}
	if r.requestRawTruncated || r.requestMaskedTruncated {
		meta["body_truncated"] = true
	}
	meta["body_rewritten"] = r.maskInputBytes != r.maskOutputBytes || !bytes.Equal(r.maskCompareRaw, r.maskCompareMasked) || r.maskCompareTruncated
	maskedForReuse := r.requestMasked
	if len(maskedForReuse) == 0 {
		maskedForReuse = r.item.Masked
	}
	meta["suffix_reused"] = r.tokenReuse != nil && r.tokenReuse.observe(maskedForReuse)
	if firstDiff := firstDiffByte(r.maskCompareRaw, r.maskCompareMasked); firstDiff >= 0 && !r.maskCompareTruncated {
		meta["first_diff_byte"] = firstDiff
	} else {
		meta["first_diff_byte"] = -1
	}
	// Keep only accounting fields in the control-plane metadata. The response
	// bodies can contain sensitive restored content and must never be copied
	// into metadata.
	usage := r.responseUsage()
	requestUsage := extractUsage(r.item.Masked)
	if usage.Model == "" {
		usage.Model = requestUsage.Model
	}
	if usage.Found {
		meta["usage_found"] = true
		meta["usage_prompt_tokens"] = usage.Prompt
		meta["usage_prompt_observed"] = usage.PromptFound
		meta["usage_completion_tokens"] = usage.Completion
	}
	if usage.CacheFound {
		meta["usage_cache_observed"] = true
		meta["usage_cache_read_tokens"] = usage.CacheRead
		meta["usage_cache_write_tokens"] = usage.CacheWrite
		if usage.CacheRateFound {
			meta["usage_cache_rate_observed"] = true
			meta["usage_cache_input_tokens"] = usage.CacheTotal
		}
		if usage.AnthropicCacheShape {
			meta["usage_cache_anthropic_shape"] = true
		}
	}
	if usage.Model != "" {
		meta["model"] = usage.Model
	}
	if r.protocolCounts != nil {
		meta["restore_count"] = r.protocolCounts.Total
		meta["restore_restored"] = r.protocolCounts.Restored
		if err != nil {
			meta["restore_restored"] = 0
		}
		meta["restore_unresolved"] = r.protocolCounts.Unresolved
		meta["restore_evidence"] = r.protocolCounts.Tokens
		meta["restore_evidence_complete"] = r.protocolCounts.Complete && err == nil
	}
	if encoded, encodeErr := json.Marshal(meta); encodeErr == nil {
		r.item.Meta = string(encoded)
	}
	if r.capture != nil && r.capture.enabled {
		meta["body_store"] = captureComplete
		if captureComplete {
			delete(meta, "body_truncated")
			delete(meta, "upstream_truncated")
			delete(meta, "restored_truncated")
		} else {
			meta["body_storage_error"] = true
		}
		encoded, _ := json.Marshal(meta)
		r.item.Meta = string(encoded)
	}
	r.item.EndedAt = time.Now().UTC()
	item := r.item
	r.mu.Unlock()
	if updateErr := r.store.UpdateRequest(context.Background(), item); updateErr != nil {
		if r.metrics != nil {
			r.metrics.storageErrors.Add(1)
		}
		log.Printf("request ledger update: %v", updateErr)
	}
	r.appendOperationalEvents(item)
	if r.auditEnabled && r.auditPassive && r.auditErrorLeak {
		if finding, ok := passiveErrorLeakFinding(item.Status, item.Restored); ok && auditSeverityRank(finding.severity) >= auditSeverityRank(r.auditSeverityFloor) {
			eventMeta := map[string]any{"signal_type": "error_leak", "severity": finding.severity, "evidence": finding.evidence, "path": string(item.Payload)}
			var requestMeta map[string]any
			if json.Unmarshal([]byte(item.Meta), &requestMeta) == nil {
				if method, ok := requestMeta["method"].(string); ok {
					eventMeta["method"] = method
				}
			}
			meta, _ := json.Marshal(eventMeta)
			event := storage.Event{ID: newRequestID(), RequestID: item.ID, Type: "AUDIT_error_leak", Payload: []byte{}, Status: item.Status, Meta: string(meta), CreatedAt: item.EndedAt}
			if eventErr := r.store.AppendEvent(context.Background(), event); eventErr != nil {
				if r.metrics != nil {
					r.metrics.storageErrors.Add(1)
				}
				log.Printf("audit event insert: %v", eventErr)
			}
		}
	}
}

func firstDiffByte(a, b []byte) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return limit
	}
	return -1
}

func (r *requestRecord) appendOperationalEvents(item storage.Request) {
	if r.store == nil {
		return
	}
	var meta map[string]any
	if item.Meta != "" {
		_ = json.Unmarshal([]byte(item.Meta), &meta)
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["sid"] = item.ID
	if meta["ingress"] != "ext" && meta["ingress"] != "capture" {
		meta["ingress"] = "proxy"
	}
	maskCount := maskedTokenCount(item.Masked)
	if meta["items_complete"] == true {
		maskCount = 0
		if items, ok := meta["items"].([]any); ok {
			for _, raw := range items {
				if item, ok := raw.(map[string]any); ok {
					maskCount += maskedItemOccurrences(item)
				}
			}
		}
	} else if count, ok := numericMetaInt(meta["mask_token_count"]); ok {
		maskCount = count
	}
	meta["count"] = maskCount
	meta["mask_count"] = maskCount
	if !item.StartedAt.IsZero() && !item.EndedAt.Before(item.StartedAt) {
		meta["total_ms"] = float64(item.EndedAt.Sub(item.StartedAt)) / float64(time.Millisecond)
	}
	if _, exists := meta["items"]; !exists {
		meta["items"] = maskedTokenItemsFromPair(r.maskCompareRaw, r.maskCompareMasked, r.maskCompareTruncated)
	}
	meta["body_rewritten"] = meta["body_rewritten"] == true
	maskMeta, _ := json.Marshal(meta)
	maskCreated := item.StartedAt
	if maskCreated.IsZero() {
		maskCreated = time.Now().UTC()
	}
	if !r.maskEventRecorded {
		r.appendLogEvent(storage.Event{ID: newRequestID(), RequestID: item.ID, Type: "MASK", Payload: append([]byte(nil), item.Payload...), Status: item.Status, Error: item.Error, Meta: string(maskMeta), StartedAt: item.StartedAt, EndedAt: item.StartedAt, CreatedAt: maskCreated})
	}

	placeholders, restoredPlaceholders, unresolvedPlaceholders := restorePlaceholderCounts(item.Upstream, item.Restored)
	if count, ok := numericMetaInt(meta["restore_count"]); ok {
		placeholders = count
		restoredPlaceholders, _ = numericMetaInt(meta["restore_restored"])
		unresolvedPlaceholders, _ = numericMetaInt(meta["restore_unresolved"])
	}
	if item.Error != "" {
		restoredPlaceholders = 0
	}
	restoreStatus := "no_sensitive_data"
	if unresolvedPlaceholders > 0 {
		restoreStatus = "unresolved"
	} else if restoredPlaceholders > 0 {
		restoreStatus = "restored"
	} else if maskCount > 0 {
		restoreStatus = "no_placeholder_in_response"
	}
	restoreMeta := cloneMap(meta)
	delete(restoreMeta, "items")
	restoreMeta["count"] = placeholders
	restoreMeta["restored"] = restoredPlaceholders
	restoreMeta["unresolved"] = unresolvedPlaceholders
	restoreMeta["restore_status"] = restoreStatus
	if item.Error != "" || item.Status == "error" || ledgerHTTPStatus(item.Status) >= 400 {
		restoreStatus = item.Status
	}
	restoreMeta["status"] = restoreStatus
	restoreRaw, _ := json.Marshal(restoreMeta)
	restoreCreated := item.EndedAt
	if restoreCreated.IsZero() || !restoreCreated.After(maskCreated) {
		restoreCreated = maskCreated.Add(time.Microsecond)
	}
	r.appendLogEvent(storage.Event{ID: newRequestID(), RequestID: item.ID, Type: "RESTORE", Payload: append([]byte(nil), item.Payload...), Status: restoreStatus, Error: item.Error, Meta: string(restoreRaw), StartedAt: item.EndedAt, EndedAt: item.EndedAt, CreatedAt: restoreCreated})
}

func (r *requestRecord) appendLogEvent(event storage.Event) {
	if err := r.store.AppendEvent(context.Background(), event); err != nil {
		if r.metrics != nil {
			r.metrics.storageErrors.Add(1)
		}
		log.Printf("request event insert: %v", err)
	}
}

func maskedTokenCount(body []byte) int {
	return len(maskedTokenPattern.FindAll(body, -1))
}

// restorePlaceholderCounts compares the masked upstream response with the
// final response delivered to the client. A token still present in Restored is
// unresolved; only tokens that disappeared from the final body count as
// restored. This keeps logs aligned with the actual response rather than with
// the mere presence of a token in the upstream payload.
func restorePlaceholderCounts(upstream, restored []byte) (total, restoredCount, unresolved int) {
	total = maskedTokenCount(upstream)
	unresolved = maskedTokenCount(restored)
	if unresolved > total {
		unresolved = total
	}
	return total, total - unresolved, unresolved
}

func maskedTokenItems(body []byte) []map[string]any {
	matches := maskedTokenPattern.FindAllSubmatch(body, -1)
	items := make([]map[string]any, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			items = append(items, map[string]any{"label": string(match[1]), "preview": string(match[0]), "display": "placeholder"})
		}
	}
	return items
}

// maskedTokenItemsFromPair recovers the original value for each placeholder
// from the bounded raw/masked request comparison kept in requestRecord. The
// engine replaces sensitive spans without changing surrounding bytes, so the
// stable literal bytes before and after a token provide a deterministic span
// for the original value. When a span cannot be aligned (for example because
// the comparison was truncated), the item remains an explicit placeholder.
func maskedTokenItemsFromPair(raw, masked []byte, truncated ...bool) []map[string]any {
	items := maskedTokenItems(masked)
	if len(items) == 0 || len(raw) == 0 {
		return items
	}
	matches := maskedTokenPattern.FindAllSubmatchIndex(masked, -1)
	comparisonTruncated := len(truncated) > 0 && truncated[0]
	rawCursor, maskedCursor := 0, 0
	for i, match := range matches {
		if len(match) < 4 {
			continue
		}
		tokenStart, tokenEnd := match[0], match[1]
		// The comparison buffers retain only a bounded prefix. Without a
		// complete right-hand anchor, the remaining raw suffix is not known to
		// belong to this final token.
		if comparisonTruncated && i == len(matches)-1 {
			continue
		}
		prefix := masked[maskedCursor:tokenStart]
		prefixAt := bytes.Index(raw[rawCursor:], prefix)
		if prefixAt < 0 {
			break
		}
		rawCursor += prefixAt + len(prefix)

		nextStart := len(masked)
		if i+1 < len(matches) {
			nextStart = matches[i+1][0]
		}
		suffix := masked[tokenEnd:nextStart]
		originalEnd := len(raw)
		if len(suffix) > 0 {
			suffixAt := bytes.Index(raw[rawCursor:], suffix)
			if suffixAt < 0 {
				break
			}
			originalEnd = rawCursor + suffixAt
		} else if i+1 < len(matches) {
			// Adjacent placeholders have no literal anchor between them. Leave
			// the ambiguous entry marked as a placeholder instead of assigning
			// the whole remaining raw suffix to the wrong token.
			break
		}
		if originalEnd < rawCursor {
			break
		}
		original := string(raw[rawCursor:originalEnd])
		if original != "" {
			items[i]["original"] = original
			items[i]["display"] = "plaintext"
		}
		rawCursor = originalEnd
		maskedCursor = tokenEnd
	}
	return items
}

func ledgerErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	if code, ok := err.(extensionLogError); ok {
		return string(code)
	}
	if errors.Is(err, errUpstreamTimeout) {
		return "upstream_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, errRequestBodyTooLarge) {
		return "request_body_too_large"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	return "upstream_or_engine_error"
}

type passiveAuditFinding struct {
	severity string
	evidence string
}

var passiveCredentialPatterns = []struct {
	rx       *regexp.Regexp
	kind     string
	severity string
}{
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{20,}`), "bearer_token", "CRITICAL"},
	{regexp.MustCompile(`\b(?:sk|rk|xai|ghp|glpat)[-_][A-Za-z0-9._-]{12,}`), "key_token", "CRITICAL"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "aws_access_key", "CRITICAL"},
	{regexp.MustCompile(`-----BEGIN[A-Z -]{0,40}PRIVATE KEY-----`), "pem_private_key", "CRITICAL"},
}

func passiveErrorLeakFinding(status string, body []byte) (passiveAuditFinding, bool) {
	code, err := strconv.Atoi(status)
	if err != nil || code < 400 || len(body) == 0 {
		return passiveAuditFinding{}, false
	}
	for _, pattern := range passiveCredentialPatterns {
		match := pattern.rx.Find(body)
		if len(match) == 0 {
			continue
		}
		digest := sha256.Sum256(match)
		return passiveAuditFinding{severity: pattern.severity, evidence: fmt.Sprintf("%s len=%d sha256=%x", pattern.kind, len(match), digest[:8])}, true
	}
	return passiveAuditFinding{}, false
}

type recordingReadCloser struct {
	io.ReadCloser
	record      *requestRecord
	restored    bool
	status      int
	captureOnly bool
	ctx         context.Context
	eof         bool
	readErr     error
}

// maskingReadCloser feeds the upstream request body through a native masking
// session as ReverseProxy consumes it. It never returns source bytes directly;
// a masking error aborts the session before any further bytes can be forwarded.
type maskingReadCloser struct {
	mu          sync.Mutex
	readMu      sync.Mutex
	closeOnce   sync.Once
	closeErr    error
	source      io.ReadCloser
	session     MaskingSession
	ctx         context.Context
	record      *requestRecord
	buffer      []byte
	readChunk   []byte
	maxBytes    int64
	readBytes   int64
	done        bool
	terminalErr error
}

func (r *maskingReadCloser) Read(dst []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buffer) == 0 && !r.done {
		if cap(r.readChunk) < 32*1024 {
			r.readChunk = make([]byte, 32*1024)
		}
		chunk := r.readChunk[:32*1024]
		r.mu.Unlock()
		n, readErr := r.source.Read(chunk)
		r.mu.Lock()
		if r.done {
			return 0, io.EOF
		}
		if n > 0 {
			if r.maxBytes >= 0 && r.readBytes+int64(n) > r.maxBytes {
				r.abort()
				r.done = true
				r.terminalErr = errRequestBodyTooLarge
				if r.record != nil {
					r.record.finish(http.StatusRequestEntityTooLarge, errRequestBodyTooLarge)
				}
				return 0, errRequestBodyTooLarge
			}
			r.readBytes += int64(n)
			if r.record != nil {
				r.record.mu.Lock()
				r.record.appendRequest(false, chunk[:n])
				r.record.mu.Unlock()
			}
			out, err := r.session.Push(r.ctx, chunk[:n])
			if err != nil {
				r.abort()
				r.done = true
				r.terminalErr = err
				if r.record != nil {
					status := statusForReadError(http.StatusBadGateway, err)
					if errors.Is(err, errRequestBodyTooLarge) {
						status = http.StatusRequestEntityTooLarge
					}
					r.record.finish(status, err)
				}
				return 0, err
			}
			r.buffer = append(r.buffer, out...)
			if r.record != nil {
				r.record.mu.Lock()
				r.record.appendRequest(true, out)
				r.record.mu.Unlock()
			}
		}
		if readErr == io.EOF {
			out, err := r.session.Finish(r.ctx)
			if err != nil {
				r.abort()
				r.done = true
				r.terminalErr = err
				if r.record != nil {
					r.record.finish(statusForReadError(http.StatusBadGateway, err), err)
				}
				return 0, err
			}
			if provider, ok := r.session.(MaskedItemsProvider); ok && r.record != nil {
				if items, itemsErr := provider.MaskedItems(); itemsErr == nil {
					r.record.mu.Lock()
					r.record.maskedItems = items
					r.record.maskedItemsComplete = true
					r.record.mu.Unlock()
				}
			}
			r.buffer = append(r.buffer, out...)
			if r.record != nil {
				r.record.mu.Lock()
				r.record.appendRequest(true, out)
				r.record.mu.Unlock()
			}
			r.done = true
		} else if readErr != nil {
			r.abort()
			r.done = true
			r.terminalErr = readErr
			if r.record != nil {
				r.record.finish(statusForReadError(http.StatusBadGateway, readErr), readErr)
			}
			return 0, readErr
		}
	}
	if len(r.buffer) == 0 {
		return 0, io.EOF
	}
	n := copy(dst, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

// stopForResponse never waits for a blocked client upload. The next Read
// discards any bytes it receives after this point. Push/Finish and Seal share mu.
func (r *maskingReadCloser) stopForResponse() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.done {
		if r.record != nil {
			r.record.mu.Lock()
			r.record.requestBodyIncomplete = true
			r.record.mu.Unlock()
		}
		if sealer, ok := r.session.(interface{ Seal() error }); ok {
			r.terminalErr = sealer.Seal()
			if r.terminalErr != nil {
				r.abort()
			}
		} else {
			r.abort()
		}
		r.done = true
	}
	r.buffer = nil
	return r.terminalErr
}
func (r *maskingReadCloser) Close() error {
	err := r.stopForResponse()
	r.closeOnce.Do(func() { r.closeErr = r.source.Close() })
	if err == nil {
		err = r.closeErr
	}
	if err == nil && r.ctx != nil {
		err = r.ctx.Err()
	}
	return err
}

func (r *maskingReadCloser) abort() {
	if aborter, ok := r.session.(AbortableMaskingSession); ok {
		aborter.Abort()
	}
}

type metricResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	record *requestRecord
}

func (w *metricResponseWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status = status
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *metricResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.recordWriteError(err, "downstream_write")
	return n, err
}

func (w *metricResponseWriter) recordWriteError(err error, stage string) {
	if err != nil && w.record != nil {
		w.record.noteFailure(stage)
		w.record.mu.Lock()
		if w.record.responseWriteErr == nil {
			w.record.responseWriteErr = err
		}
		w.record.mu.Unlock()
	}
}
func (w *metricResponseWriter) Flush() {
	_ = w.FlushError()
}
func (w *metricResponseWriter) FlushError() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		w.recordWriteError(err, "downstream_flush")
	}
	return err
}
func (w *metricResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking unsupported")
	}
	return h.Hijack()
}
func (w *metricResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *metricResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.recordWriteError(err, "downstream_write")
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.recordWriteError(err, "downstream_write")
	return n, err
}
func (w *metricResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (r *recordingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.record != nil {
		r.record.mu.Lock()
		r.record.appendResponse(r.restored, p[:n])
		r.record.mu.Unlock()
	}
	if err == io.EOF {
		r.eof = true
	} else if err != nil {
		r.readErr = err
		if !r.restored {
			r.record.noteFailure("upstream_read")
		}
	}
	return n, err
}
func (r *recordingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.record != nil && !r.captureOnly {
		if r.readErr != nil {
			err = r.readErr
		}
		r.record.mu.Lock()
		writeErr := r.record.responseWriteErr
		stage := r.record.failureStage
		r.record.mu.Unlock()
		if writeErr != nil {
			err = writeErr
		}
		if err == nil && !r.eof {
			err = r.recordContextErr()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
		}
		if err != nil {
			r.record.noteFailure("response_close")
		}
		status := statusForReadError(r.status, err)
		if writeErr == nil && r.readErr != nil && stage == "upstream_read" {
			ctx := r.ctx
			if restored, ok := r.ReadCloser.(*restoreReadCloser); ok {
				ctx = restored.ctx
			}
			if ctx == nil {
				ctx = context.Background()
			}
			status, err = proxyFailure(ctx, err)
		}
		r.record.finish(status, err)
	}
	return err
}

func (r *recordingReadCloser) recordContextErr() error {
	if restored, ok := r.ReadCloser.(*restoreReadCloser); ok && restored.ctx != nil {
		return restored.ctx.Err()
	}
	if r.ctx != nil {
		return r.ctx.Err()
	}
	return nil
}

func statusForReadError(status int, err error) int {
	if err == nil {
		return status
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 499
	}
	return http.StatusBadGateway
}

type atomicReady struct {
	mu sync.RWMutex
	ok bool
}

func (r *atomicReady) set(value bool) { r.mu.Lock(); r.ok = value; r.mu.Unlock() }
func (r *atomicReady) get() bool      { r.mu.RLock(); defer r.mu.RUnlock(); return r.ok }

// NewServer creates an HTTP handler. An empty upstream is allowed so health and
// control endpoints remain available; data-plane requests return 503 until one
// is configured.
func NewServer(cfg Config, engine Engine) (*Server, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultConfig().ListenAddr
	}
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = DefaultConfig().MaxConcurrency
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = DefaultConfig().MaxBodyBytes
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = DefaultConfig().MaxResponseBytes
	}
	if cfg.UpstreamDialTimeoutSeconds == 0 {
		cfg.UpstreamDialTimeoutSeconds = DefaultConfig().UpstreamDialTimeoutSeconds
	}
	if cfg.UpstreamHeaderTimeoutSeconds == 0 {
		cfg.UpstreamHeaderTimeoutSeconds = DefaultConfig().UpstreamHeaderTimeoutSeconds
	}
	if cfg.MaxResponseBytes < 1 {
		return nil, errors.New("max response bytes must be positive")
	}
	if cfg.MaxConcurrency < 1 {
		return nil, errors.New("max concurrency must be positive")
	}
	if cfg.UpstreamDialTimeoutSeconds < 1 || cfg.UpstreamHeaderTimeoutSeconds < 1 {
		return nil, errors.New("upstream timeouts must be positive")
	}
	if cfg.Engine == "" {
		cfg.Engine = "cpp"
	}
	if engine == nil {
		return nil, errors.New("engine is required; refusing to start without the C++ masking engine")
	}
	controlPath := strings.TrimSpace(os.Getenv("SILENCER_CONTROL_CONFIG_FILE"))
	if controlPath == "" {
		if configFile := strings.TrimSpace(os.Getenv("SILENCER_CONFIG_FILE")); configFile != "" {
			controlPath = configFile + ".control.json"
		}
	}
	anchor, _, _, pathErr := configParticipantPaths(runtimeConfigPath(), controlPath)
	if pathErr != nil {
		return nil, pathErr
	}
	diskLock, diskErr := lockConfigDisk(anchor)
	if diskErr != nil {
		return nil, diskErr
	}
	if diskLock != nil {
		defer diskLock.Close()
	}
	if diskErr = recoverConfigJournal(anchor, controlPath); diskErr != nil {
		return nil, diskErr
	}
	if controlPath != "" && configPathIdentity(controlPath) != anchor {
		if diskErr = recoverConfigJournal(configPathIdentity(controlPath)); diskErr != nil {
			return nil, diskErr
		}
	}
	s := &Server{cfg: cfg, engine: engine, configPath: controlPath, limiter: make(chan struct{}, cfg.MaxConcurrency), logger: log.Default(), started: time.Now(), control: defaultControlConfig(), tokenReuse: newTokenReuseTracker(), extRestore: make(map[string]*extensionRestoreEntry), auditJob: map[string]any{"running": false, "capability": "host_probe_unavailable"}}
	s.extensionDisabled = os.Getenv("SILENCER_EXTENSION_DISABLED") == "1"
	needsControlPersist := false
	if controlPath != "" {
		if data, err := os.ReadFile(controlPath); err == nil {
			var persisted map[string]any
			if err := json.Unmarshal(data, &persisted); err != nil || persisted == nil {
				return nil, errors.New("invalid persisted control configuration")
			} else {
				if _, present := persisted["builtin_rules"]; present && ensureBuiltinRuleInventory(persisted) {
					needsControlPersist = true
				}
				mergeControlConfig(s.control, persisted)
				// Persist newly introduced control fields so an upgrade makes the
				// complete rule inventory visible and durable immediately.
				for _, key := range []string{"sensitive", "sensitive_disabled", "sensitive_word_disabled", "sensitive_word_whole", "secret_prefixes", "builtin_rules", "record_detail_data", "log_retention_days", "log_noise_allowlist", "_meta"} {
					if _, present := persisted[key]; !present {
						needsControlPersist = true
					}
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read control config: %w", err)
		}
	}
	beforeExtensionToken, _ := s.control["ext_token"].(string)
	if err := validateToolGuard(s.control); err != nil {
		return nil, err
	}
	if s.extensionDisabled {
		if enabled, exists := s.control["ext_bridge_enabled"]; !exists || enabled != false {
			needsControlPersist = true
		}
		s.control["ext_bridge_enabled"] = false
	}
	if err := s.ensureExtensionTokenLocked(); err != nil {
		return nil, err
	}
	if beforeExtensionToken != s.control["ext_token"] {
		needsControlPersist = true
	}
	if needsControlPersist {
		if err := s.persistControlLocked(); err != nil {
			return nil, fmt.Errorf("persist extension token: %w", err)
		}
	}
	// Rebuild the native handle from the persisted control document only when
	// a control sidecar is configured. Tests and embedders may intentionally
	// provide a custom Engine implementation while leaving cfg.Engine at its
	// production default; attempting to replace that engine would incorrectly
	// require the C++ shared library in those environments.
	if (cfg.Engine == "cpp" || cfg.Engine == "native") && controlPath != "" {
		raw, marshalErr := engineConfigFromControl(s.control)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode persisted engine rules: %w", marshalErr)
		}
		if raw != "" {
			cfg.EngineConfig = raw
			rebuilt, rebuildErr := NewEngine(cfg)
			if rebuildErr != nil {
				return nil, fmt.Errorf("load persisted engine rules: %w", rebuildErr)
			}
			if closeable, ok := engine.(CloseableEngine); ok {
				closeable.Close()
			}
			engine = rebuilt
			s.cfg.EngineConfig = cfg.EngineConfig
			s.engine = rebuilt
		}
	}
	s.proxyTransport = newProxyTransport(cfg)
	s.ready.set(true)
	if strings.TrimSpace(cfg.UpstreamURL) != "" {
		u, err := url.Parse(cfg.UpstreamURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			if err == nil {
				err = errors.New("upstream URL must include scheme and host")
			}
			return nil, err
		}
		s.proxy = newReverseProxy(u, s)
		s.running = true
	}
	if hasConfiguredUpstream(s.control) {
		s.running = true
	}
	if err := s.initCapture(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) Config() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.cfg
}
func (s *Server) Engine() Engine { s.engineMu.RLock(); defer s.engineMu.RUnlock(); return s.engine }
func (s *Server) currentEngine() Engine {
	s.engineMu.RLock()
	defer s.engineMu.RUnlock()
	return s.engine
}

func (s *Server) replaceEngine(e Engine) bool {
	s.lifecycleMu.Lock()
	if s.shuttingDown {
		s.lifecycleMu.Unlock()
		if closeable, ok := e.(CloseableEngine); ok {
			closeable.Close()
		}
		return false
	}
	defer s.lifecycleMu.Unlock()
	s.installEngineLocked(e)
	return true
}

// Caller holds lifecycleMu so shutdown cannot interrupt a configuration commit.
func (s *Server) installEngineLocked(e Engine) {
	s.engineMu.Lock()
	old := s.engine
	s.engine = e
	s.engineMu.Unlock()
	if closeable, ok := old.(CloseableEngine); ok && old != nil {
		done := s.requests.drained()
		entry := &retiredEngine{closeable: closeable}
		s.retiredMu.Lock()
		s.retired = append(s.retired, entry)
		s.retiredMu.Unlock()
		go func() {
			<-done
			s.retiredMu.Lock()
			removed := false
			for i, candidate := range s.retired {
				if candidate == entry {
					s.retired = append(s.retired[:i], s.retired[i+1:]...)
					removed = true
					break
				}
			}
			s.retiredMu.Unlock()
			if removed {
				closeable.Close()
			}
		}()
	}
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.ServeHTTP) }
func (s *Server) SetStorage(store *storage.Storage) {
	// Storage is attached before serving requests. Serialize replacement with
	// retention shutdown so the old worker never observes the new store.
	s.logRetentionMu.Lock()
	defer s.logRetentionMu.Unlock()
	s.lifecycleMu.Lock()
	stopping := s.shuttingDown
	s.lifecycleMu.Unlock()
	if stopping {
		return
	}
	if s.logCancel != nil {
		s.logCancel()
		<-s.logDone
	}
	s.logCancel, s.logDone = nil, nil
	s.logWakeMu.Lock()
	s.logWake = nil
	s.logWakeMu.Unlock()
	s.logSweepMu.Lock()
	if s.logOrphanDir != nil {
		s.logOrphanDir.Close()
		s.logOrphanDir = nil
	}
	s.store = store
	s.logSweepMu.Unlock()
	if store != nil {
		s.warmReusableMappings()
		s.startLogRetention()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := otel.Tracer(tracerName).Start(ctx, "HTTP "+r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("http.request.method", r.Method),
		attribute.String("url.path", r.URL.Path),
	))
	r = r.WithContext(ctx)
	started := time.Now()
	mw := &metricResponseWriter{ResponseWriter: w}
	w = mw
	defer func() {
		status := mw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.metrics.observe(status, time.Since(started))
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		span.End()
	}()
	s.metrics.requests.Add(1)
	s.lifecycleMu.Lock()
	if s.shuttingDown {
		s.lifecycleMu.Unlock()
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "gateway_shutting_down"})
		return
	}
	s.requests.begin()
	s.lifecycleMu.Unlock()
	defer s.requests.end()
	if s.Config().LocalMode && !validLocalCaptureOrigin(r) {
		s.captureError(w, 403, "origin_rejected")
		return
	}
	if s.handleCaptureRoute(w, r) {
		return
	}
	if s.extensionDisabled && (isExtensionAPIPath(r.URL.Path) || strings.HasPrefix(r.URL.Path, "/downloads/")) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "extension_unavailable"})
		return
	}
	if !s.ready.get() && strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/auth/status" {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "gateway_shutting_down"})
		return
	}
	if s.store != nil && strings.HasPrefix(r.URL.Path, requestPrefix+"/") && r.Method == http.MethodGet && isRequestIDPath(r.URL.Path) {
		if !s.authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		limiter, ok := s.acquire(w)
		if !ok {
			return
		}
		defer s.release(limiter)
		s.handleRequestDetail(w, r)
		return
	}
	dataPlane := isDataPlanePath(r.URL.Path)
	// The exact /api/requests path is the control-plane ledger listing on
	// GET; other methods may still proxy an upstream root request there.
	if r.URL.Path == requestPrefix && r.Method != http.MethodGet {
		dataPlane = true
	}
	if dataPlane {
		// Management credentials protect the console only. Proxy clients keep
		// their provider credentials, which are authenticated by the upstream.
		limiter, ok := s.acquire(w)
		if !ok {
			return
		}
		defer s.release(limiter)
		s.handleProxy(w, r)
		return
	}
	switch r.URL.Path {
	case "/healthz":
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/readyz":
		ready := s.ready.get()
		storageOK := true
		storageError := ""
		var storageSnapshot storage.Snapshot
		if s.store != nil {
			if err := s.store.Health(r.Context()); err != nil {
				storageOK = false
				storageError = "unavailable"
			}
			storageSnapshot = s.store.Snapshot()
			ready = ready && storageOK
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		s.configMu.RLock()
		upstreamConfigured := s.proxy != nil || hasConfiguredUpstream(s.control)
		requireUpstream := s.cfg.RequireUpstream
		s.configMu.RUnlock()
		if requireUpstream && !upstreamConfigured {
			ready = false
			status = http.StatusServiceUnavailable
		}
		response := map[string]any{"ok": ready, "upstream_configured": upstreamConfigured, "require_upstream": requireUpstream, "storage_ok": storageOK}
		if s.store != nil {
			response["storage"] = storageHealthPayload(storageSnapshot)
			if storageError != "" {
				response["storage_error"] = storageError
			}
		}
		s.writeJSON(w, status, response)
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, s.metrics.text())
		if s.store != nil {
			_, _ = io.WriteString(w, s.store.MetricsText())
		}
	case "/api/auth/status":
		s.handleAuthStatus(w, r)
	case "/api/auth/login", "/api/auth/setup", "/api/auth/logout", "/api/auth/change-password":
		if r.URL.Path == "/api/auth/change-password" && !s.authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		s.handleAuthSession(w, r)
	case "/api/config":
		if !s.authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		limiter, ok := s.acquire(w)
		if !ok {
			return
		}
		defer s.release(limiter)
		s.handleConfig(w, r)
	case "/api/requests":
		if !s.authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		limiter, ok := s.acquire(w)
		if !ok {
			return
		}
		defer s.release(limiter)
		s.handleRequests(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !s.authorized(r) {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			limiter, ok := s.acquire(w)
			if !ok {
				return
			}
			defer s.release(limiter)
			s.handleControl(w, r)
			return
		}
		s.serveSPA(w, r)
	}
}

func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || s.cfg.WebDist == "" {
		http.NotFound(w, r)
		return
	}
	root := filepath.Clean(s.cfg.WebDist)
	path := filepath.Join(root, filepath.Clean("/"+r.URL.Path))
	if strings.HasPrefix(path, root+string(os.PathSeparator)) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			http.ServeFile(w, r, path)
			return
		}
	}
	if filepath.Ext(r.URL.Path) != "" {
		http.NotFound(w, r)
		return
	}
	index := filepath.Join(root, "index.html")
	if _, err := os.Stat(index); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, index)
}

func (s *Server) authorized(r *http.Request) bool {
	// Extension handlers perform their own bridge-token check so they can
	// return a stable 403 error instead of the management API's 401 response.
	if isExtensionAPIPath(r.URL.Path) {
		return true
	}
	return s.managementAuthorized(r)
}

func (s *Server) managementAuthorized(r *http.Request) bool {
	s.configMu.RLock()
	token := s.cfg.AuthToken
	s.configMu.RUnlock()
	if token == "" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("X-Silencer-Token"))
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(provided), "bearer ") {
			provided = strings.TrimSpace(provided[len("Bearer "):])
		}
	}
	if provided == "" {
		if cookie, err := r.Cookie("silencer_session"); err == nil {
			provided = cookie.Value
		}
	}
	return len(provided) == len(token) && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func isExtensionAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/ext/")
}

func (s *Server) acquire(w http.ResponseWriter) (chan struct{}, bool) {
	s.limiterMu.RLock()
	limiter := s.limiter
	s.limiterMu.RUnlock()
	select {
	case limiter <- struct{}{}:
		s.metrics.inFlight.Add(1)
		return limiter, true
	default:
		s.metrics.rejected.Add(1)
		http.Error(w, `{"error":"concurrency_limit"}`, http.StatusTooManyRequests)
		return nil, false
	}
}
func (s *Server) release(limiter chan struct{}) { <-limiter; s.metrics.inFlight.Add(-1) }

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	requestStarted := time.Now().UTC()
	s.configMu.RLock()
	proxy, running, cfg := s.proxy, s.running, s.cfg
	s.configMu.RUnlock()
	dispatch, captured := r.Context().Value(captureDispatchKey{}).(*captureDispatch)
	if captured {
		proxy, running = dispatch.proxy, true
	}
	if routeProxy, named, err := s.namedRouteProxy(r.URL.Path); !captured && named {
		if err != nil {
			// A global upstream still accepts an arbitrary named route as a
			// path alias. When no global target exists, an unknown named route
			// is a real configuration error and must be reported.
			if proxy == nil {
				if strings.Contains(err.Error(), "invalid_upstream") {
					http.Error(w, `{"error":"invalid_upstream_url"}`, http.StatusBadGateway)
				} else {
					http.Error(w, `{"error":"upstream_not_found"}`, http.StatusNotFound)
				}
				return
			}
		} else {
			proxy = routeProxy
		}
	}
	if proxy == nil || !running {
		http.Error(w, `{"error":"upstream_not_configured"}`, http.StatusServiceUnavailable)
		return
	}
	otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(r.Header))
	// Every request carrying a body, including chunked requests (ContentLength == -1),
	// must pass through the masking engine before proxying. Native engines expose
	// an incremental session; engines used by focused tests may still provide the
	// bounded one-shot interface and take the fallback path below.
	var rawBody, maskedBody []byte
	var streamingBody *maskingReadCloser
	var contextualMasker ContextualMaskingSession
	mappingScope := ""
	if r.Body != nil && r.Body != http.NoBody {
		masker, ok := s.currentEngine().(MaskingEngine)
		if !ok {
			s.metrics.proxyErrors.Add(1)
			http.Error(w, `{"error":"engine_unavailable"}`, http.StatusBadGateway)
			return
		}
		encoding := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Encoding")))
		var source io.ReadCloser = r.Body
		switch encoding {
		case "", "identity":
		case "gzip", "deflate", "br":
			reader, decodeErr := newDecodedBody(r.Body, encoding)
			if decodeErr != nil {
				s.metrics.maskErrors.Add(1)
				http.Error(w, `{"error":"request_body_unreadable"}`, http.StatusBadRequest)
				return
			}
			source = reader
			r.Header.Del("Content-Encoding")
		default:
			s.metrics.maskErrors.Add(1)
			http.Error(w, `{"error":"unsupported_request_encoding"}`, http.StatusUnsupportedMediaType)
			return
		}
		if encoding != "gzip" && encoding != "deflate" && encoding != "br" && r.ContentLength >= 0 && r.ContentLength > cfg.MaxBodyBytes {
			_ = source.Close()
			http.Error(w, `{"error":"request_body_too_large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		if streamer, supportsStreaming := s.currentEngine().(StreamingMaskingEngine); supportsStreaming {
			var session MaskingSession
			var sessionErr error
			if durable, ok := streamer.(DurableMaskingEngine); ok && s.store != nil {
				mappingScope = requestMappingScope(r)
				session, sessionErr = durable.NewDurableMaskingSession(r.Context(), mappingScope, s.store)
			} else if reusable, ok := streamer.(ReusableMaskingEngine); ok {
				mappingScope = requestMappingScope(r)
				session, sessionErr = reusable.NewReusableMaskingSession(r.Context(), mappingScope)
			} else {
				session, sessionErr = streamer.NewMaskingSession(r.Context())
			}
			if sessionErr != nil {
				_ = source.Close()
				s.metrics.proxyErrors.Add(1)
				s.metrics.maskErrors.Add(1)
				s.logger.Printf("request masking stream setup error: %v", sessionErr)
				http.Error(w, `{"error":"engine_unavailable"}`, http.StatusBadGateway)
				return
			}
			if contextual, ok := session.(ContextualMaskingSession); ok {
				contextualMasker = contextual
				r = r.WithContext(context.WithValue(r.Context(), contextualMaskingSessionContextKey{}, contextual))
			}
			streamingBody = &maskingReadCloser{source: source, session: session, ctx: r.Context(), maxBytes: cfg.MaxBodyBytes}
			r.Body = streamingBody
			r = r.WithContext(context.WithValue(r.Context(), maskingBodyContextKey{}, streamingBody))
			r.ContentLength = -1
			r.Header.Del("Content-Length")
			s.metrics.maskRequests.Add(1)
		} else {
			body, readErr := readRequestBody(r.Context(), source, cfg.MaxBodyBytes)
			closeErr := source.Close()
			if readErr != nil || closeErr != nil {
				if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) || errors.Is(closeErr, context.Canceled) || errors.Is(closeErr, context.DeadlineExceeded) {
					return
				}
				s.metrics.maskErrors.Add(1)
				if errors.Is(readErr, errRequestBodyTooLarge) {
					http.Error(w, `{"error":"request_body_too_large"}`, http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, `{"error":"request_body_unreadable"}`, http.StatusBadRequest)
				}
				return
			}
			masked, maskErr := masker.Mask(r.Context(), body)
			s.metrics.maskRequests.Add(1)
			if maskErr != nil {
				s.metrics.proxyErrors.Add(1)
				s.metrics.maskErrors.Add(1)
				s.logger.Printf("request masking error: %v", maskErr)
				http.Error(w, `{"error":"engine_unavailable"}`, http.StatusBadGateway)
				return
			}
			rawBody = append([]byte(nil), body...)
			maskedBody = append([]byte(nil), masked...)
			r.Body = io.NopCloser(bytes.NewReader(masked))
			r.ContentLength = int64(len(masked))
			r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
		}
	}
	// Bodyless requests can also return references from provider-side history.
	if contextualMasker == nil && (r.Body == nil || r.Body == http.NoBody) && s.store != nil {
		if durable, ok := s.currentEngine().(DurableMaskingEngine); ok {
			mappingScope = requestMappingScope(r)
			session, err := durable.NewDurableMaskingSession(r.Context(), mappingScope, s.store)
			if err == nil {
				_, err = session.Finish(r.Context())
			}
			if err != nil {
				if owner, ok := session.(ContextualMaskingSession); ok {
					owner.Release()
				}
				http.Error(w, `{"error":"engine_unavailable"}`, http.StatusBadGateway)
				return
			}
			contextualMasker = session
			r = r.WithContext(context.WithValue(r.Context(), contextualMaskingSessionContextKey{}, contextualMasker))
		}
	}
	if contextualMasker != nil {
		defer contextualMasker.Release()
	}
	if s.store != nil {
		requestID := newRequestID()
		started := requestStarted
		var ledgerRaw []byte
		rawTruncated := false
		recordDetailData := cfg.StoreRawBody || s.recordDetailDataEnabled()
		previewLimit := cfg.MaxResponseBytes
		if recordDetailData && previewLimit > 64<<10 {
			previewLimit = 64 << 10
		}
		if recordDetailData {
			ledgerRaw, rawTruncated = boundedLedgerBytes(rawBody, previewLimit)
		}
		ledgerMasked, maskedTruncated := boundedLedgerBytes(maskedBody, previewLimit)
		metadata := map[string]any{"method": r.Method, "path": r.URL.Path, "protocol": r.Proto, "ingress": "proxy", "body_truncated": rawTruncated || maskedTruncated}
		if captured {
			metadata["ingress"] = "capture"
			metadata["target"] = dispatch.origin
		}
		if mappingScope != "" {
			metadata["mapping_scope"], metadata["mapping_mode"] = mappingScope, "entity-v1"
		}
		meta, _ := json.Marshal(metadata)
		auditEnabled, auditPassive, auditErrorLeak, auditFloor := s.auditSettings()
		record := &requestRecord{store: s.store, metrics: &s.metrics, tokenReuse: s.tokenReuse, maxBytes: cfg.MaxResponseBytes, storeRawBody: recordDetailData, auditEnabled: auditEnabled, auditPassive: auditPassive, auditErrorLeak: auditErrorLeak, auditSeverityFloor: auditFloor, item: storage.Request{ID: requestID, Payload: []byte(r.Method + " " + r.URL.Path), Raw: ledgerRaw, Masked: ledgerMasked, Status: "running", Meta: string(meta), StartedAt: started, CreatedAt: started}}
		record.maskedTokenCount = maskedTokenCount(maskedBody)
		record.maskInputBytes = int64(len(rawBody))
		record.maskOutputBytes = int64(len(maskedBody))
		if len(rawBody) <= 64<<10 && len(maskedBody) <= 64<<10 {
			record.maskCompareRaw = append([]byte(nil), rawBody...)
			record.maskCompareMasked = append([]byte(nil), maskedBody...)
		} else {
			record.maskCompareTruncated = true
		}
		// Every live request holds a lease, even when full body recording is off.
		// Acquire it before inserting the running row so cleanup cannot race it.
		record.capture = newRequestCapture(s.logDirectory(), requestID, recordDetailData)
		if record.capture.err != nil {
			record.capture.discard()
			if streamingBody != nil {
				_ = streamingBody.Close()
			}
			s.metrics.storageErrors.Add(1)
			http.Error(w, `{"error":"log_storage_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if err := s.store.AppendRequest(r.Context(), record.item); err != nil {
			record.capture.discard()
			if streamingBody != nil {
				_ = streamingBody.Close()
			}
			s.metrics.proxyErrors.Add(1)
			s.metrics.storageErrors.Add(1)
			s.logger.Printf("request ledger insert: %v", err)
			http.Error(w, `{"error":"storage_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if recordDetailData {
			record.capture.append("raw", rawBody)
			record.capture.append("masked", maskedBody)
			record.maxBytes = cfg.MaxResponseBytes
			if record.maxBytes > 64<<10 {
				record.maxBytes = 64 << 10
			}
		}
		if streamingBody != nil {
			streamingBody.record = record
			if provider, ok := contextualMasker.(MaskedItemsProvider); ok {
				record.mappingProvider = provider
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), requestRecordContextKey{}, record))
		r.Header.Set("X-Silencer-Request-ID", requestID)
		w = &metricResponseWriter{ResponseWriter: w, record: record}
	}
	s.metrics.proxyRequests.Add(1)
	proxy.ServeHTTP(w, r)
}

// namedRouteProxy resolves /r/<configured-name>/... at request time so control
// plane edits take effect without rebuilding the HTTP server or proxy object.
func (s *Server) namedRouteProxy(path string) (*httputil.ReverseProxy, bool, error) {
	name := namedRouteName(path)
	if name == "" {
		return nil, false, nil
	}
	s.configMu.RLock()
	configured, ok := configuredUpstream(s.control, name)
	s.configMu.RUnlock()
	if !ok || strings.TrimSpace(configured.Target) == "" {
		return nil, true, errors.New("upstream_not_found")
	}
	target, err := url.Parse(strings.TrimSpace(configured.Target))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, true, errors.New("invalid_upstream_url")
	}
	target.Path = joinURLPath(target.Path, configured.BasePath)
	target.RawQuery = ""
	proxy := newReverseProxy(target, s)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		applyUpstreamHeaders(req, configured.ExtraHeaders)
		director(req)
	}
	return proxy, true, nil
}

func (s *Server) auditSettings() (enabled, passive, errorLeak bool, floor string) {
	floor = "MEDIUM"
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	audit, _ := s.control["audit"].(map[string]any)
	enabled, _ = audit["enabled"].(bool)
	passive, _ = audit["passive"].(bool)
	floor, _ = audit["severity_floor"].(string)
	if floor == "" {
		floor = "MEDIUM"
	}
	signals, _ := audit["signals"].(map[string]any)
	errorLeak, _ = signals["error_leak"].(bool)
	return enabled, passive, errorLeak, strings.ToUpper(floor)
}

var errRequestBodyTooLarge = errors.New("request body too large")

func readRequestBody(ctx context.Context, body io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		max = 0
	}
	limited := io.LimitReader(body, max+1)
	var out bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		n, err := limited.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			if int64(out.Len()) > max {
				return nil, errRequestBodyTooLarge
			}
		}
		if err != nil {
			if err == io.EOF {
				return out.Bytes(), nil
			}
			return nil, err
		}
	}
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return fmt.Sprintf("%x", raw[:])
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	s.configMu.RLock()
	required := s.cfg.AuthToken != ""
	username := strings.TrimSpace(s.cfg.AuthUsername)
	s.configMu.RUnlock()
	authenticated := s.authorized(r) || !required
	if username == "" {
		username = "admin"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"initialized": true, "authenticated": authenticated, "required": required, "username": username})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	post := r.Method == http.MethodPost
	if r.Method == http.MethodPost {
		var patch map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, s.cfg.MaxBodyBytes)).Decode(&patch); err != nil {
			http.Error(w, `{"error":"invalid_json"}`, http.StatusBadRequest)
			return
		}
		if err := validateLogSettings(patch); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		s.configMu.Lock()
		candidate := cloneMap(s.control)
		mergeControlConfig(candidate, patch)
		rebuild := false
		for key := range patch {
			rebuild = rebuild || engineConfigChanged(key)
		}
		configErr := s.commitConfigLocked(s.cfg, candidate, rebuild)
		s.configMu.Unlock()
		if configErr == nil {
			s.wakeLogRetention()
		}
		if configErr != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": configErr.Error()})
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	// AuthToken is deliberately omitted from the public configuration response.
	s.configMu.RLock()
	cfg := struct {
		ListenAddr                   string `json:"listen_addr"`
		UpstreamURL                  string `json:"upstream_url"`
		MaxConcurrency               int    `json:"max_concurrency"`
		MaxResponseBytes             int64  `json:"max_response_bytes"`
		UpstreamDialTimeoutSeconds   int    `json:"upstream_dial_timeout_seconds"`
		UpstreamHeaderTimeoutSeconds int    `json:"upstream_header_timeout_seconds"`
		Engine                       string `json:"engine"`
	}{s.cfg.ListenAddr, s.cfg.UpstreamURL, s.cfg.MaxConcurrency, s.cfg.MaxResponseBytes, s.cfg.UpstreamDialTimeoutSeconds, s.cfg.UpstreamHeaderTimeoutSeconds, s.cfg.Engine}
	response := cloneMap(s.control)
	s.configMu.RUnlock()
	response["listen_addr"] = cfg.ListenAddr
	response["upstream_url"] = cfg.UpstreamURL
	response["max_concurrency"] = cfg.MaxConcurrency
	response["max_response_bytes"] = cfg.MaxResponseBytes
	response["upstream_dial_timeout_seconds"] = cfg.UpstreamDialTimeoutSeconds
	response["upstream_header_timeout_seconds"] = cfg.UpstreamHeaderTimeoutSeconds
	response["engine"] = cfg.Engine
	if post {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": response, "warnings": []string{}, "proxy_restarted": false})
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if s.store != nil {
		outcome := r.URL.Query().Get("outcome")
		switch outcome {
		case "", "all", "abnormal", "block", "restore_failed", "error", "response_warning":
		default:
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_outcome"})
			return
		}
		ingress := r.URL.Query().Get("ingress")
		if ingress != "" && ingress != "proxy" && ingress != "ext" && ingress != "capture" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_ingress"})
			return
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 500 {
				s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_limit"})
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		rules := s.logNoiseRules()
		out := make([]map[string]any, 0, limit+1)
		var before time.Time
		var beforeID string
		const pageSize = 200
		for len(out) <= limit {
			items, err := s.store.ListRequestSummariesBefore(ctx, pageSize, before, beforeID)
			if err != nil {
				s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
				return
			}
			ids := make([]string, len(items))
			for i, item := range items {
				ids[i] = item.ID
			}
			events, err := s.store.RequestEventSummaries(ctx, ids)
			if err == nil {
				err = s.store.HydrateLegacyResponseSummaries(ctx, items, events)
			}
			if err != nil {
				s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "storage_unavailable"})
				return
			}
			prepareReadMetadata(items, events)
			anomalies := classifyRequestAnomalies(items, events, rules)
			for _, item := range items {
				if ingress != "" && ingressFromMeta(requestMeta(item)) != ingress {
					continue
				}
				kind := anomalies[item.ID].kind
				if outcome == "abnormal" && kind == "" || outcome != "" && outcome != "all" && outcome != "abnormal" && outcome != kind {
					continue
				}
				view := requestItem(item)
				view["noise_whitelisted"] = matchesLogNoise(rules, item.Status, item.Error, requestMeta(item))
				view["abnormal"], view["anomaly"] = kind != "", kind
				out = append(out, view)
				if len(out) > limit {
					break
				}
			}
			if len(items) < pageSize {
				break
			}
			last := items[len(items)-1]
			before, beforeID = last.CreatedAt, last.ID
		}
		hasMore := len(out) > limit
		if hasMore {
			out = out[:limit]
		}
		response := map[string]any{"ok": true, "items": out, "total": len(out), "has_more": hasMore}
		if r.URL.Query().Get("items_only") != "1" {
			response["requests"] = out
		}
		s.writeJSON(w, http.StatusOK, response)
		return
	}
	http.Error(w, `{"error":"storage_unavailable"}`, http.StatusServiceUnavailable)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func newProxyTransport(cfg Config) http.RoundTripper {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.DisableCompression = true
		clone.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.UpstreamDialTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		// The default transport keeps only two idle connections per host. A
		// proxy with a larger concurrency budget would then repeatedly tear down
		// upstream connections under load, adding latency and socket pressure.
		idle := cfg.MaxConcurrency
		if idle < 2 {
			idle = 2
		}
		clone.MaxIdleConnsPerHost = idle
		if clone.MaxIdleConns < idle {
			clone.MaxIdleConns = idle
		}
		clone.ResponseHeaderTimeout = time.Duration(cfg.UpstreamHeaderTimeoutSeconds) * time.Second
		clone.TLSHandshakeTimeout = time.Duration(cfg.UpstreamDialTimeoutSeconds) * time.Second
		clone.ExpectContinueTimeout = 1 * time.Second
		return clone
	}
	return http.DefaultTransport
}

func newReverseProxy(upstream *url.URL, s *Server) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{Transport: s.proxyTransport}
	proxy.Director = func(req *http.Request) {
		stripGatewayCredentials(req)
		suffix := proxyPathSuffix(req.URL.Path)
		target := *upstream
		target.Path = joinURLPath(upstream.Path, suffix)
		target.RawPath = ""
		// Request query parameters belong to the caller. Preserve a configured
		// upstream query when present, while allowing caller values to override.
		query := target.Query()
		for key, values := range req.URL.Query() {
			query[key] = values
		}
		target.RawQuery = query.Encode()
		req.URL = &target
		req.Host = target.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, errRequestBodyTooLarge) {
			if record, ok := r.Context().Value(requestRecordContextKey{}).(*requestRecord); ok {
				record.finish(http.StatusRequestEntityTooLarge, err)
			}
			http.Error(w, `{"error":"request_body_too_large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		status, failure := proxyFailure(r.Context(), err)
		if record, ok := r.Context().Value(requestRecordContextKey{}).(*requestRecord); ok {
			record.noteFailure("upstream_request")
			record.finish(status, failure)
		}
		if status == 499 {
			return
		}
		s.metrics.proxyErrors.Add(1)
		s.logger.Printf("upstream proxy error: %v", err)
		if status == http.StatusGatewayTimeout {
			http.Error(w, `{"error":"upstream_timeout"}`, status)
			return
		}
		http.Error(w, `{"error":"upstream_unavailable"}`, status)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		var record *requestRecord
		if resp.Request != nil {
			record, _ = resp.Request.Context().Value(requestRecordContextKey{}).(*requestRecord)
		}
		if record != nil {
			resp.Header.Set("X-Silencer-Request-ID", record.item.ID)
			record.mu.Lock()
			meta := requestMeta(record.item)
			meta["upstream_status"], meta["upstream_status_observed"] = resp.StatusCode, true
			encoded, _ := json.Marshal(meta)
			record.item.Meta = string(encoded)
			record.mu.Unlock()
		}
		if resp.Request != nil {
			if body, ok := resp.Request.Context().Value(maskingBodyContextKey{}).(*maskingReadCloser); ok {
				if err := body.stopForResponse(); err != nil {
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					return err
				}
			}
		}
		if resp.Body == nil {
			if record != nil {
				record.finish(resp.StatusCode, nil)
			}
			return nil
		}
		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		contentEncoding := strings.TrimSpace(strings.ToLower(resp.Header.Get("Content-Encoding")))
		textual := isRestorableContentType(contentType)
		unknownType := contentType == ""
		// Decode gzip before restoration. Returning compressed masked placeholders
		// would leak the upstream token because clients cannot restore it.
		source := io.ReadCloser(resp.Body)
		var upstreamRecorder *recordingReadCloser
		if record != nil {
			upstreamRecorder = &recordingReadCloser{ReadCloser: source, record: record, status: resp.StatusCode, captureOnly: true, ctx: resp.Request.Context()}
			source = upstreamRecorder
		}
		if (textual || unknownType) && (contentEncoding == "gzip" || contentEncoding == "deflate" || contentEncoding == "br") {
			reader, err := newDecodedBody(source, contentEncoding)
			if err != nil {
				if record != nil {
					record.finish(http.StatusBadGateway, err)
				}
				_ = source.Close()
				return err
			}
			source = reader
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
			contentEncoding = ""
		}
		if (textual || unknownType) && contentEncoding != "" && contentEncoding != "identity" {
			err := fmt.Errorf("unsupported response content encoding: %s", contentEncoding)
			if record != nil {
				record.finish(http.StatusBadGateway, err)
			}
			_ = source.Close()
			return err
		}
		if contentEncoding != "" && contentEncoding != "identity" {
			textual = false
		}
		if unknownType {
			buffered := bufio.NewReader(source)
			var err error
			contentType, err = sniffResponseContentType(buffered)
			if err != nil {
				_ = source.Close()
				return err
			}
			source = &decodedReadCloser{reader: buffered, source: source}
			textual = isRestorableContentType(contentType)
			if contentType != "" {
				resp.Header.Set("Content-Type", contentType)
			}
		}
		if !textual {
			if upstreamRecorder != nil {
				upstreamRecorder.captureOnly = false
			}
			resp.Body = source
			return nil
		}
		// Restoration can change the byte length in either direction (a token is
		// usually longer than the original secret). Let net/http use chunked
		// framing instead of advertising the masked upstream length.
		resp.Header.Del("Content-Length")
		requestCtx := context.Background()
		if resp.Request != nil {
			requestCtx = resp.Request.Context()
		}
		var factory restoreFactory
		if resp.Request != nil {
			if contextual, ok := resp.Request.Context().Value(contextualMaskingSessionContextKey{}).(ContextualMaskingSession); ok {
				factory = contextual.NewRestoreSession
			}
		}
		if factory == nil {
			streamer, ok := s.currentEngine().(StreamingEngine)
			if !ok {
				err := errors.New("streaming restore engine is unavailable")
				_ = source.Close()
				if record != nil {
					record.finish(http.StatusBadGateway, err)
				}
				return err
			}
			factory = streamer.NewRestoreSession
		}
		session, err := factory(requestCtx)
		if err != nil {
			if record != nil {
				record.finish(http.StatusBadGateway, err)
			}
			_ = source.Close()
			return err
		}
		kind := ""
		switch {
		case strings.Contains(contentType, "event-stream"):
			kind = "sse"
		case strings.Contains(contentType, "ndjson"):
			kind = "ndjson"
		case strings.Contains(contentType, "json"):
			kind = "json"
		}
		if kind != "" {
			if aborter, ok := session.(AbortableRestoreSession); ok {
				aborter.Abort()
			}
			session = newProtocolRestoreSession(kind, factory)
		}
		restored := &restoreReadCloser{source: source, session: session, ctx: requestCtx, record: record}
		guardPolicy := s.toolGuardSettings()
		if guardPolicy.enabled && kind != "" {
			guard := newToolGuardSession(session, kind, guardPolicy.mode, func(findings []toolFinding, mode string) {
				record.recordToolDecision(findings, mode)
			})
			restored.session = guard
			if kind == "json" && guardPolicy.mode != "observe" {
				// Inspect before ReverseProxy commits headers. The protocol restore
				// already bounds ordinary JSON responses to 32 MiB.
				body, readErr := io.ReadAll(restored)
				closeErr := restored.Close()
				if readErr != nil || closeErr != nil {
					if readErr == nil {
						readErr = closeErr
					}
					return readErr
				}
				if len(guard.findings) > 0 && guard.mode == "block" {
					resp.StatusCode = http.StatusForbidden
					resp.Status = "403 Forbidden"
				}
				resp.ContentLength = int64(len(body))
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
				resp.Body = io.NopCloser(bytes.NewReader(body))
				if record != nil {
					resp.Body = &recordingReadCloser{ReadCloser: resp.Body, record: record, restored: true, status: resp.StatusCode}
				}
				return nil
			}
		}
		if record != nil {
			resp.Body = &recordingReadCloser{ReadCloser: restored, record: record, restored: true, status: resp.StatusCode}
		} else {
			resp.Body = restored
		}
		return nil
	}
	return proxy
}

// stripGatewayCredentials prevents control-plane credentials from becoming
// upstream credentials. The management token is never required on the data
// plane, so preserve Authorization for the provider. Remove only explicitly
// management-only headers and the console session cookie.
func stripGatewayCredentials(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Del("X-Silencer-Token")
	cookieHeaders := req.Header.Values("Cookie")
	req.Header.Del("Cookie")
	for _, header := range cookieHeaders {
		var kept []string
		for _, part := range strings.Split(header, ";") {
			part = strings.TrimSpace(part)
			name, _, _ := strings.Cut(part, "=")
			if part != "" && strings.TrimSpace(name) != "silencer_session" {
				kept = append(kept, part)
			}
		}
		if len(kept) > 0 {
			req.Header.Add("Cookie", strings.Join(kept, "; "))
		}
	}
}

func proxyPathSuffix(path string) string {
	if strings.HasPrefix(path, requestPrefix) {
		return strings.TrimPrefix(path, requestPrefix)
	}
	if strings.HasPrefix(path, "/r/") {
		remaining := strings.TrimPrefix(path, "/r/")
		if slash := strings.IndexByte(remaining, '/'); slash >= 0 {
			return remaining[slash:]
		}
		return "/"
	}
	return path
}

func namedRouteName(path string) string {
	if !strings.HasPrefix(path, "/r/") {
		return ""
	}
	remaining := strings.TrimPrefix(path, "/r/")
	if slash := strings.IndexByte(remaining, '/'); slash >= 0 {
		remaining = remaining[:slash]
	}
	name, err := url.PathUnescape(remaining)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

type restoreReadCloser struct {
	source      io.ReadCloser
	session     RestoreSession
	ctx         context.Context
	record      *requestRecord
	buffer      []byte
	readChunk   []byte
	done        bool
	terminalErr error
}

type gzipReadCloser struct {
	*gzip.Reader
	source io.Closer
}

func (r *gzipReadCloser) Close() error {
	readerErr := r.Reader.Close()
	sourceErr := r.source.Close()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

// decodedReadCloser keeps the compressed source alive until the decoder has
// released its buffers. Deflate responses in the wild use both the zlib
// wrapper and the raw RFC 1951 stream, so detect the wrapper before choosing
// the decoder.
type decodedReadCloser struct {
	reader       io.Reader
	readerCloser io.Closer
	source       io.Closer
}

func (r *decodedReadCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }

func (r *decodedReadCloser) Close() error {
	var readerErr error
	if r.readerCloser != nil {
		readerErr = r.readerCloser.Close()
	}
	sourceErr := r.source.Close()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func newDecodedBody(source io.ReadCloser, encoding string) (io.ReadCloser, error) {
	switch encoding {
	case "gzip":
		reader, err := gzip.NewReader(source)
		if err != nil {
			return nil, err
		}
		return &decodedReadCloser{reader: reader, readerCloser: reader, source: source}, nil
	case "deflate":
		buffered := bufio.NewReader(source)
		header, err := buffered.Peek(2)
		if err != nil && err != io.EOF {
			return nil, err
		}
		wrapped := len(header) == 2 && header[0]&0x0f == 8 && ((int(header[0])<<8|int(header[1]))%31 == 0)
		var reader io.ReadCloser
		if wrapped {
			reader, err = zlib.NewReader(buffered)
		} else {
			reader = flate.NewReader(buffered)
		}
		if err != nil {
			return nil, err
		}
		return &decodedReadCloser{reader: reader, readerCloser: reader, source: source}, nil
	case "br":
		// andybalholm/brotli intentionally exposes a reader-only decoder. The
		// compressed source is still closed by decodedReadCloser.Close.
		reader := brotli.NewReader(source)
		return &decodedReadCloser{reader: reader, source: source}, nil
	default:
		return nil, fmt.Errorf("unsupported content encoding: %s", encoding)
	}
}

func (r *restoreReadCloser) Read(dst []byte) (int, error) {
	for len(r.buffer) == 0 && !r.done {
		if cap(r.readChunk) < 32*1024 {
			r.readChunk = make([]byte, 32*1024)
		}
		chunk := r.readChunk[:32*1024]
		n, readErr := r.source.Read(chunk)
		if n > 0 {
			out, err := r.session.Push(r.ctx, chunk[:n])
			r.updateCounts()
			if err != nil {
				r.record.noteFailure("response_restore")
				r.abort()
				r.done = true
				r.terminalErr = err
				return 0, err
			}
			r.buffer = append(r.buffer, out...)
			if terminal, ok := r.session.(interface{ StreamComplete() bool }); ok && terminal.StreamComplete() {
				out, err := r.session.Finish(r.ctx)
				r.updateCounts()
				if err != nil {
					r.record.noteFailure("response_restore")
					r.abort()
					r.done, r.terminalErr = true, err
					return 0, err
				}
				r.buffer = append(r.buffer, out...)
				r.done = true
				break
			}
		}
		if readErr == io.EOF {
			out, err := r.session.Finish(r.ctx)
			r.updateCounts()
			if err != nil {
				r.record.noteFailure("response_restore")
				r.abort()
				r.done = true
				r.terminalErr = err
				return 0, err
			}
			r.buffer = append(r.buffer, out...)
			r.done = true
		} else if readErr != nil {
			// Upstream disconnects and client cancellation can leave native
			// restore state allocated. Abort eagerly on any non-EOF read error.
			r.abort()
			r.done = true
			r.terminalErr = readErr
			return 0, readErr
		}
	}
	if len(r.buffer) == 0 {
		return 0, io.EOF
	}
	n := copy(dst, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

func (r *restoreReadCloser) updateCounts() {
	if counted, ok := r.session.(countedRestoreSession); ok && r.record != nil {
		counts := counted.RestoreCounts()
		r.record.mu.Lock()
		if !r.record.done {
			r.record.protocolCounts = &counts
		}
		r.record.mu.Unlock()
	}
}

func (r *restoreReadCloser) Close() error {
	err := r.source.Close()
	if r.done && r.terminalErr == nil && len(r.buffer) == 0 {
		// Successful EOF/protocol completion takes precedence over a late
		// cancellation or the error from closing an intentionally unread tail.
		return nil
	}
	if !r.done {
		r.abort()
		r.done = true
	}
	if err == nil {
		err = r.terminalErr
	}
	if err == nil && r.ctx != nil {
		err = r.ctx.Err()
	}
	return err
}

func (r *restoreReadCloser) abort() {
	if aborter, ok := r.session.(AbortableRestoreSession); ok {
		aborter.Abort()
	}
}

func joinURLPath(base, suffix string) string {
	if base == "" {
		if suffix == "" {
			return "/"
		}
		return suffix
	}
	if suffix == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

// Shutdown is provided for callers that own the http.Server lifecycle.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.capture != nil {
		s.capture.mu.Lock()
		s.capture.closed = true
		s.capture.mu.Unlock()
	}
	s.lifecycleMu.Lock()
	s.shuttingDown = true
	s.ready.set(false)
	done := s.requests.drained()
	s.lifecycleMu.Unlock()
	s.logRetentionMu.Lock()
	retentionDone := s.logDone
	if s.logCancel != nil {
		s.logCancel()
	}
	s.logRetentionMu.Unlock()
	if retentionDone == nil {
		completed := make(chan struct{})
		close(completed)
		retentionDone = completed
	}
	var shutdownErr error
	select {
	case <-done:
	case <-ctx.Done():
		shutdownErr = ctx.Err()
	}
	if shutdownErr == nil {
		select {
		case <-retentionDone:
		case <-ctx.Done():
			shutdownErr = ctx.Err()
		}
	}
	// Detach the current engine before waiting/closing. Requests that already
	// entered ServeHTTP may still hold it, so a timed-out shutdown must defer
	// Close until the in-flight requests drain.
	s.engineMu.Lock()
	current := s.engine
	s.engine = nil
	s.engineMu.Unlock()
	s.retiredMu.Lock()
	retired := append([]*retiredEngine(nil), s.retired...)
	s.retired = nil
	s.retiredMu.Unlock()
	closeEngines := func() {
		if s.capture != nil {
			s.capture.close()
		}
		if transport, ok := s.proxyTransport.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
		s.logSweepMu.Lock()
		if s.logOrphanDir != nil {
			s.logOrphanDir.Close()
			s.logOrphanDir = nil
		}
		s.logSweepMu.Unlock()
		// Run after admitted requests drain: no active restore can hold an
		// entry mutex, and all admitted MASK registrations have finished.
		s.extMu.Lock()
		for id, entry := range s.extRestore {
			entry.mu.Lock()
			if aborter, ok := entry.session.(AbortableRestoreSession); ok {
				aborter.Abort()
			}
			entry.finishRecord("gateway_shutdown", 503)
			delete(s.extRestore, id)
			entry.mu.Unlock()
		}
		s.extMu.Unlock()
		if engine, ok := current.(CloseableEngine); ok {
			engine.Close()
		}
		for _, entry := range retired {
			entry.closeable.Close()
		}
	}
	if shutdownErr == nil {
		closeEngines()
	} else {
		go func() {
			<-done
			<-retentionDone
			closeEngines()
		}()
	}
	return shutdownErr
}

// ShutdownTimeout returns the configured graceful-shutdown timeout.
func (s *Server) ShutdownTimeout() time.Duration {
	if s.cfg.ShutdownTimeoutSeconds < 1 {
		return 10 * time.Second
	}
	return time.Duration(s.cfg.ShutdownTimeoutSeconds) * time.Second
}
