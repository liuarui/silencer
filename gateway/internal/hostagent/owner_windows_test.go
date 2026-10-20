package hostagent

import (
	"os"
	"path/filepath"
	"testing"

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
