package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsCaptureTreeHelper(t *testing.T) {
	role := os.Getenv("SILENCER_TEST_CAPTURE_TREE_ROLE")
	if role == "" {
		return
	}
	if role != "child" {
		executable, _ := os.Executable()
		child := exec.Command(executable, "-test.run=^TestWindowsCaptureTreeHelper$")
		child.Env = append(os.Environ(), "SILENCER_TEST_CAPTURE_TREE_ROLE=child")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("SILENCER_TEST_CAPTURE_TREE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(3)
		}
		if role == "parent-exits" {
			os.Exit(0)
		}
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestWindowsCaptureProcessTreeStopsAfterParentExit(t *testing.T) {
	for _, role := range []string{"parent-lives", "parent-exits"} {
		t.Run(role, func(t *testing.T) {
			executable, _ := os.Executable()
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			cmd := exec.Command(executable, "-test.run=^TestWindowsCaptureTreeHelper$")
			cmd.Env = append(os.Environ(), "SILENCER_TEST_CAPTURE_TREE_ROLE="+role, "SILENCER_TEST_CAPTURE_TREE_PID="+pidFile)
			cleanup, err := startCaptureProcess(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			deadline := time.Now().Add(10 * time.Second)
			var childPID int
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(pidFile)
				childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				if childPID > 0 {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if childPID == 0 {
				t.Fatal("child fixture did not start")
			}
			child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(childPID))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(child)
			if role == "parent-exits" {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("parent did not exit")
				}
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if state, err := windows.WaitForSingleObject(child, 3000); err != nil || state != windows.WAIT_OBJECT_0 {
				t.Fatal("child survived capture stop", state, err)
			}
			if err := cleanup(); err != nil {
				t.Fatal("repeated stop", err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("parent remains running")
			}
		})
	}
}
