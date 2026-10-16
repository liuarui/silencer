package gateway

import (
	"context"
	"errors"

	"github.com/silencer/gateway/internal/storage"
)

// Engine is the opaque engine handle retained by the server. Concrete
// capabilities are expressed by MaskingEngine and StreamingEngine below;
// keeping the base interface empty prevents a no-op lifecycle hook from being
// mistaken for request inspection.
type Engine interface{}

type CloseableEngine interface{ Close() }

// MaskingEngine is the request-body masking seam. Production engines must
// implement it; a server without it rejects body-bearing data-plane requests
// before they reach the upstream.
type MaskingEngine interface {
	Engine
	Mask(context.Context, []byte) ([]byte, error)
}

// MaskingSession incrementally transforms a request body. A session may retain
// an unsafe suffix across pushes; callers must Finish or Abort exactly once.
type MaskingSession interface {
	Push(context.Context, []byte) ([]byte, error)
	Finish(context.Context) ([]byte, error)
}

type AbortableMaskingSession interface {
	MaskingSession
	Abort()
}

// ContextualMaskingSession keeps a request-local token map alive until the
// matching upstream response has been restored. Its placeholders use stable
// per-label occurrence slots, while originals remain isolated per request.
type ContextualMaskingSession interface {
	MaskingSession
	NewRestoreSession(context.Context) (RestoreSession, error)
	Release()
}

type StreamingMaskingEngine interface {
	Engine
	NewMaskingSession(context.Context) (MaskingSession, error)
}

// ReusableMaskingEngine assigns one opaque identity per entity in a trusted
// route/credential scope, retaining reverse mappings across request sessions.
type ReusableMaskingEngine interface {
	NewReusableMaskingSession(context.Context, string) (MaskingSession, error)
	ImportReusableMappings(context.Context, string, []map[string]any) error
	ClearReusableMappings() error
}

type DurableMappingStore interface {
	IssueEntityMapping(context.Context, string, string, string) (storage.EntityMapping, error)
	ResolveEntityMapping(context.Context, string, string) (storage.EntityMapping, bool, error)
}
type DurableMaskingEngine interface {
	NewDurableMaskingSession(context.Context, string, DurableMappingStore) (ContextualMaskingSession, error)
}

// MaskedItemsProvider exposes the exact per-request token map after masking is
// complete. It avoids trying to infer original spans from truncated text.
type MaskedItemsProvider interface {
	MaskedItems() ([]map[string]any, error)
}

type RestoreSession interface {
	Push(context.Context, []byte) ([]byte, error)
	Finish(context.Context) ([]byte, error)
}

// ConfigurableRestoreSession selects restoration rules before the first push.
// JSON payloads require original values to be escaped as JSON string contents.
type ConfigurableRestoreSession interface {
	RestoreSession
	SetJSONEscape(context.Context, bool) error
}

// AbortableRestoreSession lets the gateway release native stream state when a
// client disconnects before the upstream response reaches EOF.
type AbortableRestoreSession interface {
	RestoreSession
	Abort()
}

type StreamingEngine interface {
	Engine
	NewRestoreSession(context.Context) (RestoreSession, error)
}

// NewEngine selects the configured native engine. Production builds must use
// the silencer_engine tag so a missing C++ implementation fails at startup.
func NewEngine(cfg Config) (Engine, error) {
	if cfg.Engine != "cpp" && cfg.Engine != "native" {
		return nil, errors.New("SILENCER_ENGINE must be cpp or native")
	}
	return newNativeEngine(cfg)
}
