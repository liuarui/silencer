package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func releaseUpstreamServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, err := NewServer(Config{Engine: "go-fallback", MaxConcurrency: 4}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); _ = s.Shutdown(context.Background()) })
	return s, ts
}

func TestNamedRouteReusesConnectionsWithFreshHeadersAndTargets(t *testing.T) {
	type observed struct {
		address, mode, auth, cookie, internal, ledgerID, host string
	}
	seen := make(chan observed, 8)
	var closed atomic.Int32
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{r.RemoteAddr, r.Header.Get("X-Provider-Mode"), r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Silencer-Token"), r.Header.Get("X-Silencer-Request-ID"), r.Host}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "ok")
	}))
	up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Add(1)
		}
	}
	up.Start()
	defer up.Close()
	s, gateway := releaseUpstreamServer(t)
	configure := func(mode string) {
		s.configMu.Lock()
		s.control["upstreams"] = []any{
			map[string]any{"name": "one", "target": up.URL, "extra_headers": map[string]any{
				"X-Provider-Mode": mode, "X-Silencer-Token": "must-not-leak", "X-Silencer-Request-ID": "fake-id",
				"Cookie": "silencer_session=must-not-leak; provider=keep", "Host": "wrong.invalid",
				"Content-Length": "999", "Connection": "close", "X-Bad\nName": "bad", "X-Bad-Value": "bad\r\nInjected: true",
			}},
			map[string]any{"name": "two", "target": up.URL, "extra_headers": map[string]any{"X-Provider-Mode": "other-route"}},
		}
		s.running = true
		s.configMu.Unlock()
	}
	get := func(route string) observed {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, gateway.URL+"/r/"+route+"/models", nil)
		req.Header.Set("Authorization", "Bearer provider-key")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != 200 || string(body) != "ok" {
			t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, readErr)
		}
		return <-seen
	}
	configure("first")
	first := get("one")
	configure("second")
	second := get("one")
	other := get("two")
	configure("")
	removed := get("one")
	if first.address != second.address || first.address != other.address || first.address != removed.address {
		t.Fatal("named routes failed to reuse the upstream connection")
	}
	if first.mode != "first" || second.mode != "second" || other.mode != "other-route" || removed.mode != "" {
		t.Fatal("stale or cross-route provider headers")
	}
	if first.auth != "Bearer provider-key" || first.cookie != "provider=keep" || first.internal != "" || first.ledgerID == "fake-id" || first.host != strings.TrimPrefix(up.URL, "http://") {
		t.Fatal("provider header injection changed routing or leaked console credentials")
	}
	newUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "new-target")
	}))
	defer newUp.Close()
	s.configMu.Lock()
	s.control["upstreams"] = []any{map[string]any{"name": "one", "target": newUp.URL}}
	s.configMu.Unlock()
	resp, err := gateway.Client().Get(gateway.URL + "/r/one/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "new-target" {
		t.Fatalf("target update did not apply: %q", body)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("Shutdown retained the shared idle upstream connection")
	}
}

func TestUpstreamProbeHTTPFailuresAreNotSuccesses(t *testing.T) {
	for _, mode := range []string{"models", "chat"} {
		for _, status := range []int{200, 201, 302, 401, 403, 404, 429, 500, 502} {
			t.Run(fmt.Sprintf("%s/%d", mode, status), func(t *testing.T) {
				var called atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called.Add(1)
					if r.Header.Get("X-Provider-Mode") != "configured" {
						t.Error("probe omitted configured provider header")
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "private error text must not be returned")
				}))
				defer up.Close()
				s, gateway := releaseUpstreamServer(t)
				s.configMu.Lock()
				s.control["upstreams"] = []any{map[string]any{"name": "test", "target": up.URL, "extra_headers": map[string]any{"X-Provider-Mode": "configured"}}}
				s.configMu.Unlock()
				payload := fmt.Sprintf(`{"name":"test","mode":%q,"model":"synthetic"}`, mode)
				resp, err := gateway.Client().Post(gateway.URL+"/api/upstream/test", "application/json", strings.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var result struct {
					OK                       bool `json:"ok"`
					Status                   int  `json:"status"`
					Mode, Error, ProbeTarget string
				}
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
					t.Fatal(err)
				}
				if result.OK != (status >= 200 && status < 300) || result.Status != status || result.Mode != mode || called.Load() != 1 {
					t.Fatalf("wrong probe classification: %+v, calls=%d", result, called.Load())
				}
				if !result.OK && result.Error != "upstream_http_error" {
					t.Fatalf("missing safe HTTP failure: %+v", result)
				}
			})
		}
	}
}

func TestGatewayProbeDoesNotContactUpstreamOrRequireRoute(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer up.Close()
	s, gateway := releaseUpstreamServer(t)
	s.configMu.Lock()
	s.control["upstreams"] = []any{map[string]any{"name": "configured", "target": up.URL}}
	s.configMu.Unlock()
	for _, name := range []string{"configured", "missing"} {
		resp, err := gateway.Client().Post(gateway.URL+"/api/upstream/test", "application/json", strings.NewReader(fmt.Sprintf(`{"mode":"port","name":%q}`, name)))
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil || result["ok"] != true || result["probe_target"] != "gateway" || result["status"] != nil || calls.Load() != 0 {
			t.Fatalf("gateway-only probe escaped to upstream: result=%v calls=%d err=%v", result, calls.Load(), err)
		}
	}
}
