#!/usr/bin/env bash
set -euo pipefail

# Start and switch two isolated gateway projects. The script owns candidate
# lifecycle and atomically writes an Nginx upstream include; Nginx remains the
# long-lived public listener and is reloaded only when explicitly configured.

usage() {
  cat <<'EOF'
usage: silencer-blue-green.sh <up|smoke|cutover|rollback|down> [blue|green]

Environment:
  SILENCER_BLUE_PORT       Candidate port for blue (default: 18080)
  SILENCER_GREEN_PORT      Candidate port for green (default: 18081)
  SILENCER_ACTIVE_FILE     State file (default: .silencer-active)
  SILENCER_UPSTREAM_FILE   Nginx include path (default: /tmp/silencer-upstream.conf)
  SILENCER_LOCK_DIR        Cutover lock directory (default: <active>.lock)
  SILENCER_NGINX_TEST_CMD  Syntax check before reload (default: nginx -t)
  SILENCER_NGINX_RELOAD_CMD Optional command run after cutover
  SILENCER_STORAGE_MODE    Must be mysql for concurrent production instances
  SILENCER_SMOKE_URL       Optional data-plane URL to POST during smoke
  SILENCER_SMOKE_BODY      Optional file containing the smoke request body
EOF
}

die() { printf 'blue-green: %s\n' "$*" >&2; exit 1; }
has_cmd() { command -v "$1" >/dev/null 2>&1; }

command_name="${1:-}"
color="${2:-}"
case "$command_name" in
  up|smoke|cutover|down) [[ "$color" == blue || "$color" == green ]] || { usage >&2; exit 2; } ;;
  rollback) [[ -z "$color" ]] || { usage >&2; exit 2; } ;;
  *) usage >&2; exit 2 ;;
esac

has_cmd docker || die "docker is required"
has_cmd curl || die "curl is required"
docker compose version >/dev/null 2>&1 || die "docker compose is required"

compose_file="${SILENCER_COMPOSE_FILE:-docker-compose.yml}"
project_prefix="${SILENCER_PROJECT_PREFIX:-silencer}"
blue_port="${SILENCER_BLUE_PORT:-18080}"
green_port="${SILENCER_GREEN_PORT:-18081}"
active_file="${SILENCER_ACTIVE_FILE:-.silencer-active}"
upstream_file="${SILENCER_UPSTREAM_FILE:-/tmp/silencer-upstream.conf}"
storage_mode="${SILENCER_STORAGE_MODE:-sqlite}"
lock_dir="${SILENCER_LOCK_DIR:-${active_file}.lock}"
nginx_test_cmd="${SILENCER_NGINX_TEST_CMD:-nginx -t}"
nginx_reload_cmd="${SILENCER_NGINX_RELOAD_CMD:-}"
lock_held=false

if [[ "$storage_mode" == mysql && -z "${SILENCER_MYSQL_DSN:-}" && "$compose_file" != "docker-compose.mysql.yml" ]]; then
  die "SILENCER_MYSQL_DSN is required for blue-green MySQL candidates"
fi
if [[ "$storage_mode" == mysql && "$compose_file" == "docker-compose.mysql.yml" && -z "${SILENCER_MYSQL_DSN:-}" ]]; then
  die "set SILENCER_MYSQL_DSN to a shared, reachable MySQL database before blue-green cutover"
fi

case "$color" in
  blue) port="$blue_port" ;;
  green) port="$green_port" ;;
  *) port="" ;;
esac

project_for() { printf '%s-%s' "$project_prefix" "$1"; }
url_for() { printf 'http://127.0.0.1:%s' "$1"; }

release_lock() {
  if [[ "$lock_held" != true ]]; then
    return
  fi
  if [[ -f "$lock_dir/pid" && "$(cat "$lock_dir/pid" 2>/dev/null || true)" == "$$" ]]; then
    rm -f "$lock_dir/pid"
    rmdir "$lock_dir" 2>/dev/null || true
  fi
  lock_held=false
}

acquire_lock() {
  local attempts owner
  mkdir -p "$(dirname "$lock_dir")"
  for attempts in $(seq 1 "${SILENCER_LOCK_ATTEMPTS:-100}"); do
    if mkdir "$lock_dir" 2>/dev/null; then
      printf '%s\n' "$$" >"$lock_dir/pid"
      lock_held=true
      trap release_lock EXIT INT TERM
      return 0
    fi
    owner="$(cat "$lock_dir/pid" 2>/dev/null || true)"
    if [[ -n "$owner" && ! "$owner" =~ ^[0-9]+$ ]]; then
      die "invalid cutover lock owner in $lock_dir"
    fi
    if [[ -n "$owner" ]] && ! kill -0 "$owner" 2>/dev/null; then
      rm -f "$lock_dir/pid"
      rmdir "$lock_dir" 2>/dev/null || true
      continue
    fi
    sleep "${SILENCER_LOCK_INTERVAL:-0.1}"
  done
  die "another blue-green state change holds $lock_dir"
}

wait_ready() {
  local url="$1" body='' attempt
  for attempt in $(seq 1 "${SILENCER_READY_ATTEMPTS:-30}"); do
    body="$(curl -fsS --max-time 2 "$url/readyz" 2>/dev/null || true)"
    if [[ "$body" == *'"ok":true'* && "$body" == *'"storage_ok":true'* && "$body" == *'"upstream_configured":true'* ]]; then
      return 0
    fi
    sleep "${SILENCER_READY_INTERVAL:-1}"
  done
  printf '%s\n' "$body" >&2
  return 1
}

