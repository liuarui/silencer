package hostagent

import (
	"os"
	"runtime"

	"github.com/silencer/gateway/internal/platformfs"
	"golang.org/x/sys/windows"
)

func preserveOwner(file *os.File, path string, owner os.FileInfo) error {
	if owner == nil {
		return platformfs.SetPrivatePermissions(file.Name())
	}
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags)
	if err != nil {
		return err
	}
	user, _, err := sd.Owner()
	if err != nil {
		return err
	}
	group, _, err := sd.Group()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		flags |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	err = windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT, flags, user, group, dacl, nil)
	runtime.KeepAlive(sd)
	return err
}
