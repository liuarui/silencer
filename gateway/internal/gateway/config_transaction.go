package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/silencer/gateway/internal/platformfs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Candidate validation never writes to the active configuration.
func runtimeConfigPatch(cfg Config, patch map[string]any) (Config, error) {
	if value, present := patch["upstream_url"]; present {
		text, ok := value.(string)
		if !ok {
			return cfg, errors.New("upstream_url must be a string")
		}
		if text != "" {
			u, err := url.Parse(text)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return cfg, errors.New("upstream_url must be an http(s) URL with a host")
			}
		}
		cfg.UpstreamURL = text
	}
	for _, item := range []struct {
		key   string
		limit float64
		set   func(int64)
	}{
		{"max_concurrency", 65536, func(n int64) { cfg.MaxConcurrency = int(n) }},
		{"max_response_bytes", 1 << 40, func(n int64) { cfg.MaxResponseBytes = n }},
	} {
		if value, present := patch[item.key]; present {
			n, ok := numericConfigValue(value)
			if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > item.limit || math.Trunc(n) != n {
				return cfg, fmt.Errorf("%s must be a bounded positive integer", item.key)
			}
			item.set(int64(n))
		}
	}
	return cfg, nil
}

func numericConfigValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	}
	return 0, false
}

type preparedConfigFile struct {
	path, temp string
	previous   []byte
	existed    bool
}

func prepareConfigFile(path string, value any) (*preparedConfigFile, error) {
	if path == "" {
		return nil, nil
	}
	path = configPathIdentity(path)
	p := &preparedConfigFile{path: path}
	old, err := os.ReadFile(path)
	if err == nil {
		p.previous = old
		p.existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gateway-candidate-*")
	if err != nil {
		return nil, err
	}
	p.temp = f.Name()
	if err = platformfs.SetPrivatePermissions(f.Name()); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(p.temp)
		return nil, err
	}
	return p, nil
}
func persistConfigCandidate(files ...*preparedConfigFile) error {
	return persistConfigCandidateUsing(defaultConfigDiskOps(), files...)
}
func persistConfigCandidateUsing(ops configDiskOps, files ...*preparedConfigFile) error {
	var active []*preparedConfigFile
	var journal configUndoJournal
	journal.Version = 1
	for _, f := range files {
		if f != nil {
			active = append(active, f)
			defer os.Remove(f.temp)
			journal.Files = append(journal.Files, configUndoEntry{Path: f.path, Previous: f.previous, Existed: f.existed})
		}
	}
	if len(active) == 0 {
		return nil
	}
	path := active[0].path + ".transaction"
	if _, err := os.Lstat(path); err == nil {
		return errors.New("configuration recovery is required")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(data) > maxConfigJournalBytes {
		return errors.New("configuration recovery journal is too large")
	}
	if err = writeConfigBytesUsing(path, data, ops); err != nil {
		return err
	}
	// Re-publish undo intent before rollback if journal removal already happened.
	rollback := func(cause error) error {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			if journalErr := writeConfigBytes(path, data); journalErr != nil {
				cause = errors.Join(cause, journalErr)
			}
		}
		if restoreErr := restoreConfigEntries(journal.Files); restoreErr != nil {
			return errors.Join(cause, restoreErr)
		}
		// Recovery may safely repeat if removing the journal fails.
		return errors.Join(cause, removeConfigJournal(path))
	}
	for _, f := range active {
		if err = ops.rename(f.temp, f.path); err != nil {
			return rollback(err)
		}
		if err = ops.syncDir(f.path); err != nil {
			return rollback(err)
		}
	}
	// Absence of the directory-synced journal is the commit point. Any handled
	// cleanup failure restores all previous bytes before returning an error.
	if err = ops.remove(path); err != nil {
		return rollback(err)
	}
	if err = ops.syncDir(path); err != nil {
		return rollback(err)
	}
	return nil
}

