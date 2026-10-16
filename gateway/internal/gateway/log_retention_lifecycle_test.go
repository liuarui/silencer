package gateway

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A slow session reap must finish before storage replacement or a successful
// shutdown. A deadline may return earlier, with cleanup deferred until exit.
func TestLogRetentionLifecycleWaitsForWorker(t *testing.T) {
	for _, operation := range []string{"shutdown", "deadline", "replace"} {
		t.Run(operation, func(t *testing.T) {
			s, store := extensionLedgerServer(t, testEngine{})
			blocked := &retentionBlockingRestore{started: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(blocked.release) }) }
			t.Cleanup(release)
			s.extRestore["expired"] = &extensionRestoreEntry{session: blocked, lastUsed: time.Now().Add(-25 * time.Hour)}
			s.SetStorage(store)
			done := s.logDone
			select {
			case <-blocked.started:
			case <-time.After(time.Second):
				t.Fatal("retention did not start")
			}
			result := make(chan error, 1)
			go func() {
				if operation == "replace" {
					s.SetStorage(nil)
					result <- nil
					return
				}
				ctx := context.Background()
				if operation == "deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
					defer cancel()
				}
				result <- s.Shutdown(ctx)
			}()
			if operation == "deadline" {
				select {
				case err := <-result:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("shutdown error: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("shutdown ignored its deadline")
				}
			} else {
				select {
				case err := <-result:
					t.Fatalf("returned before retention exited: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			release()
			if operation != "deadline" {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("worker exit was not acknowledged")
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("retention is still running")
			}
		})
	}
}

type retentionBlockingRestore struct{ started, release chan struct{} }

func (s *retentionBlockingRestore) Push(_ context.Context, data []byte) ([]byte, error) {
	return data, nil
}
func (*retentionBlockingRestore) Finish(context.Context) ([]byte, error) { return nil, nil }
func (s *retentionBlockingRestore) Abort() {
	close(s.started)
	<-s.release
}

func TestRetentionSkipsBusyCaptureRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SILENCER_LOG_DIR", root)
	s, _ := extensionLedgerServer(t, testEngine{})
	lock, err := lockCaptureRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	result := make(chan error, 1)
	go func() {
		result <- s.withInactiveBody(context.Background(), "0123456789abcdef0123456789abcdef", func(string) error {
			return errors.New("busy capture root was accessed")
		})
	}()
	select {
	case err := <-result:
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("busy root: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retention waited for another process's capture lock")
	}
}
