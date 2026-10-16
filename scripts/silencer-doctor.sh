#!/usr/bin/env bash
# Silencer server doctor
#
# Read-only checks for a host that is going to run Silencer. This script never
# changes services, files, environment variables, or client configuration.
# It deliberately reports "manual" for a tool whose configuration format is
# unknown instead of guessing and modifying the wrong profile.
set -u

json=0
if [[ "${1:-}" == "--json" ]]; then
  json=1
fi

host_home="${HOME:-}"
data_dir="${SILENCER_DATA_DIR:-./silencer_data}"
gateway_port="${SILENCER_GATEWAY_HOST_PORT:-8080}"
container_name="${SILENCER_CONTAINER_NAME:-silencer}"

declare -a names=()
declare -a states=()
declare -a details=()

add_check() {
  names+=("$1")
  states+=("$2")
  details+=("$3")
}

has_cmd() { command -v "$1" >/dev/null 2>&1; }

port_state() {
  local port="$1"
  if has_cmd ss && ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "(:|\])${port}$"; then
    printf 'occupied'
  elif has_cmd lsof && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    printf 'occupied'
  else
    printf 'free'
  fi
}

tool_state() {
  local label="$1"
  shift
  local bin
  for bin in "$@"; do
    if has_cmd "$bin"; then
      printf '%s (%s)' "found" "$bin"
      return
    fi
  done
  printf 'not-found'
}

if [[ -n "$host_home" && -d "$host_home" ]]; then
  add_check "home" "ok" "$host_home"
else
  add_check "home" "warn" "HOME is not a readable directory; run as the target user"
fi

if has_cmd docker; then
  if docker info >/dev/null 2>&1; then
    add_check "docker" "ok" "$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf 'available')"
  else
    add_check "docker" "warn" "Docker is installed but the current user cannot access the daemon"
  fi
else
  add_check "docker" "missing" "install Docker Engine/Desktop before deployment"
fi

if has_cmd docker && docker compose version >/dev/null 2>&1; then
  add_check "compose" "ok" "$(docker compose version --short 2>/dev/null || printf 'available')"
else
  add_check "compose" "warn" "docker compose plugin is not available"
fi

state="$(port_state "$gateway_port")"
if [[ "$state" == occupied ]]; then
  add_check "port_gateway" "warn" "$gateway_port is occupied"
else
  add_check "port_gateway" "ok" "$gateway_port is free"
fi

if has_cmd docker && docker inspect "$container_name" >/dev/null 2>&1; then
  running="$(docker inspect -f '{{.State.Running}}' "$container_name" 2>/dev/null || printf false)"
  if [[ "$running" == true ]]; then
    add_check "container" "ok" "$container_name is running"
  else
    add_check "container" "warn" "$container_name exists but is stopped"
  fi
else
  add_check "container" "info" "$container_name has not been created"
fi

if [[ -d "$data_dir" && -w "$data_dir" ]]; then
  add_check "data_dir" "ok" "$data_dir is writable"
elif [[ -e "$data_dir" ]]; then
  add_check "data_dir" "warn" "$data_dir exists but is not writable"
else
  add_check "data_dir" "info" "$data_dir does not exist yet; compose will create the named volume/path"
fi

hermes="$(tool_state hermes hermes)"
codex="$(tool_state codex codex codex-cli)"
opencode="$(tool_state opencode opencode)"
add_check "hermes" "${hermes%% *}" "$hermes; adapter status: manual until this version is paired"
add_check "codex" "${codex%% *}" "$codex; adapter status: manual until this version is paired"
add_check "opencode" "${opencode%% *}" "$opencode; adapter status: manual until this version is paired"

if [[ -n "$host_home" ]]; then
  for candidate in "$host_home/.hermes" "$host_home/.codex" "$host_home/.config/opencode"; do
    if [[ -d "$candidate" ]]; then
      add_check "profile_${candidate##*/}" "found" "$candidate (not read or modified)"
    fi
  done
fi

if (( json )); then
  printf '{"checks":['
  for i in "${!names[@]}"; do
    (( i > 0 )) && printf ','
    node -e 'console.log(JSON.stringify({name:process.argv[1],state:process.argv[2],detail:process.argv[3]}))' \
      "${names[$i]}" "${states[$i]}" "${details[$i]}" | tr -d '\n'
  done
  printf ']}\n'
  exit 0
fi

printf 'Silencer doctor (read-only)\n'
printf 'host=%s data_dir=%s gateway=%s\n\n' "$(uname -srm 2>/dev/null || printf unknown)" "$data_dir" "$gateway_port"
for i in "${!names[@]}"; do
  printf '%-18s %-8s %s\n' "${names[$i]}" "${states[$i]}" "${details[$i]}"
done
printf '\nNo configuration was changed. Unknown or unsupported client layouts must be paired and reviewed in the Silencer web console before applying a change.\n'
