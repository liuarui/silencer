package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupAndRestoreSQLite(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	db, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE values_table (value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO values_table(value) VALUES('before')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := BackupSQLite(ctx, sourcePath, backupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreSQLite(ctx, backupPath, restoredPath, false); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: restoredPath})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var value string
	if err := restored.QueryRowContext(ctx, "SELECT value FROM values_table").Scan(&value); err != nil || value != "before" {
		t.Fatalf("restored value=%q err=%v", value, err)
	}
	if err := RestoreSQLite(ctx, backupPath, restoredPath, false); err == nil {
		t.Fatal("restore overwrote existing destination without force")
	}
}

func TestSQLiteSnapshotIncludesCommittedWALAndForceRestore(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	db, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE committed(value TEXT); INSERT INTO committed VALUES('in-wal')"); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err = BackupSQLite(ctx, sourcePath, backupPath); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "target.db")
	if err = RestoreSQLite(ctx, backupPath, targetPath, false); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO committed VALUES('second')"); err != nil {
		t.Fatal(err)
	}
	if err = BackupSQLite(ctx, sourcePath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err = RestoreSQLite(ctx, backupPath, targetPath, true); err != nil {
		t.Fatal(err)
	}
	target, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: targetPath})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var n int
	if err = target.QueryRowContext(ctx, "SELECT count(*) FROM committed").Scan(&n); err != nil || n != 2 {
		t.Fatalf("committed WAL data missing: count=%d err=%v", n, err)
	}
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err = os.Symlink(sourcePath, alias); err != nil {
		t.Fatal(err)
	}
	if err = BackupSQLite(ctx, sourcePath, alias); err == nil {
		t.Fatal("source alias overwritten")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = RestoreSQLite(canceled, backupPath, targetPath, true); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestRestoreRefusesOrphanDestinationSidecars(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "source.db")
	db, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE source(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	target := filepath.Join(t.TempDir(), "target.db")
	if err = os.WriteFile(target+"-wal", []byte("old-wal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = RestoreSQLite(ctx, source, target, false); err == nil {
		t.Fatal("snapshot installed alongside stale WAL")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("refused restore created destination")
	}
}

func TestSQLiteBackupEscapedPathsAndReadOnlySource(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "db space #%")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source #% .db")
	db, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE fixture(value TEXT); INSERT INTO fixture VALUES('kept')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	snapshot := filepath.Join(root, "backup #% .db")
	if err = BackupSQLite(ctx, source, snapshot); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "restored #% .db")
	if err = RestoreSQLite(ctx, snapshot, restored, false); err != nil {
		t.Fatal(err)
	}
	ro, err := sql.Open("sqlite3", sqliteFileDSN(restored, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var value string
	if err = ro.QueryRow("SELECT value FROM fixture").Scan(&value); err != nil || value != "kept" {
		t.Fatal(value, err)
	}
	if _, err = ro.Exec("INSERT INTO fixture VALUES('forbidden')"); err == nil {
		t.Fatal("read-only URI permitted writes")
	}
	rw, err := sql.Open("sqlite3", sqliteFileDSN(filepath.Join(root, "missing.db"), "rw"))
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if err = rw.Ping(); err == nil {
		t.Fatal("rw URI created missing database")
	}
}
