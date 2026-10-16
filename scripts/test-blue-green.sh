#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/silencer-blue-green-test.XXXXXX")"
fake_bin="$tmp_dir/bin"
upstream_file="$tmp_dir/upstream.conf"
active_file="$tmp_dir/active"
mkdir -p "$fake_bin"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT

printf '%s\n' 'old upstream configuration' >"$upstream_file"
printf '%s\n' blue >"$active_file"
printf '%s\n' legacy >"${active_file}.previous"

cat >"$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == compose && "${2:-}" == version ]]; then
  exit 0
fi
exit 0
EOF
cat >"$fake_bin/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' '{"ok":true,"storage_ok":true,"upstream_configured":true}'
EOF
chmod +x "$fake_bin/docker" "$fake_bin/curl"

set +e
PATH="$fake_bin:$PATH" \
SILENCER_STORAGE_MODE=mysql \
SILENCER_MYSQL_DSN='user:pass@tcp(db:3306)/silencer' \
SILENCER_UPSTREAM_URL='https://upstream.example.test' \
SILENCER_UPSTREAM_FILE="$upstream_file" \
SILENCER_ACTIVE_FILE="$active_file" \
SILENCER_NGINX_TEST_CMD=':' \
SILENCER_NGINX_RELOAD_CMD='exit 17' \
  "$ROOT/scripts/silencer-blue-green.sh" cutover green
failure_status=$?
set -e
[[ "$failure_status" -ne 0 ]] || { echo 'reload failure unexpectedly succeeded' >&2; exit 1; }
[[ "$(cat "$upstream_file")" == 'old upstream configuration' ]] || { echo 'upstream state was not restored' >&2; exit 1; }
[[ "$(cat "$active_file")" == 'blue' ]] || { echo 'active state was not restored' >&2; exit 1; }
[[ "$(cat "${active_file}.previous")" == 'legacy' ]] || { echo 'previous state was not restored' >&2; exit 1; }

PATH="$fake_bin:$PATH" \
SILENCER_STORAGE_MODE=mysql \
SILENCER_MYSQL_DSN='user:pass@tcp(db:3306)/silencer' \
SILENCER_UPSTREAM_URL='https://upstream.example.test' \
SILENCER_UPSTREAM_FILE="$upstream_file" \
SILENCER_ACTIVE_FILE="$active_file" \
SILENCER_NGINX_TEST_CMD=':' \
SILENCER_NGINX_RELOAD_CMD=':' \
  "$ROOT/scripts/silencer-blue-green.sh" cutover green
[[ "$(cat "$active_file")" == 'green' ]] || { echo 'successful cutover did not update active state' >&2; exit 1; }
[[ "$(cat "${active_file}.previous")" == 'blue' ]] || { echo 'successful cutover did not retain previous state' >&2; exit 1; }
rg -q 'server 127\.0\.0\.1:18081;' "$upstream_file" || { echo 'successful cutover did not write upstream' >&2; exit 1; }

# A syntax-check failure must restore both durable state files.
set +e
PATH="$fake_bin:$PATH" \
SILENCER_STORAGE_MODE=mysql \
SILENCER_MYSQL_DSN='user:pass@tcp(db:3306)/silencer' \
SILENCER_UPSTREAM_URL='https://upstream.example.test' \
SILENCER_UPSTREAM_FILE="$upstream_file" \
SILENCER_ACTIVE_FILE="$active_file" \
SILENCER_NGINX_TEST_CMD='exit 17' \
SILENCER_NGINX_RELOAD_CMD=':' \
  "$ROOT/scripts/silencer-blue-green.sh" cutover blue
syntax_status=$?
set -e
[[ "$syntax_status" -ne 0 ]] || { echo 'syntax failure unexpectedly succeeded' >&2; exit 1; }
[[ "$(cat "$active_file")" == 'green' ]] || { echo 'syntax failure changed active state' >&2; exit 1; }
rg -q 'server 127\.0\.0\.1:18081;' "$upstream_file" || { echo 'syntax failure changed upstream state' >&2; exit 1; }

# A second state-changing process must not race the first one.
lock_dir="${active_file}.lock"
mkdir "$lock_dir"
printf '%s\n' "$$" >"$lock_dir/pid"
set +e
PATH="$fake_bin:$PATH" \
SILENCER_STORAGE_MODE=mysql \
SILENCER_MYSQL_DSN='user:pass@tcp(db:3306)/silencer' \
SILENCER_UPSTREAM_FILE="$upstream_file" \
SILENCER_ACTIVE_FILE="$active_file" \
SILENCER_LOCK_ATTEMPTS=1 \
  "$ROOT/scripts/silencer-blue-green.sh" cutover blue
lock_status=$?
set -e
rm -rf "$lock_dir"
[[ "$lock_status" -ne 0 ]] || { echo 'concurrent cutover unexpectedly succeeded' >&2; exit 1; }
printf '%s\n' 'test-blue-green: OK'
