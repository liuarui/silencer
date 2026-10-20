package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureReadinessSeparatesHeartbeatFromProtection(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name                              string
		heartbeat                         time.Time
		protected, closed, ready, applied bool
	}{
		{"no heartbeat", time.Time{}, true, false, false, false},
		{"stale heartbeat", now.Add(-11 * time.Second), true, false, false, false},
		{"future heartbeat", now.Add(time.Second), true, false, false, false},
		{"authorization only", now, false, false, true, false},
		{"targets applied", now, true, false, true, true},
		{"process exited", now, true, true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &captureProcess{done: make(chan struct{}), heartbeat: test.heartbeat, protected: test.protected}
			if test.closed {
				close(p.done)
			}
			alive, ready, applied := captureProcessReadiness(p, now)
			if alive == test.closed || ready != test.ready || applied != test.applied {
				t.Fatal(alive, ready, applied)
			}
		})
	}
	for _, invalid := range []string{"component", "certificate", "pipeline", "targets", "heartbeat"} {
		installed, trusted, pipeline, applied, ready := invalid != "component", invalid != "certificate", invalid != "pipeline", invalid != "targets", invalid != "heartbeat"
		if state := captureLifecycleState(true, installed, trusted, pipeline, true, ready, applied); state != "degraded" {
			t.Fatal(invalid, state)
		}
	}
	if state := captureLifecycleState(true, true, true, true, true, true, true); state != "running" {
		t.Fatal(state)
	}
	if state := captureLifecycleState(true, true, true, true, false, true, true); state != "ready" {
		t.Fatal(state)
	}
}

func TestCaptureMaintenanceRecoveryUsesActualProcessIdentity(t *testing.T) {
	now := time.Now()
	job := captureSetupJob{ID: strings.Repeat("a", 32), State: "running", Action: "update", PID: 123, CreatedAt: now.Add(-time.Minute)}
	for _, test := range []struct {
		name        string
		alive       bool
		err         error
		state, code string
	}{
		{"still running", true, nil, "running", ""},
		{"process gone", false, nil, "interrupted", "maintenance_interrupted"},
		{"probe unavailable", false, errors.New("probe failed"), "running", "maintenance_status_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := reconcileCaptureJob(job, now, func(pid int, id string) (bool, error) {
				if pid != job.PID || id != job.ID {
					t.Fatal("identity changed")
				}
				return test.alive, test.err
			})
			if result.State != test.state || result.Error != test.code {
				t.Fatal(result)
			}
		})
	}
	queued := job
	queued.PID = 0
	queued.State = "queued"
	queued.CreatedAt = now
	if result := reconcileCaptureJob(queued, now, func(int, string) (bool, error) {
		t.Fatal("startup grace must not probe a missing PID")
		return false, nil
	}); result.State != "queued" {
		t.Fatal(result)
	}
	queued.CreatedAt = now.Add(-time.Minute)
	if result := reconcileCaptureJob(queued, now, func(pid int, _ string) (bool, error) {
		if pid != 0 {
			t.Fatal(pid)
		}
		return true, nil
	}); result.State != "running" {
		t.Fatal("legacy live installer was not retained", result)
	}
}

func TestCaptureMaintenanceDoesNotOverwriteOrPermitConcurrentHostChanges(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	job := captureSetupJob{ID: strings.Repeat("b", 32), State: "queued", Action: "update", CreatedAt: time.Now().UTC()}
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(s.capture.root, "setup-job.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "authorize", "certificate", "trust_certificate", "diagnose", "detect_proxy"} {
		if err := s.captureAction(context.Background(), action); err == nil || err.Error() != "maintenance_in_progress" {
			t.Fatal(action, err)
		}
	}
	for _, path := range []string{"/api/local/capture/config", "/api/local/processing/config"} {
		if w := captureControl(t, s, "POST", path, `{}`); w.Code != 409 || !strings.Contains(w.Body.String(), "maintenance_in_progress") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	if err := s.captureAction(context.Background(), "check"); err != nil {
		t.Fatal(err)
	}
	if err := s.captureAction(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureMaintenanceCompletionCannotReplaceNewTask(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	old := captureSetupJob{ID: strings.Repeat("a", 32), State: "complete", Action: "repair"}
	newJob := captureSetupJob{ID: strings.Repeat("b", 32), State: "queued", Action: "update", CreatedAt: time.Now().UTC()}
	data, _ := json.Marshal(old)
	path := filepath.Join(s.capture.root, "setup-job.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s.capture.actionMu.Lock()
	done := make(chan struct{})
	go func() { s.capture.finishMaintenance(old); close(done) }()
	select {
	case <-done:
		s.capture.actionMu.Unlock()
		t.Fatal("completion bypassed maintenance serialization")
	case <-time.After(20 * time.Millisecond):
	}
	data, _ = json.Marshal(newJob)
	err := os.WriteFile(path, data, 0600)
	s.capture.actionMu.Unlock()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	actual, err := readCaptureJobRaw(s.capture.root)
	if err != nil || actual == nil || actual.ID != newJob.ID || actual.State != "queued" {
		t.Fatal("old completion replaced new task", actual, err)
	}
}

func TestCaptureMaintenanceUnreadableRecordBlocksNewWork(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	path := filepath.Join(s.capture.root, "setup-job.json")
	if captureMaintenanceInProgress(s.capture.root) {
		t.Fatal("missing job should not block")
	}
	for _, data := range []string{`{broken`, `{}`, `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"unknown"}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if !captureMaintenanceInProgress(s.capture.root) {
			t.Fatal("corrupt record allowed concurrent maintenance")
		}
		job := readCaptureJob(s.capture.root).(captureSetupJob)
		if job.Error != "maintenance_status_unavailable" {
			t.Fatal(job)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if !captureMaintenanceInProgress(s.capture.root) {
		t.Fatal("unreadable record allowed concurrent maintenance")
	}
}
