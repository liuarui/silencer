package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type configUndoEntry struct {
	Path     string `json:"path"`
	Previous []byte `json:"previous"`
	Existed  bool   `json:"existed"`
}
type configUndoJournal struct {
	Version int               `json:"version"`
	Files   []configUndoEntry `json:"files"`
}

const maxConfigJournalBytes = 64 << 20

func configPathIdentityOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	return configPathIdentity(path)
}
func lockConfigDisk(anchor string) (*os.File, error) {
	if anchor == "" {
		return nil, nil
	}
	fd, err := syscall.Open(anchor+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "configuration lock")
	if info, statErr := f.Stat(); statErr != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("configuration lock is not a regular file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func syncConfigDirectory(path string) error {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Hooks are local to one transaction so fault tests do not mutate global state.
type configDiskOps struct {
	rename  func(string, string) error
	remove  func(string) error
	syncDir func(string) error
}

func defaultConfigDiskOps() configDiskOps {
	return configDiskOps{os.Rename, os.Remove, syncConfigDirectory}
}
func writeConfigBytes(path string, data []byte) error {
	return writeConfigBytesUsing(path, data, defaultConfigDiskOps())
}
func writeConfigBytesUsing(path string, data []byte, ops configDiskOps) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".gateway-durable-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = ops.rename(temp, path); err != nil {
		return err
	}
	return ops.syncDir(path)
}
func restoreConfigEntries(entries []configUndoEntry) error {
	for _, entry := range entries {
		if !filepath.IsAbs(entry.Path) {
			return errors.New("invalid configuration journal path")
		}
		if entry.Existed {
			if err := writeConfigBytes(entry.Path, entry.Previous); err != nil {
				return err
			}
		} else {
			info, err := os.Lstat(entry.Path)
			if err == nil {
				if !info.Mode().IsRegular() {
					return errors.New("configuration recovery target is not a file")
				}
				err = os.Remove(entry.Path)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err = syncConfigDirectory(entry.Path); err != nil {
				return err
			}
		}
	}
	return nil
}
func removeConfigJournal(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncConfigDirectory(path)
}

// Recovery paths come from trusted configuration, never from journal contents.
func validateConfigJournal(anchor string, journal configUndoJournal, allowed ...string) error {
	if journal.Version != 1 || len(journal.Files) == 0 || len(journal.Files) > 3 {
		return errors.New("invalid configuration recovery journal")
	}
	paths := map[string]bool{anchor: true}
	for _, path := range allowed {
		if path != "" {
			paths[configPathIdentity(path)] = true
		}
	}
	seen := map[string]bool{}
	for _, entry := range journal.Files {
		identity := configPathIdentity(entry.Path)
		permitted := paths[identity]
		if !permitted {
			for path := range paths {
				suffix := strings.TrimPrefix(identity, path+".backup-")
				if suffix != identity && strings.HasSuffix(suffix, ".json") {
					_, err := time.Parse("20060102-150405.000000000", strings.TrimSuffix(suffix, ".json"))
					permitted = err == nil
					if permitted {
						break
					}
				}
			}
		}
		if !filepath.IsAbs(entry.Path) || identity != entry.Path || !permitted || seen[identity] || (!entry.Existed && len(entry.Previous) != 0) {
			return errors.New("invalid configuration recovery target")
		}
		if info, err := os.Lstat(entry.Path); err == nil && !info.Mode().IsRegular() {
			return errors.New("configuration recovery target is not a regular file")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		seen[identity] = true
	}
	if !seen[anchor] {
		return errors.New("configuration journal anchor missing")
	}
	return nil
}
func recoverConfigJournal(anchor string, allowed ...string) error {
	if anchor == "" {
		return nil
	}
	anchor = configPathIdentity(anchor)
	path := anchor + ".transaction"
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "configuration journal")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("configuration journal is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigJournalBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxConfigJournalBytes {
		return errors.New("configuration recovery journal is too large")
	}
	var journal configUndoJournal
	if json.Unmarshal(data, &journal) != nil {
		return errors.New("invalid configuration recovery journal")
	}
	if err = validateConfigJournal(anchor, journal, allowed...); err != nil {
		return err
	}
	if err = restoreConfigEntries(journal.Files); err != nil {
		return fmt.Errorf("recover configuration: %w", err)
	}
	return removeConfigJournal(path)
}
func configParticipantPaths(runtime, control string) (string, string, string, error) {
	if control == "" && runtime != "" {
		control = runtime + ".control.json"
	}
	control = configPathIdentityOrEmpty(control)
	auth := ""
	if runtime != "" {
		auth = configPathIdentity(runtime + ".auth")
	}
	runtime = configPathIdentityOrEmpty(runtime)
	seen := map[string]bool{}
	for _, path := range []string{runtime, control, auth} {
		if path == "" {
			continue
		}
		if seen[path] {
			return "", "", "", errors.New("configuration paths must differ")
		}
		seen[path] = true
	}
	anchor := runtime
	if anchor == "" {
		anchor = control
	}
	return anchor, control, auth, nil
}
func lockAndRecoverConfiguredConfig() (func(), error) {
	anchor, control, auth, err := configParticipantPaths(runtimeConfigPath(), strings.TrimSpace(os.Getenv("SILENCER_CONTROL_CONFIG_FILE")))
	if err != nil {
		return nil, err
	}
	var locks []*os.File
	unlock := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Close()
		}
	}
	for _, path := range []string{anchor, auth} {
		if path == "" {
			continue
		}
		lock, err := lockConfigDisk(path)
		if err != nil {
			unlock()
			return nil, err
		}
		locks = append(locks, lock)
		allowed := []string(nil)
		if path == anchor {
			allowed = []string{control}
		}
		if err = recoverConfigJournal(path, allowed...); err != nil {
			unlock()
			return nil, err
		}
		if path == anchor && control != "" && control != anchor {
			if err = recoverConfigJournal(control); err != nil {
				unlock()
				return nil, err
			}
		}
	}
	return unlock, nil
}
func recoverConfiguredConfig() error {
	unlock, err := lockAndRecoverConfiguredConfig()
	if err == nil {
		unlock()
	}
	return err
}
func persistSingleConfig(path string, value any) error {
	if path == "" {
		return nil
	}
	path = configPathIdentity(path)
	lock, err := lockConfigDisk(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = recoverConfigJournal(path); err != nil {
		return err
	}
	prepared, err := prepareConfigFile(path, value)
	if err != nil {
		return err
	}
	return persistConfigCandidate(prepared)
}
