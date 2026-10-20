package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func readCaptureJobRaw(root string) (*captureSetupJob, error) {
	data, err := os.ReadFile(filepath.Join(root, "setup-job.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("maintenance_status_unavailable")
	}
	var job captureSetupJob
	if json.Unmarshal(data, &job) != nil || len(job.ID) != 32 || strings.Trim(job.ID, "0123456789abcdef") != "" {
		return nil, errors.New("maintenance_status_unavailable")
	}
	switch job.State {
	case "queued", "running", "complete", "failed", "interrupted":
	default:
		return nil, errors.New("maintenance_status_unavailable")
	}
	return &job, nil
}

func reconcileCaptureJob(job captureSetupJob, now time.Time, probe func(int, string) (bool, error)) captureSetupJob {
	if job.State != "queued" && job.State != "running" {
		return job
	}
	// Give startup/finalizers a short window. After this, a timestamp alone is
	// never treated as proof that a maintenance process is still alive.
	recent := !job.CreatedAt.IsZero() && now.Sub(job.CreatedAt) >= 0 && now.Sub(job.CreatedAt) < 15*time.Second
	if job.PID > 0 || !recent {
		alive, err := probe(job.PID, job.ID)
		if err != nil {
			job.Error = "maintenance_status_unavailable"
			return job
		}
		if alive {
			job.State = "running"
			job.Error = ""
			return job
		}
	}
	if recent {
		return job
	}
	job.State, job.Error = "interrupted", "maintenance_interrupted"
	return job
}

func readCaptureJob(root string) any {
	job, err := readCaptureJobRaw(root)
	if err != nil {
		// An unreadable record is not evidence that a previous installer exited.
		return captureSetupJob{State: "running", Error: "maintenance_status_unavailable"}
	}
	if job == nil {
		return nil
	}
	// Older in-progress records have no creation time. The file timestamp
	// supplies the same short startup grace, without making it a lifetime lease.
	if job.CreatedAt.IsZero() {
		if info, err := os.Stat(filepath.Join(root, "setup-job.json")); err == nil {
			job.CreatedAt = info.ModTime()
		}
	}
	return reconcileCaptureJob(*job, time.Now(), func(pid int, id string) (bool, error) { return captureMaintenanceProcessAlive(root, pid, id) })
}

func captureMaintenanceInProgress(root string) bool {
	job, ok := readCaptureJob(root).(captureSetupJob)
	return ok && (job.State == "queued" || job.State == "running")
}

func captureMaintenanceProcessAlive(root string, pid int, id string) (bool, error) {
	if pid < 0 || len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return false, errors.New("maintenance_status_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	name := "setup-" + id + ".sh"
	if runtime.GOOS == "windows" {
		name = "setup-" + id + ".ps1"
		filter := "Name = 'powershell.exe'"
		if pid > 0 {
			filter = "ProcessId = " + strconv.Itoa(pid)
		}
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false); $ErrorActionPreference='Stop'; Get-CimInstance Win32_Process -Filter \""+filter+"\" | Select-Object -ExpandProperty CommandLine")
	} else {
		if pid > 0 {
			cmd = exec.CommandContext(ctx, "ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=")
		} else {
			cmd = exec.CommandContext(ctx, "ps", "-ww", "-axo", "command=")
		}
	}
	data, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if runtime.GOOS != "windows" && ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, errors.New("maintenance_status_unavailable")
	}
	if pid > 0 {
		return strings.Contains(string(data), name), nil
	}
	// Recover old records without PIDs only after checking for installers rooted
	// at this installation. Never expose or log other processes' command lines.
	legacy := "install.sh"
	if runtime.GOOS == "windows" {
		legacy = "install.ps1"
	}
	return strings.Contains(string(data), filepath.Join(root, name)) || strings.Contains(string(data), filepath.Join(root, legacy)), nil
}
