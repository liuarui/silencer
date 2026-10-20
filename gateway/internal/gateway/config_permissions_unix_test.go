//go:build !windows

package gateway

import (
	"os"
	"testing"
)

func checkPrivateConfigPermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private persistence permissions: %v", err)
	}
}
