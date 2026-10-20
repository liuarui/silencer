package gateway

import (
	"golang.org/x/sys/windows"
	"runtime"
	"testing"
	"unsafe"
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

	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("configuration ACL is not protected", err)
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
			t.Fatal("configuration grants another identity access")
		}
	}
	runtime.KeepAlive(sd)
	if !seenUser || !seenSystem {
		t.Fatal("configuration must allow current user and SYSTEM")
	}
}
