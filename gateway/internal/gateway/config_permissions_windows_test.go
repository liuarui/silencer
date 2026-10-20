package gateway

import (
	"golang.org/x/sys/windows"
	"strings"
	"testing"
)

func checkPrivateConfigPermissions(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	value := sd.String()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 || strings.Count(value, "(A;") != 2 || !strings.Contains(value, ";;;SY)") || !strings.Contains(value, ";;;"+user.User.Sid.String()+")") {
		t.Fatal("configuration must allow only current user and SYSTEM", value, err)
	}
}
