package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/silencer/gateway/internal/platformfs"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

var ledgerStages = []string{"raw", "masked", "upstream_response", "restored_response"}
var ledgerIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Full bodies stream to the persistent data volume; accounting buffers stay
// bounded independently. File names are generated request IDs, never API paths.
type bodyCapture struct {
	dir     string
	files   map[string]*os.File
	writers map[string]*bufio.Writer
	err     error
	lease   *os.File
	enabled bool
	owned   bool
}

func newBodyCapture(root, id string) *bodyCapture {
	return newRequestCapture(root, id, true)
}

func newRequestCapture(root, id string, enabled bool) *bodyCapture {
	c := &bodyCapture{files: map[string]*os.File{}, writers: map[string]*bufio.Writer{}, enabled: enabled}
	if !ledgerIDPattern.MatchString(id) {
		c.err = errors.New("invalid ledger ID")
		return c
	}
	c.dir = filepath.Join(root, id)
	if c.err = os.MkdirAll(root, 0700); c.err != nil {
		return c
	}
	rootLock, err := lockCaptureRoot(root)
	if err != nil {
		c.err = err
		return c
	}
	defer rootLock.Close()
	if c.err = os.Mkdir(c.dir, 0700); c.err != nil {
		return c
	}
	c.owned = true
	if c.err = platformfs.SetPrivatePermissions(c.dir); c.err != nil {
		return c
	}
	c.lease, c.err = platformfs.OpenNoFollow(filepath.Join(c.dir, ".lease"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if c.err != nil {
		return c
	}
	if c.err = platformfs.Lock(c.lease, platformfs.Exclusive|platformfs.Nonblock); c.err != nil {
		return c
	}
	if !enabled {
		return c
	}
	for _, stage := range ledgerStages {
		file, err := os.OpenFile(filepath.Join(c.dir, stage), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			c.err = err
			break
		}
		c.files[stage] = file
		c.writers[stage] = bufio.NewWriterSize(file, 64<<10)
	}
	return c
}

func lockCaptureRoot(root string) (*os.File, error) {
	return lockCaptureRootMode(root, platformfs.Exclusive)
}

func lockCaptureRootMode(root string, mode int) (*os.File, error) {
	f, err := platformfs.OpenNoFollow(filepath.Join(root, ".capture.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = platformfs.Lock(f, mode); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func (c *bodyCapture) discard() {
	if c == nil {
		return
	}
	c.close()
	if c.owned {
		_ = os.RemoveAll(c.dir)
	}
	c.release()
}

func (c *bodyCapture) append(stage string, data []byte) {
	if c == nil || !c.enabled || c.err != nil || len(data) == 0 {
		return
	}
	_, c.err = c.writers[stage].Write(data)
}

func (c *bodyCapture) close() bool {
	if c == nil {
		return false
	}
	for stage, file := range c.files {
		if err := c.writers[stage].Flush(); err != nil && c.err == nil {
			c.err = err
		}
		if err := file.Sync(); err != nil && c.err == nil {
			c.err = err
		}
		if err := file.Close(); err != nil && c.err == nil {
			c.err = err
		}
	}
	return c.enabled && c.err == nil
}

func (c *bodyCapture) release() {
	if c != nil && c.lease != nil {
		_ = c.lease.Close()
		c.lease = nil
	}
}

// Kernel locks survive neither a crash nor a container restart. They distinguish
// an actual long-running stream from an abandoned row across blue/green processes.
func (s *Server) logRequestActive(id string) bool {
	if !ledgerIDPattern.MatchString(id) {
		return false
	}
	file, err := platformfs.OpenNoFollow(filepath.Join(s.logDirectory(), id, ".lease"), os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	defer file.Close()
	if err = platformfs.Lock(file, platformfs.Exclusive|platformfs.Nonblock); err != nil {
		return true
	}
	return false
}

func (s *Server) attachFullLedgerBodies(view map[string]any, item storage.Request) {
	attachLedgerBodies(view, item)
	meta := requestMeta(item)
	if meta["log_expired"] == true || meta["body_storage_error"] == true {
		for _, key := range ledgerStages {
			delete(view, key)
		}
		view["body_unavailable"] = true
		view["body_expired"] = meta["log_expired"] == true
		view["body_storage_error"] = meta["body_storage_error"] == true
		return
	}
	if meta["body_store"] != true {
		return
	}
	if !ledgerIDPattern.MatchString(item.ID) {
		view["body_unavailable"] = true
		return
	}
	for _, stage := range ledgerStages {
		body, err := os.ReadFile(filepath.Join(s.logDirectory(), item.ID, stage))
		if err != nil {
			// A prefix is not a complete body. Never present a partial fallback
			// as if it were the full retained request or response.
			for _, key := range ledgerStages {
				delete(view, key)
			}
			view["body_unavailable"] = true
			view["body_storage_error"] = true
			return
		}
		view[stage] = string(body)
	}
	delete(view, "truncated")
	view["body_unavailable"] = false
	view["request_body_incomplete"] = meta["request_body_incomplete"] == true
}

func (s *Server) logDirectory() string {
	if root := os.Getenv("SILENCER_LOG_DIR"); root != "" {
		return root
	}
	if s.configPath != "" {
		return filepath.Join(filepath.Dir(s.configPath), "request-logs")
	}
	if path := os.Getenv("SILENCER_SQLITE_PATH"); filepath.IsAbs(path) {
		return filepath.Join(filepath.Dir(path), "request-logs")
	}
	return "request-logs"
}

func (s *Server) logRetentionDays() int {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if days, ok := numericMetaInt(s.control["log_retention_days"]); ok && days >= 0 {
		return days
	}
	return 3
}

func validateLogSettings(config map[string]any) error {
	if value, exists := config["log_noise_allowlist"]; exists {
		if _, err := parseLogNoiseAllowlist(value); err != nil {
			return err
		}
	}
	if value, exists := config["log_retention_days"]; exists {
		days, ok := numericMetaInt(value)
		if !ok || days < 0 || days > 36500 {
			return errors.New("log_retention_days must be an integer between 0 and 36500")
		}
		if v, ok := value.(float64); ok && v != float64(days) {
			return errors.New("log_retention_days must be an integer")
		}
	}
	if value, exists := config["record_detail_data"]; exists {
		if _, ok := value.(bool); !ok {
			return errors.New("record_detail_data must be boolean")
		}
	}
	return nil
}

// Keep non-text accounting after detail expiry. Plaintext words, mappings and
// per-token evidence expire together with bodies; aggregate totals remain.
func expiredRequestMeta(r storage.Request) string {
	meta := requestMeta(r)
	var aggregate maskAggregate
	addMaskedRequestStats(&aggregate, r)
	_, restored, unresolved := restorePlaceholderCounts(r.Upstream, r.Restored)
	if _, present := meta["restore_count"]; !present {
		meta["restore_count"], meta["restore_restored"], meta["restore_unresolved"] = restored+unresolved, restored, unresolved
	}
	meta["mask_summary"] = aggregate.byLabel
	text := expiredMeta(meta)
	var expired map[string]any
	_ = json.Unmarshal([]byte(text), &expired)
	expired["body_cleanup_pending"] = true
	encoded, _ := json.Marshal(expired)
	return string(encoded)
}

func expiredEventMeta(e storage.Event) string {
	meta := eventMeta(e)
	meta["count"] = maskEventItemCount(e)
	var aggregate maskAggregate
	if len(e.Masked) > 0 {
		addMaskedTextStats(&aggregate, e.Masked)
	} else {
		addMaskedItemStats(&aggregate, meta)
	}
	meta["mask_summary"] = aggregate.byLabel
	return expiredMeta(meta)
}

func expiredMeta(meta map[string]any) string {
	keep := map[string]any{"log_expired": true, "items_complete": true}
	for _, key := range []string{"mask_summary", "mask_token_count", "count", "model", "ingress", "method", "path", "body_rewritten", "suffix_reused", "first_diff_byte", "restore_count", "restore_restored", "restore_unresolved", "usage_found", "usage_prompt_tokens", "usage_prompt_observed", "usage_completion_tokens", "usage_cache_observed", "usage_cache_read_tokens", "usage_cache_write_tokens", "usage_cache_rate_observed", "usage_cache_input_tokens", "usage_cache_anthropic_shape"} {
		if value, ok := meta[key]; ok {
			keep[key] = value
		}
	}
	data, _ := json.Marshal(keep)
	return string(data)
}

func (s *Server) sweepLogs(ctx context.Context) { s.sweepLogsWithDays(ctx, s.logRetentionDays()) }

func (s *Server) sweepLogsWithDays(ctx context.Context, days int) {
	s.logSweepMu.Lock()
	defer s.logSweepMu.Unlock()
	if s.store == nil || ctx.Err() != nil {
		return
	}
	s.sweepOrphanBodies(ctx)
	if err := s.store.RepairExpiredLogDetails(ctx, 256); err != nil {
		return
	}
	pending, err := s.store.PendingBodyCleanup(ctx, 256)
	if err != nil {
		return
	}
	for _, id := range pending {
		s.removeExpiredBody(ctx, id)
	}
	if days == 0 {
		return
	}
	before := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	for batch := 0; batch < 8; batch++ {
		ids, n, err := s.store.ExpireLogDetails(ctx, before, 32, expiredRequestMeta, expiredEventMeta, s.logRequestActive)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Print("log retention cleanup failed")
			}
			return
		}
		for _, id := range ids {
			s.removeExpiredBody(ctx, id)
		}
		if n == 0 {
			break
		}
	}
}

// Root and lease locks are held throughout inspection/removal, including the
// mkdir-to-lease window in another process. DB failures always retain bodies.
func (s *Server) withInactiveBody(ctx context.Context, id string, action func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ledgerIDPattern.MatchString(id) {
		return nil
	}
	root := s.logDirectory()
	// A busy root is retried on the next sweep, never allowed to hold shutdown
	// behind another process's capture creation.
	lock, err := lockCaptureRootMode(root, platformfs.Exclusive|platformfs.Nonblock)
	if os.IsNotExist(err) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return action(filepath.Join(root, id))
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Join(root, id)
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return action(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid capture directory")
	}
	f, err := platformfs.OpenNoFollow(filepath.Join(dir, ".lease"), os.O_RDWR, 0600)
	if err == nil {
		defer f.Close()
		if err = platformfs.Lock(f, platformfs.Exclusive|platformfs.Nonblock); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return action(dir)
}
func (s *Server) sweepOrphanBodies(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if s.logOrphanDir == nil {
		s.logOrphanDir, _ = os.Open(s.logDirectory())
	}
	if s.logOrphanDir == nil {
		return
	}
	entries, err := s.logOrphanDir.ReadDir(256)
	if err != nil || len(entries) < 256 {
		s.logOrphanDir.Close()
		s.logOrphanDir = nil
		if err != nil && err != io.EOF {
			return
		}
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() || !ledgerIDPattern.MatchString(entry.Name()) {
			continue
		}
		_ = s.withInactiveBody(ctx, entry.Name(), func(dir string) error {
			exists, err := s.store.RequestExists(ctx, entry.Name())
			if err != nil || exists {
				return err
			}
			return os.RemoveAll(dir)
		})
	}
}
func (s *Server) removeExpiredBody(ctx context.Context, id string) {
	err := s.withInactiveBody(ctx, id, func(dir string) error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return s.store.CompleteBodyCleanup(ctx, id)
	})
	if err != nil && ctx.Err() == nil && !platformfs.IsBusy(err) {
		s.logger.Print("expired log body cleanup failed")
	}
}

func (s *Server) startLogRetention() {
	ctx, cancel := context.WithCancel(context.Background())
	s.logCancel = cancel
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	s.logDone = done
	s.logWakeMu.Lock()
	s.logWake = wake
	s.logWakeMu.Unlock()
	days := s.logRetentionDays()
	storeDone := s.store.Done()
	go func() {
		defer close(done)
		defer cancel()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-storeDone:
				return
			default:
			}
			s.extMu.Lock()
			s.reapExpiredExtensionRestoresLocked(time.Now())
			s.extMu.Unlock()
			s.sweepLogsWithDays(ctx, days)
			select {
			case <-ctx.Done():
				return
			case <-storeDone:
				return
			case <-ticker.C:
			case <-wake:
			}
			days = s.logRetentionDays()
		}
	}()
}

func (s *Server) wakeLogRetention() {
	s.logWakeMu.Lock()
	wake := s.logWake
	s.logWakeMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
