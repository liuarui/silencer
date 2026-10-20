package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config controls the gateway process. Values are loaded from an optional JSON
// file first, then environment variables take precedence.
type Config struct {
	ListenAddr                   string `json:"listen_addr"`
	UpstreamURL                  string `json:"upstream_url"`
	RequireUpstream              bool   `json:"require_upstream"`
	MaxConcurrency               int    `json:"max_concurrency"`
	MaxBodyBytes                 int64  `json:"max_body_bytes"`
	MaxResponseBytes             int64  `json:"max_response_bytes"`
	StoreRawBody                 bool   `json:"store_raw_body"`
	UpstreamDialTimeoutSeconds   int    `json:"upstream_dial_timeout_seconds"`
	UpstreamHeaderTimeoutSeconds int    `json:"upstream_header_timeout_seconds"`
	ShutdownTimeoutSeconds       int    `json:"shutdown_timeout_seconds"`
	Engine                       string `json:"engine"`
	EngineLibrary                string `json:"engine_library"`
	EngineConfig                 string `json:"-"`
	WebDist                      string `json:"web_dist"`
	AuthToken                    string `json:"-"`
	AuthUsername                 string `json:"-"`
	HostAgentSocket              string `json:"host_agent_socket"`
	LocalMode                    bool   `json:"local_mode"`
	CaptureForwardEnabled        bool   `json:"capture_forward_enabled"`
	CaptureAllowedTargets        string `json:"capture_allowed_targets"`
}

// An explicitly configured runtime file remains required. Control-only
// deployments use an optional sidecar, created by the first settings update.
func runtimeConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("SILENCER_CONFIG_FILE")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("SILENCER_CONTROL_CONFIG_FILE")); p != "" {
		return p + ".runtime.json"
	}
	return ""
}

func authPersistencePath() string {
	if p := runtimeConfigPath(); p != "" {
		return p + ".auth"
	}
	return ""
}

