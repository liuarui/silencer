package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) backupDir() string {
	if s.configPath == "" {
		return ""
	}
	return filepath.Dir(s.configPath)
}
func validBackupName(name string) bool {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) || strings.HasPrefix(name, ".") {
		return false
	}
	return strings.Contains(name, ".backup-")
}
func (s *Server) createConfigBackupLocked() (string, error) {
	if s.configPath == "" {
		return "", fmt.Errorf("config path not configured")
	}
	data, err := json.MarshalIndent(s.control, "", "  ")
	if err != nil {
		return "", err
	}
	dir := s.backupDir()
	if err = os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s.backup-%s.json", filepath.Base(s.configPath), time.Now().UTC().Format("20060102-150405.000000000"))
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".backup-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return "", err
	}
	return name, os.Rename(tmpName, path)
}
func (s *Server) handleConfigBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	dir := s.backupDir()
	if dir == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backups": []any{}, "current": map[string]any{}})
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		s.writeJSON(w, 500, map[string]any{"error": "backup_list_failed"})
		return
	}
	out := []map[string]any{}
	prefix := filepath.Base(s.configPath) + ".backup-"
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !validBackupName(e.Name()) {
			continue
		}
		info, er := e.Info()
		if er != nil {
			continue
		}
		var backup map[string]any
		if data, readErr := os.ReadFile(filepath.Join(dir, e.Name())); readErr == nil {
			_ = json.Unmarshal(data, &backup)
		}
		out = append(out, map[string]any{"file": e.Name(), "name": e.Name(), "mtime": info.ModTime().Unix(), "size": info.Size(), "modified_at": info.ModTime(),
			"upstreams": len(asSlice(backup["upstreams"])), "cats": len(asMap(backup["sensitive"])), "words": len(asSlice(backup["words"])), "domains": len(asSlice(backup["target_domains"]))})
	}
	s.configMu.RLock()
	current := cloneMap(s.control)
	s.configMu.RUnlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backups": out, "current": map[string]any{
		"upstreams": len(asSlice(current["upstreams"])), "cats": len(asMap(current["sensitive"])), "words": len(asSlice(current["words"])), "domains": len(asSlice(current["target_domains"])),
	}})
}

func asSlice(value any) []any        { v, _ := value.([]any); return v }
func asMap(value any) map[string]any { v, _ := value.(map[string]any); return v }
