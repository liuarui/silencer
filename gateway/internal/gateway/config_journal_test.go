package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func prepareJournalTestFiles(t *testing.T, dir string) ([]*preparedConfigFile, string, string) {
	t.Helper()
	runtime := filepath.Join(dir, "runtime.json")
	control := filepath.Join(dir, "control.json")
	backup := control + ".backup-20261015-120000.123456789.json"
	var files []*preparedConfigFile
	for _, path := range []string{runtime, control, backup} {
		f, err := prepareConfigFile(path, map[string]string{"state": "new"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files, runtime, control
}
func seedJournalTestFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"runtime.json", "control.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{\"state\": \"old\"}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func assertJournalTestState(t *testing.T, dir string, old bool) {
	t.Helper()
	for _, name := range []string{"runtime.json", "control.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if old {
			if string(data) != "{\"state\": \"old\"}\n" {
				t.Fatal("previous bytes not restored")
			}
		} else if !bytes.Contains(data, []byte("new")) {
			t.Fatal("committed state lost")
		}
	}
	_, err := os.Stat(filepath.Join(dir, "control.json.backup-20261015-120000.123456789.json"))
	if old && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncommitted backup survived")
	}
	if !old && err != nil {
		t.Fatal("committed backup missing")
	}
}
func TestConfigJournalCrashRecovery(t *testing.T) {
	if dir := os.Getenv("SILENCER_JOURNAL_TEST_DIR"); dir != "" {
		files, anchor, _ := prepareJournalTestFiles(t, dir)
		stage := os.Getenv("SILENCER_JOURNAL_TEST_STAGE")
		ops := defaultConfigDiskOps()
		ops.rename = func(from, to string) error {
			if err := os.Rename(from, to); err != nil {
				return err
			}
			if (stage == "journal" && to == anchor+".transaction") || (stage == "first" && to == files[0].path) || (stage == "last" && to == files[2].path) {
				os.Exit(73)
			}
			return nil
		}
		if err := persistConfigCandidateUsing(ops, files...); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	for _, stage := range []string{"journal", "first", "last", "committed"} {
		t.Run(stage, func(t *testing.T) {
			dir := configPathIdentity(t.TempDir())
			seedJournalTestFiles(t, dir)
			cmd := exec.Command(os.Args[0], "-test.run=^TestConfigJournalCrashRecovery$")
			cmd.Env = append(os.Environ(), "SILENCER_JOURNAL_TEST_DIR="+dir, "SILENCER_JOURNAL_TEST_STAGE="+stage)
			err := cmd.Run()
			if stage == "committed" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 73 {
					t.Fatalf("crash injection failed: %v", err)
				}
			}
			anchor := filepath.Join(dir, "runtime.json")
			control := filepath.Join(dir, "control.json")
			for i := 0; i < 2; i++ {
				if err := recoverConfigJournal(anchor, control); err != nil {
					t.Fatal(err)
				}
			}
			assertJournalTestState(t, dir, stage != "committed")
		})
	}
}
func TestConfigJournalCleanupFailuresRollback(t *testing.T) {
	for _, stage := range []string{"remove", "sync-after-remove", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := configPathIdentity(t.TempDir())
			seedJournalTestFiles(t, dir)
			files, anchor, control := prepareJournalTestFiles(t, dir)
			ops := defaultConfigDiskOps()
			ops.remove = func(path string) error {
				if stage == "remove" && path == anchor+".transaction" {
					return errors.New("injected remove failure")
				}
				return os.Remove(path)
			}
			syncs := 0
			ops.syncDir = func(path string) error {
				if path == anchor+".transaction" {
					syncs++
					if stage == "sync-after-remove" && syncs == 2 {
						return errors.New("injected sync failure")
					}
				}
				return syncConfigDirectory(path)
			}
			ops.rename = func(from, to string) error {
				if stage == "rename" && to == control {
					return errors.New("injected rename failure")
				}
				return os.Rename(from, to)
			}
			if err := persistConfigCandidateUsing(ops, files...); err == nil {
				t.Fatal("fault was reported successful")
			}
			assertJournalTestState(t, dir, true)
			if err := recoverConfigJournal(anchor, control); err != nil {
				t.Fatal(err)
			}
			assertJournalTestState(t, dir, true)
		})
	}
}
func TestConfigJournalRejectsAllInvalidTargetsBeforeMutation(t *testing.T) {
	for _, kind := range []string{"unrelated", "relative", "duplicate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := configPathIdentity(t.TempDir())
			seedJournalTestFiles(t, dir)
			anchor := filepath.Join(dir, "runtime.json")
			other := filepath.Join(dir, "unrelated.json")
			os.WriteFile(other, []byte("untouched"), 0600)
			entries := []configUndoEntry{{Path: anchor, Existed: true, Previous: []byte("corrupt")}}
			second := configUndoEntry{Path: other, Existed: true, Previous: []byte("overwrite")}
			if kind == "relative" {
				second.Path = "relative.json"
			}
			if kind == "duplicate" {
				second.Path = anchor
			}
			entries = append(entries, second)
			data, _ := json.Marshal(configUndoJournal{Version: 1, Files: entries})
			if kind == "symlink" {
				os.WriteFile(other, data, 0600)
				if err := os.Symlink(other, anchor+".transaction"); err != nil {
					t.Fatal(err)
				}
			} else {
				os.WriteFile(anchor+".transaction", data, 0600)
			}
			if err := recoverConfigJournal(anchor); err == nil {
				t.Fatal("invalid journal accepted")
			}
			got, _ := os.ReadFile(anchor)
			if string(got) != "{\"state\": \"old\"}\n" {
				t.Fatal("invalid journal mutated valid first participant")
			}
			if kind != "symlink" {
				got, _ = os.ReadFile(other)
				if string(got) != "untouched" {
					t.Fatal("unrelated file mutated")
				}
			}
		})
	}
}
func TestConfigAuthAliasRejectedWithoutDeadlock(t *testing.T) {
	dir := configPathIdentity(t.TempDir())
	runtime := filepath.Join(dir, "runtime.json")
	os.WriteFile(runtime, []byte(`{}`), 0600)
	if err := os.Symlink(runtime, runtime+".auth"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILENCER_CONFIG_FILE", runtime)
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("auth/runtime alias accepted")
	}
}

