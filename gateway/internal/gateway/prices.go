package gateway

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

type priceEntry struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

var builtinPrices = []priceEntry{
	{Model: "gpt-4o", Input: 2.5, Output: 10},
	{Model: "gpt-4o-mini", Input: 0.15, Output: 0.6},
	{Model: "gpt-4-turbo", Input: 10, Output: 30},
	{Model: "claude-3-5-sonnet", Input: 3, Output: 15},
	{Model: "claude-3-haiku", Input: 0.25, Output: 1.25},
}
var priceMu sync.RWMutex
var priceSyncedAt time.Time

func priceForModel(model string) (priceEntry, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return priceEntry{}, false
	}
	priceMu.RLock()
	defer priceMu.RUnlock()
	for _, entry := range builtinPrices {
		if strings.EqualFold(entry.Model, model) {
			return entry, true
		}
	}
	return priceEntry{}, false
}

func usageCostUSD(model string, prompt, completion int64) (float64, bool) {
	price, ok := priceForModel(model)
	if !ok || prompt < 0 || completion < 0 {
		return 0, false
	}
	return (float64(prompt)*price.Input + float64(completion)*price.Output) / 1_000_000, true
}

func (s *Server) handlePricesStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	priceMu.RLock()
	synced := priceSyncedAt
	priceMu.RUnlock()
	if synced.IsZero() {
		synced = time.Now().UTC()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "syncing": false, "last_error": "", "synced_at": synced.Unix(), "model_count": len(builtinPrices), "source": "builtin", "builtin_count": len(builtinPrices)})
}
func (s *Server) handlePricesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	priceMu.RLock()
	models := append([]priceEntry(nil), builtinPrices...)
	priceMu.RUnlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(models), "models": models})
}
func (s *Server) handlePricesSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	priceMu.Lock()
	priceSyncedAt = time.Now().UTC()
	synced := priceSyncedAt
	priceMu.Unlock()
	state := map[string]any{"ok": true, "syncing": false, "last_error": "", "synced_at": synced.Unix(), "model_count": len(builtinPrices), "source": "builtin", "builtin_count": len(builtinPrices)}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}
