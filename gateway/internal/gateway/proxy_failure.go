package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

var errUpstreamTimeout = errors.New("upstream timeout")

// Transport timeouts can wrap DeadlineExceeded even when the inbound request
// is still live. Only its own context establishes a downstream cancellation.
func proxyFailure(ctx context.Context, err error) (int, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 499, ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return 499, err
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return http.StatusGatewayTimeout, fmt.Errorf("%w: %w", errUpstreamTimeout, err)
	}
	return http.StatusBadGateway, err
}

func (r *requestRecord) noteFailure(stage string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.done && r.failureStage == "" {
		r.failureStage = stage
	}
	r.mu.Unlock()
}
