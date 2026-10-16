#!/usr/bin/env bash
set -euo pipefail

# Exercise the migration binary against disposable SQLite/config files. This
# keeps migration regressions in the same gate as the Go/C++ runtime checks.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/silencer-migration-smoke.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

db="$WORK/silencer.db"
backup="$WORK/silencer.backup.db"
restored="$WORK/restored.db"
legacy="$WORK/legacy.json"
runtime="$WORK/runtime.json"

printf '%s\n' '{"upstream_url":"https://example.test/v1","max_concurrency":4,"session_ttl":900,"api_key":"drop-me","builtin_rules":{"email":true}}' >"$legacy"

(cd "$ROOT/gateway" && go run ./cmd/migrate -mode sqlite -sqlite-path "$db")
(cd "$ROOT/gateway" && go run ./cmd/migrate -mode sqlite -sqlite-path "$db" -dry-run)
(cd "$ROOT/gateway" && go run ./cmd/migrate -mode sqlite -sqlite-path "$db" -backup "$backup")
(cd "$ROOT/gateway" && go run ./cmd/migrate -mode sqlite -sqlite-path "$restored" -restore "$backup" -force-restore)
(cd "$ROOT/gateway" && go run ./cmd/migrate -config-source "$legacy" -config-destination "$runtime")

test -s "$backup"
test -s "$restored"
test -s "$runtime"
test -s "$runtime.control.json"
grep -q 'upstream_url' "$runtime"
grep -q 'builtin_rules' "$runtime.control.json"
grep -q 'session_ttl' "$runtime.control.json"
if grep -q 'drop-me' "$runtime" "$runtime.control.json"; then
  echo "migration smoke: secret value leaked" >&2
  exit 1
fi

echo "migration smoke: OK"
