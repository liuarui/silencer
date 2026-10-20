//go:build !windows

package platformfs

import "os"

func MkdirPrivate(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}
