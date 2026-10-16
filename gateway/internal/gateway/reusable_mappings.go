package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

func proxyMappingScope(r *http.Request) string {
	hash := sha256.New()
	for _, value := range []string{r.Host, namedRouteName(r.URL.Path), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Api-Key")} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Server) warmReusableMappings() {
	engine, ok := s.currentEngine().(ReusableMaskingEngine)
	if !ok || s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.store.RecentMappingMetadata(ctx, time.Now().Add(-24*time.Hour), 5000)
	if err != nil {
		s.logger.Print("reusable mapping warmup unavailable")
		return
	}
	for i := len(rows) - 1; i >= 0; i-- {
		meta := eventMeta(rows[i])
		scope, _ := meta["mapping_scope"].(string)
		if meta["mapping_mode"] != "entity-v1" || len(scope) != 64 || meta["log_expired"] == true {
			continue
		}
		items, _ := meta["items"].([]any)
		var retained []map[string]any
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := item["original"].(string); !ok {
				continue
			}
			copy := cloneMap(item)
			copy["used_at"] = rows[i].CreatedAt.Unix()
			retained = append(retained, copy)
		}
		if len(retained) > 0 {
			if err := engine.ImportReusableMappings(ctx, scope, retained); err != nil {
				s.logger.Print("reusable mapping warmup incomplete")
				return
			}
		}
	}
}

func maskedItemOccurrences(item map[string]any) int {
	if count, ok := numericMetaInt(item["count"]); ok && count >= 0 {
		return count
	}
	return 1
}
