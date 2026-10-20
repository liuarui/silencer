//go:build !windows

package gateway

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func detachMaintenance(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func configureCaptureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func startCaptureProcess(cmd *exec.Cmd) (func() error, error) { return nil, cmd.Start() }

func signalCaptureProcess(cmd *exec.Cmd, signal os.Signal) error {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		return syscall.Kill(-cmd.Process.Pid, signal.(syscall.Signal))
	}
	return cmd.Process.Signal(signal)
}

func captureGroupAlive(pid int) bool {
	probeErr := syscall.Kill(-pid, 0)
	if errors.Is(probeErr, syscall.ESRCH) {
		return false
	}
	if probeErr != nil {
		return true
	}
	// Orphaned zombies may await the host/container init reaper. They cannot
	// capture traffic; distinguish them from a still-running privileged helper.
	data, err := exec.Command("ps", "-axo", "pgid=,stat=").Output()
	return captureGroupListedAlive(pid, data, err)
}

func captureGroupListedAlive(pid int, data []byte, err error) bool {
	if err != nil {
		return true
	}
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == strconv.Itoa(pid) {
			seen = true
			if !strings.HasPrefix(fields[1], "Z") && !strings.HasPrefix(fields[1], "X") {
				return true
			}
		}
	}
	// An incomplete ps view is not proof that a privileged helper exited.
	return !seen
}

func confirmCaptureChildrenStopped(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		return nil
	}
	pid := cmd.Process.Pid
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if !captureGroupAlive(pid) {
			return nil
		}
		_ = syscall.Kill(-pid, signal)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if !captureGroupAlive(pid) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return errors.New("capture_stop_failed")
}