compose() {
  local target="$1"
  shift
  SILENCER_GATEWAY_HOST_PORT="$port" \
  SILENCER_REQUIRE_UPSTREAM=true \
    docker compose -p "$(project_for "$target")" -f "$compose_file" "$@"
}

smoke() {
  local target="$1" target_port="$2" base
  base="$(url_for "$target_port")"
  wait_ready "$base" || die "$target candidate did not become ready"
  if [[ -n "${SILENCER_SMOKE_URL:-}" ]]; then
    [[ -n "${SILENCER_SMOKE_BODY:-}" && -f "$SILENCER_SMOKE_BODY" ]] || die "SILENCER_SMOKE_BODY must name a readable file"
    curl -fsS --max-time "${SILENCER_SMOKE_TIMEOUT:-10}" \
      -H 'Content-Type: application/json' \
      --data-binary "@$SILENCER_SMOKE_BODY" \
      "${base}${SILENCER_SMOKE_URL}" >/dev/null || die "$target data-plane smoke failed"
  fi
}

write_upstream() {
  local target="$1" target_port="$2" dir tmp backup_dir
  local upstream_existed=false active_existed=false previous_existed=false
  dir="$(dirname "$upstream_file")"
  mkdir -p "$dir"
  backup_dir="$(mktemp -d "$dir/.silencer-cutover.XXXXXX")"
  if [[ -e "$upstream_file" ]]; then
    upstream_existed=true
    cp "$upstream_file" "$backup_dir/upstream"
  fi
  if [[ -e "$active_file" ]]; then
    active_existed=true
    cp "$active_file" "$backup_dir/active"
  fi
  if [[ -e "${active_file}.previous" ]]; then
    previous_existed=true
    cp "${active_file}.previous" "$backup_dir/previous"
  fi
  tmp="$(mktemp "$dir/.silencer-upstream.XXXXXX")"
  cat >"$tmp" <<EOF
# Generated by scripts/silencer-blue-green.sh. Do not edit.
upstream silencer_gateway {
    server 127.0.0.1:${target_port};
    keepalive 64;
}
EOF
  mv -f "$tmp" "$upstream_file"
  printf '%s\n' "$target" >"${active_file}.next"
  if [[ -f "$active_file" ]]; then
    cp "$active_file" "${active_file}.previous"
  fi
  mv -f "${active_file}.next" "$active_file"
  if [[ -n "$nginx_reload_cmd" ]]; then
    local nginx_failure=false
    if ! sh -c "$nginx_test_cmd"; then
      printf 'blue-green: Nginx syntax check failed; restoring previous cutover state\n' >&2
      nginx_failure=true
    elif ! sh -c "$nginx_reload_cmd"; then
      printf 'blue-green: Nginx reload failed; restoring previous cutover state\n' >&2
      nginx_failure=true
    fi
    if [[ "$nginx_failure" == true ]]; then
      if [[ "$upstream_existed" == true ]]; then
        cp "$backup_dir/upstream" "$upstream_file"
      else
        rm -f "$upstream_file"
      fi
      if [[ "$active_existed" == true ]]; then
        cp "$backup_dir/active" "$active_file"
      else
        rm -f "$active_file"
      fi
      if [[ "$previous_existed" == true ]]; then
        cp "$backup_dir/previous" "${active_file}.previous"
      else
        rm -f "${active_file}.previous"
      fi
      # A reload command may have applied the new file before returning an
      # error. Best effort a second syntax-checked reload after restoring the
      # old state so the public listener follows the durable state files.
      if sh -c "$nginx_test_cmd" >/dev/null 2>&1; then
        sh -c "$nginx_reload_cmd" >/dev/null 2>&1 || true
      fi
      rm -rf "$backup_dir"
      return 1
    fi
  fi
  rm -rf "$backup_dir"
}

case "$command_name" in
  up)
    [[ "$storage_mode" == mysql || "${SILENCER_BLUE_GREEN_ALLOW_SQLITE:-false}" == true ]] || \
      die "blue-green requires SILENCER_STORAGE_MODE=mysql; set SILENCER_BLUE_GREEN_ALLOW_SQLITE=true only for isolated testing"
    compose "$color" up -d --build --no-deps silencer
    smoke "$color" "$port"
    printf '%s candidate is ready on %s\n' "$color" "$(url_for "$port")"
    ;;
  smoke)
    smoke "$color" "$port"
    printf '%s candidate smoke passed\n' "$color"
    ;;
  cutover)
    acquire_lock
    smoke "$color" "$port"
    write_upstream "$color" "$port"
    printf 'cut over to %s on port %s; reload the public Nginx listener if configured\n' "$color" "$port"
    ;;
  rollback)
    acquire_lock
    [[ -f "${active_file}.previous" ]] || die "no previous color recorded"
    previous="$(tr -d '[:space:]' <"${active_file}.previous")"
    [[ "$previous" == blue || "$previous" == green ]] || die "invalid previous color"
    case "$previous" in
      blue) previous_port="$blue_port" ;;
      green) previous_port="$green_port" ;;
    esac
    smoke "$previous" "$previous_port"
    write_upstream "$previous" "$previous_port"
    printf 'rolled back to %s on port %s\n' "$previous" "$previous_port"
    ;;
  down)
    compose "$color" down
    ;;
esac