func TestConfigRuntimeSymlinkKeepsOriginalSidecarPaths(t *testing.T) {
	dir := configPathIdentity(t.TempDir())
	actual := filepath.Join(dir, "actual.json")
	alias := filepath.Join(dir, "link.json")
	os.WriteFile(actual, []byte(`{}`), 0600)
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	anchor, control, auth, err := configParticipantPaths(alias, "")
	if err != nil || anchor != actual || control != alias+".control.json" || auth != alias+".auth" {
		t.Fatal("sidecars moved with runtime symlink", err)
	}
}
func TestConfigStartupRecoversAuthBeforeLoading(t *testing.T) {
	dir := configPathIdentity(t.TempDir())
	runtime := filepath.Join(dir, "runtime.json")
	os.WriteFile(runtime, []byte(`{}`), 0600)
	auth := runtime + ".auth"
	old := []byte(`{"auth_token":"previous-password"}`)
	os.WriteFile(auth, old, 0600)
	f, err := prepareConfigFile(auth, map[string]string{"auth_token": "incomplete-password"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(configUndoJournal{Version: 1, Files: []configUndoEntry{{Path: auth, Previous: f.previous, Existed: true}}})
	if err = writeConfigBytes(auth+".transaction", data); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(f.temp, auth); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILENCER_CONFIG_FILE", runtime)
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", "")
	cfg, err := LoadConfig()
	if err != nil || cfg.AuthToken != "previous-password" {
		t.Fatal("startup loaded uncommitted auth", err)
	}
}

func TestConfigJournalRejectsUnrecoverableSizeBeforeWriting(t *testing.T) {
	dir := configPathIdentity(t.TempDir())
	seedJournalTestFiles(t, dir)
	files, anchor, _ := prepareJournalTestFiles(t, dir)
	// Base64 alone fills the reader budget; the journal metadata exceeds it.
	files[0].previous = make([]byte, 48<<20)
	if err := persistConfigCandidate(files...); err == nil {
		t.Fatal("oversized journal accepted")
	}
	if _, err := os.Stat(anchor + ".transaction"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unrecoverable journal published")
	}
	assertJournalTestState(t, dir, true)
}
