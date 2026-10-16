package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

// BackupSQLite uses SQLite's snapshot API, including committed WAL pages.
func BackupSQLite(ctx context.Context, sourcePath, destinationPath string) error {
	return copySQLiteFile(ctx, sourcePath, destinationPath, true)
}

// RestoreSQLite atomically installs a snapshot. The destination gateway must
// be stopped; a destination that cannot leave WAL mode is refused as in use.
func RestoreSQLite(ctx context.Context, backupPath, destinationPath string, force bool) error {
	return copySQLiteFile(ctx, backupPath, destinationPath, force)
}
func sqliteFileDSN(path, mode string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String() + "?mode=" + mode + "&_busy_timeout=100"
}
func copySQLiteFile(ctx context.Context, sourcePath, destinationPath string, overwrite bool) error {
	if sourcePath == "" || destinationPath == "" {
		return fmt.Errorf("source and destination paths are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	destinationPath, err = filepath.Abs(destinationPath)
	if err != nil {
		return err
	}
	srcInfo, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}
	if !srcInfo.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular SQLite file")
	}
	dstInfo, dstErr := os.Stat(destinationPath)
	if sourcePath == destinationPath || (dstErr == nil && os.SameFile(srcInfo, dstInfo)) {
		return fmt.Errorf("source and destination must differ")
	}
	if dstErr != nil && !os.IsNotExist(dstErr) {
		return dstErr
	}
	if dstErr == nil && !overwrite {
		return fmt.Errorf("destination exists; pass --force-restore to overwrite")
	}
	if err = os.MkdirAll(filepath.Dir(destinationPath), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destinationPath), ".silencer-db-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if err = sqliteSnapshot(ctx, sourcePath, tmpPath); err != nil {
		return err
	}
	f, err := os.OpenFile(tmpPath, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if overwrite {
		// SQLite, rather than unlink, checkpoints any crash-left destination WAL.
		if dstErr == nil {
			db, e := sql.Open("sqlite3", sqliteFileDSN(destinationPath, "rw"))
			if e != nil {
				return e
			}
			var mode string
			e = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode)
			closeErr = db.Close()
			if e == nil {
				e = closeErr
			}
			if e != nil || mode != "delete" {
				return fmt.Errorf("destination must be stopped before overwrite")
			}
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, e := os.Stat(destinationPath + suffix); e == nil {
			return fmt.Errorf("destination has active SQLite sidecars")
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	if overwrite {
		return os.Rename(tmpPath, destinationPath)
	}
	// Link is an atomic no-clobber install on the destination filesystem.
	return os.Link(tmpPath, destinationPath)
}
func sqliteSnapshot(ctx context.Context, sourcePath, destinationPath string) error {
	src, err := sql.Open("sqlite3", sqliteFileDSN(sourcePath, "ro"))
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := sql.Open("sqlite3", sqliteFileDSN(destinationPath, "rw"))
	if err != nil {
		return err
	}
	defer dst.Close()
	source, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := dst.Conn(ctx)
	if err != nil {
		return err
	}
	defer destination.Close()
	return source.Raw(func(rawSource any) error {
		return destination.Raw(func(rawDestination any) error {
			backup, err := rawDestination.(*sqlite3.SQLiteConn).Backup("main", rawSource.(*sqlite3.SQLiteConn), "main")
			if err != nil {
				return err
			}
			finished := false
			defer func() {
				if !finished {
					backup.Finish()
				}
			}()
			for {
				if err = ctx.Err(); err != nil {
					return err
				}
				done, e := backup.Step(256)
				if e != nil {
					return e
				}
				if done {
					finished = true
					return backup.Finish()
				}
				timer := time.NewTimer(5 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		})
	})
}