func loadPersistedAuthToken() string {
	p := authPersistencePath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var v struct {
		AuthToken string `json:"auth_token"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.AuthToken
}

func DefaultConfig() Config {
	return Config{
		// Bind the local control/data plane to loopback by default. Deployments
		// that need network access must opt in explicitly via
		// SILENCER_GATEWAY_ADDR (and should configure SILENCER_GATEWAY_TOKEN).
		ListenAddr:                   "127.0.0.1:8080",
		UpstreamURL:                  "",
		MaxConcurrency:               64,
		MaxBodyBytes:                 64 << 20,
		MaxResponseBytes:             8 << 20,
		UpstreamDialTimeoutSeconds:   10,
		UpstreamHeaderTimeoutSeconds: 180,
		ShutdownTimeoutSeconds:       10,
		Engine:                       "cpp",
		EngineLibrary:                "",
		WebDist:                      "/app/web_dist",
		AuthUsername:                 "admin",
		HostAgentSocket:              "/run/silencer/host-agent.sock",
	}
}

// LoadConfig reads the runtime file, then applies environment overrides. Only
// the implicit control sidecar may be absent on a deployment's first start.
func LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	unlock, err := lockAndRecoverConfiguredConfig()
	if err != nil {
		return cfg, fmt.Errorf("recover configuration: %w", err)
	}
	defer unlock()
	if path := runtimeConfigPath(); path != "" {
		data, err := os.ReadFile(path)
		optionalMissing := errors.Is(err, os.ErrNotExist) && strings.TrimSpace(os.Getenv("SILENCER_CONFIG_FILE")) == ""
		if err != nil && !optionalMissing {
			return cfg, fmt.Errorf("read config file: %w", err)
		}
		if !optionalMissing {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config file: %w", err)
			}
		}
	}

	firstNonEmpty := func(current *string, names ...string) {
		for _, name := range names {
			if value := strings.TrimSpace(os.Getenv(name)); value != "" {
				*current = value
				return
			}
		}
	}
	firstNonEmpty(&cfg.ListenAddr, "SILENCER_GATEWAY_ADDR")
	firstNonEmpty(&cfg.UpstreamURL, "SILENCER_UPSTREAM_URL")
	firstNonEmpty(&cfg.Engine, "SILENCER_ENGINE")
	firstNonEmpty(&cfg.EngineLibrary, "SILENCER_ENGINE_LIB")
	firstNonEmpty(&cfg.WebDist, "SILENCER_WEB_DIST")
	firstNonEmpty(&cfg.HostAgentSocket, "SILENCER_HOST_AGENT_SOCKET")
	for name, dest := range map[string]*bool{"SILENCER_LOCAL_MODE": &cfg.LocalMode, "SILENCER_CAPTURE_FORWARD_ENABLED": &cfg.CaptureForwardEnabled} {
		if value := os.Getenv(name); value != "" {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return cfg, fmt.Errorf("%s must be boolean", name)
			}
			*dest = parsed
		}
	}
	if value := os.Getenv("SILENCER_CAPTURE_ALLOWED_TARGETS"); value != "" {
		cfg.CaptureAllowedTargets = value
	}
	if path := strings.TrimSpace(os.Getenv("SILENCER_ENGINE_CONFIG_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read engine config: %w", err)
		}
		cfg.EngineConfig = string(data)
	}
	if token := os.Getenv("SILENCER_GATEWAY_TOKEN"); token != "" {
		cfg.AuthToken = token
	}
	if persisted := loadPersistedAuthToken(); persisted != "" {
		cfg.AuthToken = persisted
	}
	if username := strings.TrimSpace(os.Getenv("SILENCER_GATEWAY_USERNAME")); username != "" {
		cfg.AuthUsername = username
	}
	for _, item := range []struct {
		name string
		dest *int
	}{
		{"SILENCER_MAX_CONCURRENCY", &cfg.MaxConcurrency},
		{"SILENCER_UPSTREAM_DIAL_TIMEOUT", &cfg.UpstreamDialTimeoutSeconds},
		{"SILENCER_UPSTREAM_HEADER_TIMEOUT", &cfg.UpstreamHeaderTimeoutSeconds},
		{"SILENCER_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeoutSeconds},
	} {
		if value := strings.TrimSpace(os.Getenv(item.name)); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("%s must be an integer: %w", item.name, err)
			}
			*item.dest = n
		}
	}
	if value := strings.TrimSpace(os.Getenv("SILENCER_MAX_BODY_BYTES")); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("SILENCER_MAX_BODY_BYTES must be a positive integer")
		}
		cfg.MaxBodyBytes = n
	}
	if value := strings.TrimSpace(os.Getenv("SILENCER_MAX_RESPONSE_BYTES")); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("SILENCER_MAX_RESPONSE_BYTES must be a positive integer")
		}
		cfg.MaxResponseBytes = n
	}
	if value := strings.TrimSpace(os.Getenv("SILENCER_STORE_RAW_BODY")); value != "" {
		store, err := strconv.ParseBool(value)
		if err != nil {
			return cfg, fmt.Errorf("SILENCER_STORE_RAW_BODY must be boolean: %w", err)
		}
		cfg.StoreRawBody = store
	}
	if value := strings.TrimSpace(os.Getenv("SILENCER_REQUIRE_UPSTREAM")); value != "" {
		required, err := strconv.ParseBool(value)
		if err != nil {
			return cfg, fmt.Errorf("SILENCER_REQUIRE_UPSTREAM must be boolean: %w", err)
		}
		cfg.RequireUpstream = required
	}
	if cfg.MaxBodyBytes < 1 {
		return cfg, fmt.Errorf("max body bytes must be positive")
	}
	if cfg.MaxResponseBytes < 1 {
		return cfg, fmt.Errorf("max response bytes must be positive")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.MaxConcurrency < 1 {
		return cfg, fmt.Errorf("max concurrency must be positive")
	}
	if cfg.ShutdownTimeoutSeconds < 1 {
		return cfg, fmt.Errorf("shutdown timeout must be positive")
	}
	if cfg.UpstreamDialTimeoutSeconds < 1 {
		return cfg, fmt.Errorf("upstream dial timeout must be positive")
	}
	if cfg.UpstreamHeaderTimeoutSeconds < 1 {
		return cfg, fmt.Errorf("upstream header timeout must be positive")
	}
	if cfg.Engine == "" {
		cfg.Engine = "cpp"
	}
	return cfg, nil
}
