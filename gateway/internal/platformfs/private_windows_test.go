package platformfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateStateRepairsInheritedAndExplicitPublicACLs(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "capture")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "credentials.json")
	if err := os.WriteFile(file, []byte("synthetic test credential"), 0600); err != nil {
		t.Fatal(err)
	}
	public, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := public.DACL()
	if err := windows.SetNamedSecurityInfo(file, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := MkdirPrivate(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, file} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, _ := sd.Control()
		if control&windows.SE_DACL_PROTECTED == 0 || strings.Contains(sd.String(), ";;;WD)") || strings.Contains(sd.String(), ";;;BU)") {
			t.Fatal("private state retained public access", sd.String())
		}
	}
	if _, err := os.ReadFile(file); err != nil {
		t.Fatal("current user lost state access", err)
	}
}
