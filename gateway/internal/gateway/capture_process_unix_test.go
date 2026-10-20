//go:build !windows

package gateway

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureChildProcessVisibilityFailureCannotConfirmExit(t *testing.T) {
	if !captureGroupListedAlive(123, nil, nil) || !captureGroupListedAlive(123, nil, errors.New("cannot inspect processes")) {
		t.Fatal("unknown process group was treated as stopped")
	}
	if captureGroupListedAlive(123, []byte("123 Z\n123 Z+\n"), nil) {
		t.Fatal("reaped helper is waiting only for zombie collection")
	}
	if !captureGroupListedAlive(123, []byte("123 S\n"), nil) {
		t.Fatal("live helper was ignored")
	}
}

func TestCaptureStopConfirmsChildProcessGroupExit(t *testing.T) {
	childFile := filepath.Join(t.TempDir(), "child")
	cmd := exec.Command("/bin/sh", "-c", `sleep 60 & printf '%s' "$!" > "$1"; wait`, "fixture", childFile)
	configureCaptureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &captureProcess{cmd: cmd, done: make(chan struct{}), cancel: func() { _ = cmd.Process.Kill() }}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { _ = p.stop() })
	until := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(childFile); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if captureGroupAlive(cmd.Process.Pid) {
		t.Fatal("capture child group remains active")
	}
}