func configPathIdentity(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		return filepath.Join(parent, filepath.Base(absolute))
	}
	return absolute
}

// Caller holds configMu. Build and stage all candidates before installing any
// runtime state. lifecycleMu prevents shutdown from splitting the commit.
func (s *Server) commitConfigLocked(cfg Config, control map[string]any, rebuild bool, backupPrevious ...bool) error {
	var err error
	cfg, err = runtimeConfigPatch(cfg, control)
	if err != nil {
		return err
	}
	if err = validateLogSettings(control); err != nil {
		return err
	}
	if err = validateToolGuard(control); err != nil {
		return err
	}
	ensureBuiltinRuleInventory(control)
	candidate := &Server{cfg: cfg, control: control, extensionDisabled: s.extensionDisabled}
	if err = candidate.ensureExtensionTokenLocked(); err != nil {
		return err
	}
	var next Engine
	if rebuild {
		raw, e := engineConfigFromControl(control)
		if e != nil {
			return e
		}
		cfg.EngineConfig = raw
		next, err = NewEngine(cfg)
		if err != nil {
			return err
		}
	}
	installed := false
	defer func() {
		if !installed && next != nil {
			if c, ok := next.(CloseableEngine); ok {
				c.Close()
			}
		}
	}()
	runtimePath := runtimeConfigPath()
	if runtimePath != "" && s.configPath != "" && configPathIdentity(runtimePath) == configPathIdentity(s.configPath) {
		return errors.New("runtime and control configuration paths must differ")
	}
	anchor := runtimePath
	if anchor == "" {
		anchor = s.configPath
	}
	anchor = configPathIdentityOrEmpty(anchor)
	diskLock, err := lockConfigDisk(anchor)
	if err != nil {
		return err
	}
	if diskLock != nil {
		defer diskLock.Close()
	}
	if err = recoverConfigJournal(anchor, s.configPath); err != nil {
		return err
	}
	runtimeFile, err := prepareConfigFile(runtimePath, cfg)
	if err != nil {
		return err
	}
	if runtimeFile != nil {
		defer os.Remove(runtimeFile.temp)
	}
	controlFile, err := prepareConfigFile(s.configPath, control)
	if err != nil {
		return err
	}
	if controlFile != nil {
		defer os.Remove(controlFile.temp)
	}
	var backupFile *preparedConfigFile
	if len(backupPrevious) > 0 && backupPrevious[0] && s.configPath != "" {
		backupPath := s.configPath + ".backup-" + time.Now().UTC().Format("20060102-150405.000000000") + ".json"
		backupFile, err = prepareConfigFile(backupPath, s.control)
		if err != nil {
			return err
		}
		defer os.Remove(backupFile.temp)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shuttingDown {
		return errors.New("gateway is shutting down")
	}
	if err = persistConfigCandidate(runtimeFile, controlFile, backupFile); err != nil {
		return err
	}
	if cfg.UpstreamURL != s.cfg.UpstreamURL {
		s.proxy = nil
		if cfg.UpstreamURL != "" {
			u, _ := url.Parse(cfg.UpstreamURL)
			s.proxy = newReverseProxy(u, s)
		}
	}
	if cfg.MaxConcurrency != s.cfg.MaxConcurrency {
		s.limiterMu.Lock()
		s.limiter = make(chan struct{}, cfg.MaxConcurrency)
		s.limiterMu.Unlock()
	}
	s.cfg.UpstreamURL = cfg.UpstreamURL
	s.cfg.MaxConcurrency = cfg.MaxConcurrency
	s.cfg.MaxResponseBytes = cfg.MaxResponseBytes
	if rebuild {
		s.cfg.EngineConfig = cfg.EngineConfig
	}
	s.control = control
	if next != nil {
		s.installEngineLocked(next)
	}
	installed = true
	if s.proxy != nil || hasConfiguredUpstream(control) {
		s.running = true
	}
	return nil
}
