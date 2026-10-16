// Command migrate initializes and validates the Go storage schema. The
// rewritten runtime uses the existing SQLite/MySQL tables in place, so the
// migration is deliberately additive and never deletes source data.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func main() {
	mode := flag.String("mode", "sqlite", "storage mode: sqlite or mysql")
	sqlitePath := flag.String("sqlite-path", "", "SQLite database path")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN")
	copyData := flag.Bool("copy", false, "copy rows from an explicit source database")
	sourceMode := flag.String("source-mode", "sqlite", "source storage mode: sqlite or mysql")
	sourceSQLitePath := flag.String("source-sqlite-path", "", "source SQLite database path")
	sourceMySQLDSN := flag.String("source-mysql-dsn", "", "source MySQL DSN")
	batchSize := flag.Int("batch-size", 500, "migration batch size")
	verify := flag.Bool("verify", true, "verify copied rows after each write")
	configSource := flag.String("config-source", "", "legacy JSON config to migrate")
	configDestination := flag.String("config-destination", "", "destination runtime JSON config path")
	backupPath := flag.String("backup", "", "create a SQLite backup before migration")
	restorePath := flag.String("restore", "", "restore a SQLite backup instead of migrating")
	forceRestore := flag.Bool("force-restore", false, "allow restore to overwrite an existing SQLite database")
	dryRun := flag.Bool("dry-run", false, "report pending migrations without applying them")
	flag.Parse()
	if *mode != "sqlite" && *mode != "mysql" {
		log.Fatalf("unsupported mode %q", *mode)
	}
	_ = os.Setenv("SILENCER_STORAGE_MODE", *mode)
	if *sqlitePath != "" {
		_ = os.Setenv("SILENCER_SQLITE_PATH", *sqlitePath)
	}
	if *mysqlDSN != "" {
		_ = os.Setenv("SILENCER_MYSQL_DSN", *mysqlDSN)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if *configSource != "" || *configDestination != "" {
		if *configSource == "" || *configDestination == "" {
			log.Fatalf("config-source and config-destination must be provided together")
		}
		report, err := storage.MigrateConfigFile(*configSource, *configDestination)
		if err != nil {
			log.Fatalf("migrate config: %v", err)
		}
		result := map[string]any{"ok": true, "config_migrated": true, "report": report}
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
		return
	}
	if *backupPath != "" && *restorePath != "" {
		log.Fatalf("backup and restore cannot be used together")
	}
	if *restorePath != "" {
		if *mode != "sqlite" {
			log.Fatalf("restore is supported only for SQLite")
		}
		target := *sqlitePath
		if target == "" {
			target = "silencer.db"
		}
		if err := storage.RestoreSQLite(ctx, *restorePath, target, *forceRestore); err != nil {
			log.Fatalf("restore: %v", err)
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "restored": true, "path": target})
		fmt.Println(string(data))
		return
	}
	if *dryRun {
		version, err := storage.MigrationStatusFromEnv(ctx)
		if err != nil {
			log.Fatalf("migrate dry-run: %v", err)
		}
		result := map[string]any{"ok": true, "mode": *mode, "dry_run": true, "current_version": version, "latest_version": storage.LatestSchemaVersion, "pending": storage.LatestSchemaVersion - version}
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
		return
	}
	if *backupPath != "" {
		if *mode != "sqlite" {
			log.Fatalf("backup is supported only for SQLite")
		}
		source := *sqlitePath
		if source == "" {
			source = "silencer.db"
		}
		if err := storage.BackupSQLite(ctx, source, *backupPath); err != nil {
			log.Fatalf("backup: %v", err)
		}
	}
	store, err := storage.OpenFromEnv(ctx)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	defer store.Close()
	if err := store.Health(ctx); err != nil {
		log.Fatalf("migrate health check: %v", err)
	}
	if *copyData {
		source, err := storage.OpenDatabase(ctx, storage.DatabaseConfig{Mode: *sourceMode, SQLitePath: *sourceSQLitePath, MySQLDSN: *sourceMySQLDSN})
		if err != nil {
			log.Fatalf("migrate source: %v", err)
		}
		defer source.Close()
		report, err := store.CopyFrom(ctx, source, storage.CopyOptions{SourceMode: *sourceMode, BatchSize: *batchSize, Verify: *verify})
		if err != nil {
			log.Fatalf("migrate copy: %v", err)
		}
		result := map[string]any{"ok": report.Verified, "mode": *mode, "source_mode": *sourceMode, "copied": true, "report": report}
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
		if !report.Verified {
			os.Exit(1)
		}
		return
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		log.Fatalf("migrate schema version: %v", err)
	}
	result := map[string]any{"ok": true, "mode": *mode, "message": "schema ready", "current_version": version, "latest_version": storage.LatestSchemaVersion}
	data, _ := json.Marshal(result)
	fmt.Println(string(data))
}
