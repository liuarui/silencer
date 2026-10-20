package platformfs

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

const (
	Shared    = 1
	Exclusive = 2
	Nonblock  = 4
)

func OpenNoFollow(path string, flags int, mode os.FileMode) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	if flags&os.O_RDWR != 0 {
		access |= windows.GENERIC_WRITE
	} else if flags&os.O_WRONLY != 0 {
		access = windows.GENERIC_WRITE
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		disposition = windows.OPEN_ALWAYS
	}
	if flags&os.O_EXCL != 0 {
		disposition = windows.CREATE_NEW
	}
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, disposition, windows.FILE_FLAG_OPEN_REPARSE_POINT, nilHandle())
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("reparse points are not allowed")
	}
	return os.NewFile(uintptr(h), path), nil
}
func nilHandle() windows.Handle { return 0 }
func Lock(f *os.File, mode int) error {
	flags := uint32(0)
	if mode&Exclusive != 0 {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if mode&Nonblock != 0 {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
}
func IsBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
func Replace(old, new string) error {
	from, err := windows.UTF16PtrFromString(old)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(new)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// File contents are flushed by the caller and metadata replacement uses
// MOVEFILE_WRITE_THROUGH; Windows does not expose POSIX directory fsync.
func SyncDirectory(path string) error {
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("parent is not a directory")
	}
	return nil
}
