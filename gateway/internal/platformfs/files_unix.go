//go:build !windows

package platformfs

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

const (
	Shared    = 1
	Exclusive = 2
	Nonblock  = 4
)

func OpenNoFollow(path string, flags int, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
func Lock(f *os.File, mode int) error { return unix.Flock(int(f.Fd()), mode) }
func IsBusy(err error) bool           { return errors.Is(err, unix.EWOULDBLOCK) }
func Replace(old, new string) error   { return os.Rename(old, new) }
func SyncDirectory(path string) error {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
