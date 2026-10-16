package gateway

import (
	"context"
	"io"
	"sync"
	"testing"
)

func TestMaskingReadCloserSealsBlockedUpload(t *testing.T) {
	source := &repairBlockedUpload{started: make(chan struct{}), closed: make(chan struct{})}
	session := &repairSealableSession{}
	record := &requestRecord{}
	r := &maskingReadCloser{record: record, source: source, session: session, ctx: context.Background(), maxBytes: 100}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.Read(make([]byte, 20)) }()
	<-source.started
	if err := r.stopForResponse(); err != nil {
		t.Fatal(err)
	}
	if !record.requestBodyIncomplete {
		t.Fatal("incomplete upload not marked in ledger")
	}
	if !session.sealed {
		t.Fatal("mapping was not sealed")
	}
	r.Close()
	<-done
	if session.pushed {
		t.Fatal("source bytes processed after seal")
	}
}

type repairBlockedUpload struct {
	started, closed chan struct{}
	once            sync.Once
}

func (b *repairBlockedUpload) Read(p []byte) (int, error) {
	close(b.started)
	<-b.closed
	copy(p, "secret")
	return 6, io.EOF
}
func (b *repairBlockedUpload) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type repairSealableSession struct{ sealed, pushed bool }

func (s *repairSealableSession) Seal() error { s.sealed = true; return nil }
func (s *repairSealableSession) Push(context.Context, []byte) ([]byte, error) {
	s.pushed = true
	return nil, nil
}
func (s *repairSealableSession) Finish(context.Context) ([]byte, error) { return nil, nil }
