//go:build !windows

package hostagent

import (
	"os"
	"syscall"
)

func preserveOwner(file *os.File, _ string, owner os.FileInfo) error {
	if owner != nil {
		if stat, ok := owner.Sys().(*syscall.Stat_t); ok {
			return file.Chown(int(stat.Uid), int(stat.Gid))
		}
	}
	return nil
}
