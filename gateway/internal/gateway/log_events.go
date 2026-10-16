package gateway

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/silencer/gateway/internal/storage"
)

func ledgerHTTPStatus(status string) int {
	code, _ := strconv.Atoi(status)
	return code
}

// Keep MASK/RESTORE as the stored phases used by accounting. Presentation and
// filtering share this projection of the completed request's outcome.
func displayLogEventType(event storage.Event, requestIDs map[string]struct{}) string {
	typ := strings.ToUpper(strings.TrimSpace(event.Type))
	if strings.HasPrefix(typ, "AUDIT_") {
		return "SCAN_WARN"
	}
	if typ != "MASK" && typ != "RESTORE" {
		return typ
	}
	meta := eventMeta(event)
	code, _ := numericMetaInt(meta["http_status"])
	if code == 0 {
		code = ledgerHTTPStatus(event.Status)
	}
	switch {
	case event.Error == "canceled" || event.Error == "deadline_exceeded" || code == 499:
		return "CANCEL"
	case event.Error == "dns_error":
		return "DNS_ERROR"
	case meta["noise_whitelisted"] == true && code == 404 && event.Error == "" && event.Status != "error" && meta["ingress"] == "proxy":
		return "SKIP"
	case event.Error != "" || event.Status == "error" || code >= 400:
		return "ERR"
	case !eventHasSensitiveEvidence(event, requestIDs):
		return "PASS"
	default:
		return typ
	}
}

// Older RESTORE rows retained only restoration status, even on failed requests.
// Recover the outcome from the matching MASK without changing persisted data.
func normalizeLogOutcome(event storage.Event, masks map[string]storage.Event) storage.Event {
	if !strings.EqualFold(event.Type, "RESTORE") {
		return event
	}
	// Only fill an absent legacy outcome. A phase's own observed failure must
	// never be replaced by an earlier MASK outcome (including an allowed 404).
	meta := eventMeta(event)
	observedCode, _ := numericMetaInt(meta["http_status"])
	if event.Error != "" || event.Status == "error" || event.Status == "canceled" || ledgerHTTPStatus(event.Status) >= 400 || observedCode >= 400 {
		return event
	}
	mask, ok := masks[eventRequestKey(event)]
	if !ok {
		return event
	}
	maskMeta := eventMeta(mask)
	code, _ := numericMetaInt(maskMeta["http_status"])
	if code == 0 {
		code = ledgerHTTPStatus(mask.Status)
	}
	if mask.Error == "" && mask.Status != "error" && code < 400 {
		return event
	}
	meta = cloneReadMetadata(meta)
	event.Status, event.Error = mask.Status, mask.Error
	meta["status"] = event.Status
	for _, key := range []string{"method", "path", "ingress", "failure_stage"} {
		if _, exists := meta[key]; !exists {
			if value, ok := maskMeta[key]; ok {
				meta[key] = value
			}
		}
	}
	if code, ok := eventMeta(mask)["http_status"]; ok {
		meta["http_status"] = code
	} else if code := ledgerHTTPStatus(mask.Status); code > 0 {
		meta["http_status"] = code
	}
	event.StatsMeta = meta
	return event
}

func isNoiseLogType(typ string) bool {
	return typ == "PASS" || typ == "BYPASS" || typ == "SKIP"
}

// Full-text searches must include the retained ledger, not only the event's
// path and token metadata. MatchReader scans files without loading full bodies.
func (s *Server) logRequestBodyMatches(ctx context.Context, item storage.Request, query string, pattern *regexp.Regexp) bool {
	if requestMeta(item)["log_expired"] == true {
		return false
	}

	for _, body := range [][]byte{item.Raw, item.Masked, item.Upstream, item.Restored} {
		if strings.Contains(strings.ToLower(string(body)), query) {
			return true
		}
	}
	if requestMeta(item)["body_store"] != true || !ledgerIDPattern.MatchString(item.ID) {
		return false
	}
	if pattern == nil {
		pattern = regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	}
	for _, stage := range ledgerStages {
		if ctx.Err() != nil {
			return false
		}
		file, err := os.Open(filepath.Join(s.logDirectory(), item.ID, stage))
		if err != nil {
			continue
		}
		matched := pattern.MatchReader(bufio.NewReader(logContextReader{ctx: ctx, reader: file}))
		_ = file.Close()
		if matched {
			return true
		}
	}
	return false
}

type logContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r logContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
