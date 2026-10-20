package hostagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/silencer/gateway/internal/platformfs"
)

const MaxMessage = 1 << 20
const MaxConfigBytes = 4 << 20
const MaxPlans = 64

type Request struct {
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response.Data contains operation-specific fields. The gateway unwraps it
// before returning a control-plane response to the management UI.
type Response struct {
	OK        bool   `json:"ok"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	Data      any    `json:"data,omitempty"`
}

type Client struct {
	Path    string
	Timeout time.Duration
}

type FieldChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

type Plan struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"`
	Path        string        `json:"path"`
	SHA256      string        `json:"sha256"`
	NewSHA256   string        `json:"new_sha256"`
	Changes     []FieldChange `json:"changes,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
}

type Change struct {
	Path      string    `json:"path"`
	Backup    string    `json:"backup"`
	OldSHA256 string    `json:"old_sha256"`
	NewSHA256 string    `json:"new_sha256"`
	Status    string    `json:"status"`
	Mode      uint32    `json:"mode"`
	CreatedAt time.Time `json:"created_at"`
}

type planPayload struct {
	ConfigPath string `json:"config_path"`
	ProxyURL   string `json:"proxy_url"`
}

type applyPayload struct {
	Plan Plan `json:"plan"`
}

type rollbackPayload struct {
	Change Change `json:"change"`
}

func ValidSocketPath(p string) bool {
	p = strings.TrimSpace(p)
	return p != "" && filepath.IsAbs(p) && len(p) <= 108 && filepath.Clean(p) == p
}

func (c Client) Call(ctx context.Context, op string, payload any) (Response, error) {
	if !ValidSocketPath(c.Path) {
		return Response{}, errors.New("invalid host agent socket path")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("marshal host-agent payload: %w", err)
	}
	req, err := json.Marshal(Request{Op: op, Payload: b})
	if err != nil {
		return Response{}, fmt.Errorf("marshal host-agent request: %w", err)
	}
	if len(req)+1 > MaxMessage {
		return Response{}, errors.New("host-agent request too large")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", c.Path)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if _, err = conn.Write(append(req, '\n')); err != nil {
		return Response{}, err
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return Response{}, errors.New("unexpected host-agent connection type")
	}
	if err := unixConn.CloseWrite(); err != nil {
		return Response{}, err
	}
	reader := bufio.NewReader(conn)
	frame, err := readFrame(reader, MaxMessage)
	if err != nil {
		return Response{}, err
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("host-agent response contains trailing data")
	}
	var resp Response
	if err := json.Unmarshal(frame, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func readFrame(r *bufio.Reader, max int) ([]byte, error) {
	if max < 1 {
		return nil, errors.New("invalid message limit")
	}
	var frame []byte
	for {
		part, err := r.ReadSlice('\n')
		frame = append(frame, part...)
		if len(frame) > max {
			return nil, errors.New("message too large")
		}
		if err == nil {
			return bytes.TrimSpace(frame), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

type Server struct {
	Socket, ConfigPath, StatePath string
	ConfigPaths                   []string
	MaxBytes                      int64
	SocketMode                    os.FileMode
	SocketDirMode                 os.FileMode
	SocketGroup                   int
	SetSocketGroup                bool

	mu      sync.Mutex
	history []Change
	plans   map[string]pendingPlan
}

type pendingPlan struct {
	plan    Plan
	content []byte
	mode    os.FileMode
}

const planTTL = 5 * time.Minute

func (s *Server) ListenAndServe(ctx context.Context) error {
	if !ValidSocketPath(s.Socket) {
		return errors.New("invalid host agent socket path")
	}
	if s.MaxBytes <= 0 || s.MaxBytes > MaxMessage {
		s.MaxBytes = MaxMessage
	}
	if s.plans == nil {
		s.plans = make(map[string]pendingPlan)
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0700); err != nil {
		return err
	}
	parentPath := filepath.Dir(s.Socket)
	parent, err := os.Lstat(parentPath)
	if err != nil || parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() {
		return errors.New("socket parent must be a real directory")
	}
	if runtime.GOOS == "windows" {
		if s.SetSocketGroup || (s.SocketMode != 0 && s.SocketMode.Perm() != 0600) || (s.SocketDirMode != 0 && s.SocketDirMode.Perm() != 0700) {
			return errors.New("Windows host agent requires private user socket permissions")
		}
		// Windows AF_UNIX enforces the ACL inherited from the socket directory.
		if err := platformfs.SetPrivatePermissions(parentPath); err != nil {
			return err
		}
	}
	if s.SetSocketGroup {
		if err := os.Chown(parentPath, -1, s.SocketGroup); err != nil {
			return fmt.Errorf("set socket directory group: %w", err)
		}
	}
	if s.SocketDirMode != 0 {
		if err := os.Chmod(parentPath, s.SocketDirMode.Perm()); err != nil {
			return fmt.Errorf("set socket directory mode: %w", err)
		}
	}
	if err := prepareSocketPath(s.Socket); err != nil {
		return err
	}
	l, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer l.Close()
	socketMode := s.SocketMode
	if socketMode == 0 {
		socketMode = 0600
	}
	if s.SetSocketGroup {
		if err := os.Chown(s.Socket, -1, s.SocketGroup); err != nil {
			return fmt.Errorf("set socket group: %w", err)
		}
	}
	if err := os.Chmod(s.Socket, socketMode.Perm()); err != nil {
		return err
	}
	socketInfo, err := os.Lstat(s.Socket)
	if err != nil {
		return err
	}
	defer func() {
		if current, err := os.Lstat(s.Socket); err == nil && os.SameFile(socketInfo, current) {
			_ = os.Remove(s.Socket)
		}
	}()
	s.loadHistory()
	go func() { <-ctx.Done(); _ = l.Close() }()
	for {
		c, e := l.Accept()
		if e != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return e
			}
		}
		go s.handle(c)
	}
}

func prepareSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect socket path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("socket path is occupied")
	}
	probe, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = probe.Close()
		return errors.New("host-agent socket is already in use")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return errors.New("socket path is occupied")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(c)
	frame, err := readFrame(reader, int(s.MaxBytes))
	if err != nil {
		_ = json.NewEncoder(c).Encode(Response{Available: true, Error: "invalid_request"})
		return
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		_ = json.NewEncoder(c).Encode(Response{Available: true, Error: "invalid_request"})
		return
	}
	var req Request
	if err := json.Unmarshal(frame, &req); err != nil || req.Op == "" {
		_ = json.NewEncoder(c).Encode(Response{Available: true, Error: "invalid_request"})
		return
	}
	_ = json.NewEncoder(c).Encode(s.process(req))
}

func (s *Server) process(req Request) Response {
	switch req.Op {
	case "discover":
		return s.discover()
	case "history":
		return s.historyResponse()
	case "plan":
		return s.plan(req.Payload)
	case "apply":
		return s.apply(req.Payload)
	case "rollback":
		return s.rollback(req.Payload)
	default:
		return Response{Available: true, Error: "unknown_operation"}
	}
}

func okResponse(data any) Response { return Response{OK: true, Available: true, Data: data} }

func errorResponse(err string, data any) Response {
	return Response{OK: false, Available: true, Error: err, Data: data}
}

func (s *Server) configuredPaths() []string {
	paths := append([]string(nil), s.ConfigPaths...)
	if s.ConfigPath != "" {
		paths = append(paths, s.ConfigPath)
	}
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		if _, exists := seen[path]; !exists {
			seen[path] = struct{}{}
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func (s *Server) allowedConfigPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", errors.New("invalid config path")
	}
	allowed := false
	for _, root := range s.configuredPaths() {
		if root == raw {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errors.New("config path is outside host-agent roots")
	}
	info, err := os.Lstat(raw)
	if err != nil {
		return "", errors.New("config file unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("config file must be a regular file")
	}
	return raw, nil
}

func (s *Server) discover() Response {
	clients := make([]map[string]any, 0)
	for _, path := range s.configuredPaths() {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if _, err := JSONConfig(path); err != nil {
			continue
		}
		if len(clients) == 256 {
			break
		}
		clients = append(clients, map[string]any{
			"client":       clientName(path),
			"user":         os.Getenv("USER"),
			"config_paths": []string{path},
			"evidence":     []string{"json_config"},
		})
	}
	return okResponse(map[string]any{"scope": "host", "clients": clients, "config_paths": s.configuredPaths()})
}

func clientName(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if name == "" {
		return "configured-client"
	}
	return name
}

func (s *Server) historyResponse() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return okResponse(map[string]any{"scope": "host", "changes": append([]Change(nil), s.history...)})
}

func decodePayload(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("missing_payload")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid_payload")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid_payload")
	}
	return nil
}

func validateProxyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid_proxy_url")
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func marshalConfig(value any) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

var proxyKeys = map[string]struct{}{
	"base_url": {}, "baseurl": {}, "api_base": {}, "api_base_url": {},
	"api_url": {}, "apiurl": {}, "openai_base_url": {}, "openai_api_base": {}, "proxy_url": {},
}

func isProxyKey(key string) bool {
	_, ok := proxyKeys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

func jsonPointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func replaceProxyFields(value any, path string, proxyURL string, changes *[]FieldChange) {
	switch item := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "/" + jsonPointerEscape(key)
			if isProxyKey(key) {
				if old, ok := item[key].(string); ok && old != proxyURL {
					*changes = append(*changes, FieldChange{Path: childPath, Before: safeURLPreview(old), After: proxyURL})
					item[key] = proxyURL
				}
				continue
			}
			replaceProxyFields(item[key], childPath, proxyURL, changes)
		}
	case []any:
		for index := range item {
			replaceProxyFields(item[index], fmt.Sprintf("%s/%d", path, index), proxyURL, changes)
		}
	}
}

func safeURLPreview(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[configured]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON value")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("configuration root must be an object")
	}
	return value, nil
}

func (s *Server) plan(raw json.RawMessage) Response {
	var payload planPayload
	if err := decodePayload(raw, &payload); err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Reason: err.Error()}})
	}
	if err := validateProxyURL(payload.ProxyURL); err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Reason: err.Error()}})
	}
	path, err := s.allowedConfigPath(payload.ConfigPath)
	if err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Reason: err.Error()}})
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxConfigBytes {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "config_unavailable_or_too_large"}})
	}
	original, err := readConfig(path)
	if err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "config_unreadable"}})
	}
	before, err := decodeJSON(original)
	if err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "unsupported_configuration_format"}})
	}
	after := cloneJSONValue(before)
	changes := make([]FieldChange, 0)
	replaceProxyFields(after, "", payload.ProxyURL, &changes)
	if len(changes) == 0 {
		plan := Plan{Status: "manual_required", Path: path, SHA256: sha256Hex(original), Reason: "no_supported_proxy_field", GeneratedAt: time.Now().UTC()}
		return okResponse(map[string]any{"plan": plan})
	}
	if len(changes) > 256 {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "too_many_proxy_fields"}})
	}
	newBytes, err := marshalConfig(after)
	if err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "config_encode_failed"}})
	}
	id, err := randomID()
	if err != nil {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "plan_id_unavailable"}})
	}
	plan := Plan{ID: id, Status: "planned", Path: path, SHA256: sha256Hex(original), NewSHA256: sha256Hex(newBytes), Changes: changes, GeneratedAt: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plans == nil {
		s.plans = make(map[string]pendingPlan)
	}
	for key, pending := range s.plans {
		if time.Since(pending.plan.GeneratedAt) > planTTL {
			delete(s.plans, key)
		}
	}
	if len(s.plans) >= MaxPlans {
		return okResponse(map[string]any{"plan": Plan{Status: "manual_required", Path: path, Reason: "too_many_pending_plans"}})
	}
	s.plans[id] = pendingPlan{plan: plan, content: newBytes, mode: info.Mode().Perm()}
	return okResponse(map[string]any{"plan": plan})
}

func cloneJSONValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(item))
		for key, value := range item {
			clone[key] = cloneJSONValue(value)
		}
		return clone
	case []any:
		clone := make([]any, len(item))
		for index, value := range item {
			clone[index] = cloneJSONValue(value)
		}
		return clone
	default:
		return item
	}
}

func (s *Server) apply(raw json.RawMessage) Response {
	var payload applyPayload
	if err := decodePayload(raw, &payload); err != nil || payload.Plan.Status != "planned" {
		return errorResponse("invalid_plan", map[string]any{"status": "rejected", "reason": "invalid_plan"})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, exists := s.plans[payload.Plan.ID]
	if !exists || time.Since(pending.plan.GeneratedAt) > planTTL || payload.Plan.Path != pending.plan.Path || payload.Plan.SHA256 != pending.plan.SHA256 || payload.Plan.NewSHA256 != pending.plan.NewSHA256 {
		return errorResponse("invalid_or_expired_plan", map[string]any{"status": "rejected", "reason": "invalid_or_expired_plan"})
	}
	path, err := s.allowedConfigPath(pending.plan.Path)
	if err != nil {
		return errorResponse(err.Error(), map[string]any{"status": "rejected", "reason": err.Error()})
	}
	current, err := os.ReadFile(path)
	if err != nil || sha256Hex(current) != pending.plan.SHA256 {
		return errorResponse("external_modification", map[string]any{"status": "rejected", "reason": "external_modification"})
	}
	info, err := os.Stat(path)
	if err != nil {
		return errorResponse("config_unreadable", map[string]any{"status": "rejected", "reason": "config_unreadable"})
	}
	backup, err := s.createBackup(path, current)
	if err != nil {
		return errorResponse("backup_failed", map[string]any{"status": "rejected", "reason": "backup_failed"})
	}
	latest, err := os.ReadFile(path)
	if err != nil || sha256Hex(latest) != pending.plan.SHA256 {
		return errorResponse("external_modification", map[string]any{"status": "rejected", "reason": "external_modification"})
	}
	if err := atomicWrite(path, pending.content, info.Mode().Perm(), info); err != nil {
		return errorResponse("write_failed", map[string]any{"status": "rejected", "reason": "write_failed"})
	}
	change := Change{Path: path, Backup: backup, OldSHA256: pending.plan.SHA256, NewSHA256: pending.plan.NewSHA256, Status: "applied", Mode: uint32(info.Mode().Perm()), CreatedAt: time.Now().UTC()}
	if err := s.appendHistoryLocked(change); err != nil {
		_ = atomicWrite(path, current, info.Mode().Perm(), info)
		return errorResponse("history_write_failed", map[string]any{"status": "rejected", "reason": "history_write_failed"})
	}
	delete(s.plans, payload.Plan.ID)
	return okResponse(map[string]any{"change": change})
}

func (s *Server) rollback(raw json.RawMessage) Response {
	var payload rollbackPayload
	if err := decodePayload(raw, &payload); err != nil || payload.Change.Path == "" || payload.Change.Backup == "" {
		return errorResponse("invalid_change", map[string]any{"status": "rejected", "reason": "invalid_change"})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active := false
	for _, item := range s.history {
		if item.Backup == payload.Change.Backup {
			active = item.Status == "applied"
			break
		}
	}
	if !active {
		return errorResponse("change_not_active", map[string]any{"status": "rejected", "reason": "change_not_active"})
	}
	var recorded Change
	for _, item := range s.history {
		if item.Backup == payload.Change.Backup {
			recorded = item
			break
		}
	}
	if payload.Change.Path != recorded.Path || payload.Change.NewSHA256 != recorded.NewSHA256 || payload.Change.OldSHA256 != recorded.OldSHA256 || payload.Change.Mode != recorded.Mode {
		return errorResponse("invalid_change", map[string]any{"status": "rejected", "reason": "invalid_change"})
	}
	payload.Change = recorded
	path, err := s.allowedConfigPath(payload.Change.Path)
	if err != nil {
		return errorResponse(err.Error(), map[string]any{"status": "rejected", "reason": err.Error()})
	}
	current, err := os.ReadFile(path)
	if err != nil || sha256Hex(current) != payload.Change.NewSHA256 {
		return errorResponse("external_modification", map[string]any{"status": "rejected", "reason": "external_modification"})
	}
	backup, err := s.allowedBackupPath(path, payload.Change.Backup)
	if err != nil {
		return errorResponse("invalid_backup", map[string]any{"status": "rejected", "reason": "invalid_backup"})
	}
	backupBytes, err := os.ReadFile(backup)
	if err != nil || sha256Hex(backupBytes) != payload.Change.OldSHA256 {
		return errorResponse("backup_unavailable", map[string]any{"status": "rejected", "reason": "backup_unavailable"})
	}
	info, err := os.Stat(path)
	if err != nil {
		return errorResponse("config_unreadable", map[string]any{"status": "rejected", "reason": "config_unreadable"})
	}
	if err := atomicWrite(path, backupBytes, os.FileMode(payload.Change.Mode), info); err != nil {
		return errorResponse("write_failed", map[string]any{"status": "rejected", "reason": "write_failed"})
	}
	change := payload.Change
	change.Status = "rolled_back"
	change.CreatedAt = time.Now().UTC()
	if err := s.updateHistoryLocked(change); err != nil {
		_ = atomicWrite(path, current, info.Mode().Perm(), info)
		return errorResponse("history_write_failed", map[string]any{"status": "rejected", "reason": "history_write_failed"})
	}
	return okResponse(map[string]any{"change": change})
}

func (s *Server) backupDir(path string) string {
	return filepath.Join(filepath.Dir(path), ".silencer-backups")
}

func (s *Server) createBackup(path string, data []byte) (string, error) {
	dir := s.backupDir(path)
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("backup directory is unsafe")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	if err := platformfs.MkdirPrivate(dir); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d-%s.bak", filepath.Base(path), time.Now().UnixNano(), sha256Hex(data)[:12])
	dst := filepath.Join(dir, name)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return "", closeErr
	}
	return dst, nil
}

func (s *Server) allowedBackupPath(configPath, raw string) (string, error) {
	backup, err := filepath.Abs(strings.TrimSpace(raw))
	if err != nil || filepath.Clean(backup) != backup {
		return "", errors.New("invalid backup")
	}
	root := s.backupDir(configPath)
	if filepath.Dir(backup) != root {
		return "", errors.New("invalid backup")
	}
	info, err := os.Lstat(backup)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("invalid backup")
	}
	return backup, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode, owner os.FileInfo) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".silencer-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode.Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := preserveOwner(tmp, path, owner); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := platformfs.Replace(tmpName, path); err != nil {
		return err
	}
	return platformfs.SyncDirectory(path)
}

func (s *Server) historyPath() string {
	if s.StatePath != "" {
		return s.StatePath
	}
	return ""
}

func (s *Server) loadHistory() {
	path := s.historyPath()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var history []Change
	if json.Unmarshal(b, &history) == nil {
		s.mu.Lock()
		s.history = history
		s.mu.Unlock()
	}
}

func (s *Server) appendHistoryLocked(change Change) error {
	history := append([]Change{change}, s.history...)
	if err := s.persistHistory(history); err != nil {
		return err
	}
	s.history = history
	return nil
}

func (s *Server) updateHistoryLocked(change Change) error {
	history := append([]Change(nil), s.history...)
	found := false
	for index := range history {
		if history[index].Backup == change.Backup {
			history[index] = change
			found = true
			break
		}
	}
	if !found {
		return errors.New("change not found in history")
	}
	if err := s.persistHistory(history); err != nil {
		return err
	}
	s.history = history
	return nil
}

func (s *Server) persistHistory(history []Change) error {
	path := s.historyPath()
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0600, nil)
}

func JSONConfig(path string) (map[string]any, error) {
	b, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	value, err := decodeJSON(b)
	if err != nil {
		return nil, fmt.Errorf("manual_required: %w", err)
	}
	return value.(map[string]any), nil
}

func readConfig(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxConfigBytes {
		return nil, errors.New("configuration too large")
	}
	return b, nil
}
