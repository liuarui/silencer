package hostagent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/silencer/gateway/internal/platformfs"
	"golang.org/x/sys/windows"
)

func TestWindowsAtomicWritePreservesACLAndReportsCommittedSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := platformfs.SetPrivatePermissions(path); err != nil {
		t.Fatal(err)
	}
	previous, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("updated"), 0600, info); err != nil {
		t.Fatal("committed configuration reported failure", err)
	}
	updated, err := os.ReadFile(path)
	if err != nil || string(updated) != "updated" {
		t.Fatal("new configuration not committed", err)
	}
	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || current.String() != previous.String() {
		t.Fatal("replacement changed the original ACL", err)
	}
}

func TestWindowsSocketDirectoryAndBackupsArePrivate(t *testing.T) {
	dir, err := os.MkdirTemp("", "ha-acl-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	server := &Server{Socket: filepath.Join(dir, "a.sock")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("socket server did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat(server.Socket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("socket did not start", err)
	}
	backup, err := server.createBackup(filepath.Join(dir, "client.json"), []byte("synthetic private value"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Dir(backup), backup} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}

		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 2 {
			t.Fatal("expected two private access entries", err)
		}
		system, err := windows.StringToSid("S-1-5-18")
		if err != nil {
			t.Fatal(err)
		}
		seenUser, seenSystem := false, false
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.IsValid() {
				t.Fatal("unexpected private access entry")
			}
			if sid.Equals(user.User.Sid) {
				seenUser = true
			} else if sid.Equals(system) {
				seenSystem = true
			} else {
				t.Fatal("host-agent files grant another identity access")
			}
		}
		runtime.KeepAlive(sd)
		if !seenUser || !seenSystem {
			t.Fatal("private files must allow current user and SYSTEM")
		}
	}
}
