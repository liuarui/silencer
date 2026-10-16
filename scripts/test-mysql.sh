#!/usr/bin/env bash
set -euo pipefail

# Run the opt-in MySQL integration and migration smoke against a disposable or
# explicitly supplied database. The default gate remains self-contained and
# skips this path when no DSN is provided.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dsn="${SILENCER_TEST_MYSQL_DSN:-}"
if [[ -z "$dsn" ]]; then
  printf '%s\n' 'mysql smoke: SKIPPED (set SILENCER_TEST_MYSQL_DSN to enable)'
  exit 0
fi
if [[ -z "${SILENCER_TEST_MYSQL_DOCKER_NETWORK:-}" ]]; then
  command -v go >/dev/null 2>&1 || { echo 'mysql smoke: go is required' >&2; exit 1; }
fi

run_go() {
  if [[ -n "${SILENCER_TEST_MYSQL_DOCKER_NETWORK:-}" ]]; then
    command -v docker >/dev/null 2>&1 || { echo 'mysql smoke: docker is required for network mode' >&2; exit 1; }
    docker run --rm --network "$SILENCER_TEST_MYSQL_DOCKER_NETWORK" \
      -e SILENCER_STORAGE_MODE=mysql -e SILENCER_MYSQL_DSN="$dsn" \
      -e SILENCER_TEST_MYSQL_DSN="$dsn" \
      -v "$ROOT/gateway:/src" -w /src golang:1.22-bookworm "$@"
  else
    (cd "$ROOT/gateway" && "$@")
  fi
}

run_go go run ./cmd/migrate -mode mysql -mysql-dsn "$dsn"
run_go go run ./cmd/migrate -mode mysql -mysql-dsn "$dsn" -dry-run
run_go go test ./internal/storage -run '^TestMySQL' -count=1

printf '%s\n' 'mysql smoke: OK'
