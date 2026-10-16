//go:build silencer_engine && cgo

package gateway

import (
	"context"
	"errors"
	"testing"
)

type cancelAfterNativePush struct {
	inner  *nativeRestoreSession
	cancel context.CancelFunc
	aborts int
}

func (s *cancelAfterNativePush) Push(ctx context.Context, body []byte) ([]byte, error) {
	out, err := s.inner.Push(ctx, body)
	if err == nil {
		s.cancel()
	}
	return out, err
}
func (s *cancelAfterNativePush) Finish(ctx context.Context) ([]byte, error) {
	return s.inner.Finish(ctx)
}
func (s *cancelAfterNativePush) Abort() { s.aborts++; s.inner.Abort() }

func TestNativeTerminalCancellationReleasesRestoreChannel(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	for _, kind := range []string{"sse", "ndjson"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var wrapped *cancelAfterNativePush
			protocol := newProtocolRestoreSession(kind, func(ctx context.Context) (RestoreSession, error) {
				inner, err := engine.(*nativeEngine).NewRestoreSession(ctx)
				if err != nil {
					return nil, err
				}
				wrapped = &cancelAfterNativePush{inner: inner.(*nativeRestoreSession), cancel: cancel}
				return wrapped, nil
			})
			defer protocol.Abort()
			frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic\"},\"finish_reason\":\"stop\"}]}\n\n"
			if kind == "ndjson" {
				frame = "{\"response\":\"synthetic\",\"done\":true}\n"
			}
			if _, err := protocol.Push(ctx, []byte(frame)); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
			protocol.Abort()
			if wrapped == nil || wrapped.aborts != 1 || wrapped.inner.id != 0 || len(protocol.channels) != 0 {
				t.Fatal("canceled terminal frame retained a native restore channel")
			}
		})
	}
}

func TestNativeCanceledFinishReleasesStandaloneRestore(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	session, err := engine.(*nativeEngine).NewRestoreSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.Finish(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("finish: %v", err)
	}
	if session.(*nativeRestoreSession).id != 0 {
		t.Fatal("canceled Finish left native stream open")
	}
}
