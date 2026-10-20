#!/usr/bin/env bash
set -euo pipefail
umask 077
capture_sandbox=''
capture_uninstall=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --uninstall) capture_uninstall=true; shift;;
    --sandbox) [[ $# -ge 2 && "$2" == /* ]] || exit 2; capture_sandbox="$2"; shift 2;;
    *) echo 'Unknown installer argument' >&2; exit 2;;
  esac
done
# Lifecycle tests use a marked temporary directory. This branch cannot dispatch
# native service, network, health-check or browser commands, and never sets HOME.
if [[ -n "$capture_sandbox" ]]; then
  capture_sandbox="$(cd "$capture_sandbox" && pwd -P)"
  capture_temp_base="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
  case "$capture_sandbox" in /tmp/*|/private/tmp/*|"$capture_temp_base"/*) ;; *) echo 'Sandbox must be temporary' >&2; exit 2;; esac
  [[ -f "$capture_sandbox/.silencer-installer-sandbox" && "$(cat "$capture_sandbox/.silencer-installer-sandbox")" == silencer-installer-test-v1 ]] || exit 2
  [[ -z "$(find "$capture_sandbox" -type l -print -quit)" ]] || { echo 'Sandbox symlinks are forbidden' >&2; exit 2; }
  unset SILENCER_SETUP_JOB_ID SILENCER_SETUP_JOB_FILE SILENCER_SETUP_JOB_ACTION SILENCER_SETUP_JOB_CREATED_AT
  capture_system="$(cat "$capture_sandbox/platform")"
  capture_machine=x86_64
else
  capture_system="$(uname -s)"
  capture_machine="$(uname -m)"
fi
case "$capture_system" in Darwin) capture_os=darwin;; Linux) capture_os=linux;; *) echo 'Unsupported system' >&2; exit 1;; esac
case "$capture_machine" in arm64|aarch64) capture_arch=arm64;; x86_64) capture_arch=amd64;; *) echo 'Unsupported architecture' >&2; exit 1;; esac
capture_root="${SILENCER_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/silencer}"
if [[ "$capture_os" == darwin && -z "${SILENCER_DATA_DIR:-}" ]]; then capture_root="$HOME/Library/Application Support/Silencer"; fi
capture_label=com.silencer.local
if [[ "$capture_os" == darwin ]]; then
  capture_service_dir="$HOME/Library/LaunchAgents"
  capture_service_name="$capture_label.plist"
else
  capture_service_dir="$HOME/.config/systemd/user"
  capture_service_name=silencer-local.service
fi
if [[ -n "$capture_sandbox" ]]; then
  capture_root="$capture_sandbox/data"
  capture_service_dir="$capture_sandbox/services"
fi
capture_service_file="$capture_service_dir/$capture_service_name"
capture_service() {
  if [[ -n "$capture_sandbox" ]]; then
    printf '%s\n' "$*" >> "$capture_sandbox/operations"
    if [[ -f "$capture_sandbox/fail-service" && "$*" == *"$(cat "$capture_sandbox/fail-service")"* ]]; then rm "$capture_sandbox/fail-service"; return 1; fi
    case "$*" in
      'systemctl --user show silencer-local.service -p ActiveState --value')
        if [[ -f "$capture_sandbox/service-active-state" ]]; then cat "$capture_sandbox/service-active-state"; else printf 'inactive\n'; fi;;
      'systemctl --user show silencer-local.service -p MainPID --value'|'systemctl --user show silencer-local.service -p ControlPID --value') printf '0\n';;
      'ps -axo command=') if [[ -f "$capture_sandbox/service-processes" ]]; then cat "$capture_sandbox/service-processes"; fi;;
    esac
    return 0
  fi
  command "$@"
}
capture_check_service_owner() {
  [[ -z "$capture_sandbox" ]] || return 0
  local capture_loaded_service capture_load_state capture_expected_root
  if [[ "$capture_os" == darwin ]]; then
    if [[ -e "$capture_service_file" ]]; then
      [[ "$(/usr/bin/plutil -extract ProgramArguments.1 raw -o - "$capture_service_file")" == "$capture_root/launch.sh" ]] || return 1
    fi
    # Loaded jobs can outlive their service files. Verify the actual job too.
    if capture_loaded_service="$(launchctl print "gui/$(id -u)/$capture_label" 2>/dev/null)"; then
      printf '%s\n' "$capture_loaded_service" | awk -v path="$capture_root/launch.sh" '{ gsub(/^[[:space:]]+|[[:space:]]+$/, ""); if ($0 == path) found=1 } END { exit !found }' || return 1
    fi
  else
    command -v systemctl >/dev/null || return 1
    capture_load_state="$(systemctl --user show silencer-local.service -p LoadState --value)" || return 1
    if [[ -e "$capture_service_file" ]]; then
      capture_expected_root="${capture_root//\\/\\\\}"
      capture_expected_root="${capture_expected_root//\"/\\\"}"
      capture_expected_root="${capture_expected_root//%/%%}"
      grep -Fx -- "ExecStart=/bin/bash \"$capture_expected_root/launch.sh\"" "$capture_service_file" >/dev/null || return 1
    fi
    if [[ "$capture_load_state" != not-found ]]; then
      [[ "$capture_load_state" == loaded ]] || return 1
      capture_loaded_service="$(systemctl --user show silencer-local.service -p ExecStart --value)" || return 1
      printf '%s\n' "$capture_loaded_service" | awk -v path="$capture_root/launch.sh" '{ n=index($0,path); if (n && substr($0,n-1,1) ~ /[ "]/ && substr($0,n+length(path),1) ~ /[ ";]/) found=1 } END { exit !found }' || return 1
    fi
  fi
}
capture_linux_service_stopped() {
  local capture_state capture_main capture_control capture_processes
  capture_state="$(capture_service systemctl --user show silencer-local.service -p ActiveState --value)" || return 1
  case "$capture_state" in inactive|failed) ;; *) return 1;; esac
  capture_main="$(capture_service systemctl --user show silencer-local.service -p MainPID --value)" || return 1
  capture_control="$(capture_service systemctl --user show silencer-local.service -p ControlPID --value)" || return 1
  [[ "$capture_main" == 0 && "$capture_control" == 0 ]] || return 1
  capture_processes="$(capture_service ps -axo command=)" || return 1
  if printf '%s\n' "$capture_processes" | awk -v root="$capture_root/versions/" 'index($0,root) == 1 { found=1 } END { exit !found }'; then return 1; fi
}
capture_uninstall_stage() {
  # Deterministic interruption points exist only inside the already validated
  # temporary sandbox. Production never reads a failure switch or changes HOME.
  if [[ -n "$capture_sandbox" && -f "$capture_sandbox/stop-at" && "$(cat "$capture_sandbox/stop-at")" == "$1" ]]; then
    rm "$capture_sandbox/stop-at"
    kill -TERM "$$"
  fi
}
capture_fetch() {
  if [[ -n "$capture_sandbox" ]]; then cp "$capture_sandbox/release/$1" "$2"; return; fi
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' "$release_base/$1" -o "$2"
}
capture_job_finish() {
  capture_code="$1"
  if [[ "${SILENCER_SETUP_JOB_ID:-}" =~ ^[a-f0-9]{32}$ && -n "${SILENCER_SETUP_JOB_FILE:-}" ]]; then
    capture_job_state=complete
    [[ "$capture_code" == 0 ]] || capture_job_state=failed
    printf '{"id":"%s","action":"%s","state":"%s"}\n' "$SILENCER_SETUP_JOB_ID" "${SILENCER_SETUP_JOB_ACTION:-update}" "$capture_job_state" > "$SILENCER_SETUP_JOB_FILE.installer-new"
    mv "$SILENCER_SETUP_JOB_FILE.installer-new" "$SILENCER_SETUP_JOB_FILE"
  fi
}
capture_job_start() {
  if [[ "${SILENCER_SETUP_JOB_ID:-}" =~ ^[a-f0-9]{32}$ && -n "${SILENCER_SETUP_JOB_FILE:-}" ]]; then
    case "${SILENCER_SETUP_JOB_ACTION:-}" in update|repair|uninstall) ;; *) exit 2;; esac
    # The standalone script owns this PID even if its launching gateway exits.
    printf '{"id":"%s","action":"%s","state":"running","pid":%s,"created_at":"%s"}\n' "$SILENCER_SETUP_JOB_ID" "$SILENCER_SETUP_JOB_ACTION" "$$" "${SILENCER_SETUP_JOB_CREATED_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" > "$SILENCER_SETUP_JOB_FILE.installer-start"
    mv "$SILENCER_SETUP_JOB_FILE.installer-start" "$SILENCER_SETUP_JOB_FILE"
    sleep 1
  fi
}
capture_tmp=''
capture_committed=false
capture_installed_version=''
capture_replaced_version=''
capture_restore() {
  if [[ "$capture_os" == linux && ! -f "$capture_tmp/rollback/service" ]]; then capture_service systemctl --user disable --now silencer-local.service || true; fi
  if [[ -n "$capture_installed_version" ]]; then rm -rf "$capture_installed_version" || return 1; fi
  if [[ -n "$capture_replaced_version" ]]; then mv "$capture_tmp/rollback/replaced-version" "$capture_replaced_version" || return 1; fi
  for capture_item in current launch.sh release-base .silencer-local-install; do
    if [[ -f "$capture_tmp/rollback/$capture_item" ]]; then cp -p "$capture_tmp/rollback/$capture_item" "$capture_root/$capture_item" || return 1; else rm -f "$capture_root/$capture_item" || return 1; fi
  done
  if [[ -f "$capture_tmp/rollback/service" ]]; then cp -p "$capture_tmp/rollback/service" "$capture_service_file" || return 1; else rm -f "$capture_service_file" || return 1; fi
  if [[ "$capture_os" == darwin ]]; then
    capture_service launchctl bootout "gui/$(id -u)/$capture_label" 2>/dev/null || true
    if [[ -f "$capture_service_file" ]]; then capture_service launchctl bootstrap "gui/$(id -u)" "$capture_service_file" || true; fi
  else
    capture_service systemctl --user daemon-reload || true
    if [[ -f "$capture_service_file" ]]; then capture_service systemctl --user restart silencer-local.service || true; fi
  fi
}
capture_finalize() {
  capture_status=$?
  trap - EXIT
  set +e
  if [[ "$capture_status" != 0 && "$capture_committed" == true ]]; then
    if ! capture_restore; then
      echo "Installation rollback failed; recovery files retained at $capture_tmp" >&2
      capture_tmp=''
    fi
  fi
  if [[ -n "$capture_tmp" ]]; then rm -rf "$capture_tmp"; fi
  capture_job_finish "$capture_status"
  exit "$capture_status"
}
trap capture_finalize EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
capture_job_start
capture_check_service_owner || { echo 'Existing service belongs to another installation; no service was changed.' >&2; exit 1; }
if [[ "$capture_uninstall" == true ]]; then
  [[ -f "$capture_root/.silencer-local-install" && "$(cat "$capture_root/.silencer-local-install")" == "$capture_os" ]] || { echo 'Not a managed Silencer installation' >&2; exit 1; }
  if [[ "$capture_os" == darwin ]]; then
    if ! capture_service launchctl bootout "gui/$(id -u)/$capture_label"; then
      if capture_service launchctl print "gui/$(id -u)/$capture_label" >/dev/null 2>&1; then echo 'Service could not stop' >&2; exit 1; fi
    fi
  else
    capture_service systemctl --user disable --now silencer-local.service || true
    capture_linux_service_stopped || { echo 'Service stop could not be confirmed; installation retained for retry.' >&2; exit 1; }
  fi
  capture_uninstall_stage service-stopped
  rm -f "$capture_service_file"
  capture_uninstall_stage service-file-removed
  if [[ "$capture_os" == linux ]]; then capture_service systemctl --user daemon-reload; fi
  rm -f "$capture_root/launch.sh"
  capture_uninstall_stage launcher-removed
  rm -f "$capture_root/current"
  capture_uninstall_stage current-removed
  # Keep the marker if any earlier removal failed, allowing a safe retry.
  rm -f "$capture_root/.silencer-local-install"
  capture_uninstall_stage marker-removed
  echo 'Service removed; certificates, settings, logs and mappings retained.'
  exit 0
fi
release_base="${SILENCER_RELEASE_BASE:-}"
if [[ -n "$capture_sandbox" ]]; then release_base=https://sandbox.invalid; fi
if [[ -z "$release_base" && -f "$capture_root/release-base" ]]; then release_base="$(cat "$capture_root/release-base")"; fi
if [[ -z "$release_base" ]]; then release_base=https://github.com/liuarui/silencer/releases/latest/download; fi
[[ "$release_base" == https://* ]] || { echo 'Set SILENCER_RELEASE_BASE to your HTTPS release URL.' >&2; exit 1; }
if [[ ! -d "$capture_root" ]]; then mkdir -m 700 -p "$capture_root"; fi
mkdir -p "$capture_root/versions"
capture_tmp="$(mktemp -d "$capture_root/versions/.download.XXXXXX")"
capture_file="silencer-local-$capture_os-$capture_arch.tar.gz"
capture_fetch SHA256SUMS "$capture_tmp/SHA256SUMS"
capture_hash="$(awk -v name="$capture_file" '$2 == name {print $1}' "$capture_tmp/SHA256SUMS")"
[[ "$capture_hash" =~ ^[a-f0-9]{64}$ ]] || { echo 'Unsupported platform or invalid release checksum' >&2; exit 1; }
capture_fetch "$capture_file" "$capture_tmp/bundle.tar.gz"
if command -v sha256sum >/dev/null; then capture_actual="$(sha256sum "$capture_tmp/bundle.tar.gz" | awk '{print $1}')"; else capture_actual="$(shasum -a 256 "$capture_tmp/bundle.tar.gz" | awk '{print $1}')"; fi
[[ "$capture_hash" == "$capture_actual" ]] || { echo 'Artifact checksum mismatch' >&2; exit 1; }
tar -tzf "$capture_tmp/bundle.tar.gz" | awk '/^\// || /(^|\/)\.\.(\/|$)/ {exit 1}'
if [[ -n "$capture_sandbox" ]]; then tar -tvzf "$capture_tmp/bundle.tar.gz" | awk '$1 ~ /^[lh]/ || / link to / {exit 1}'; fi
mkdir "$capture_tmp/package"
tar -xzf "$capture_tmp/bundle.tar.gz" -C "$capture_tmp/package"
capture_valid_bundle() { [[ -x "$1/silencer-gateway" && -x "$1/runtime/mitmdump" && -f "$1/web_dist/index.html" ]]; }
capture_valid_bundle "$capture_tmp/package" || { echo 'Incomplete native bundle' >&2; exit 1; }
if [[ -f "$capture_service_file" && ! -f "$capture_root/.silencer-local-install" ]]; then echo 'Existing unmanaged service conflicts with Silencer' >&2; exit 1; fi
mkdir -p "$capture_tmp/rollback" "$capture_service_dir"
for capture_item in current launch.sh release-base .silencer-local-install; do
  if [[ -f "$capture_root/$capture_item" ]]; then cp -p "$capture_root/$capture_item" "$capture_tmp/rollback/$capture_item"; fi
done
if [[ -f "$capture_service_file" ]]; then cp -p "$capture_service_file" "$capture_tmp/rollback/service"; fi
capture_committed=true
if [[ -e "$capture_root/versions/$capture_hash" ]]; then
  mv "$capture_root/versions/$capture_hash" "$capture_tmp/rollback/replaced-version"
  capture_replaced_version="$capture_root/versions/$capture_hash"
fi
mv "$capture_tmp/package" "$capture_root/versions/$capture_hash"
capture_installed_version="$capture_root/versions/$capture_hash"
printf '%s\n' "$capture_hash" > "$capture_root/current.new"
mv "$capture_root/current.new" "$capture_root/current"
printf '%s\n' "$release_base" > "$capture_root/release-base"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\numask 077\ncapture_root=%q\n' "$capture_root"
  cat <<'LAUNCH'
version="$(cat "$capture_root/current")"
[[ "$version" =~ ^[a-f0-9]{64}$ ]] || exit 1
bundle="$capture_root/versions/$version"
export SILENCER_LOCAL_MODE=true SILENCER_GATEWAY_ADDR=127.0.0.1:18711
export SILENCER_INSTALLATION_ID="$version"
export SILENCER_CONTROL_CONFIG_FILE="$capture_root/control.json"
export SILENCER_WEB_DIST="$bundle/web_dist"
export SILENCER_SQLITE_PATH="$capture_root/silencer.db"
export SILENCER_MITMDUMP_PATH="$bundle/runtime/mitmdump"
export LD_LIBRARY_PATH="$bundle/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export DYLD_LIBRARY_PATH="$bundle/lib${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}"
exec "$bundle/silencer-gateway"
LAUNCH
} > "$capture_root/launch.sh"
chmod 700 "$capture_root/launch.sh"
if [[ "$capture_os" == darwin ]]; then
  capture_xml_root="$(printf '%s' "$capture_root" | sed 's/\&/\&amp;/g;s/</\&lt;/g;s/>/\&gt;/g;s/"/\&quot;/g')"
  cat > "$capture_service_file" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>$capture_label</string><key>ProgramArguments</key><array><string>/bin/bash</string><string>$capture_xml_root/launch.sh</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>AbandonProcessGroup</key><true/><key>StandardErrorPath</key><string>$capture_xml_root/service.log</string></dict></plist>
PLIST
  capture_service launchctl bootout "gui/$(id -u)/$capture_label" 2>/dev/null || true
  capture_service launchctl bootstrap "gui/$(id -u)" "$capture_service_file"
else
  if [[ -z "$capture_sandbox" ]]; then command -v systemctl >/dev/null || { echo 'systemd user services are required' >&2; exit 1; }; fi
  capture_service systemctl --user import-environment DISPLAY WAYLAND_DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS 2>/dev/null || true
  capture_unit_root="${capture_root//\\/\\\\}"
  capture_unit_root="${capture_unit_root//\"/\\\"}"
  capture_unit_root="${capture_unit_root//%/%%}"
  cat > "$capture_service_file" <<UNIT
[Unit]
Description=Silencer local privacy gateway
[Service]
ExecStart=/bin/bash "$capture_unit_root/launch.sh"
KillMode=process
Restart=on-failure
RestartSec=5
[Install]
WantedBy=default.target
UNIT
  capture_service systemctl --user daemon-reload
  capture_service systemctl --user enable silencer-local.service
  capture_service systemctl --user restart silencer-local.service
fi
capture_ready=false
if [[ -n "$capture_sandbox" ]]; then
  [[ -f "$capture_sandbox/fail-ready" ]] || capture_ready=true
else
  for capture_attempt in {1..30}; do
    capture_identity="$(curl --fail --silent --noproxy '*' --max-time 1 'http://127.0.0.1:18711/local-readyz' || true)"
    if [[ "$capture_identity" == "$capture_hash" ]]; then capture_ready=true; break; fi
    sleep 1
  done
fi
[[ "$capture_ready" == true ]] || { echo 'Local gateway failed to become ready' >&2; exit 1; }
printf '%s\n' "$capture_os" > "$capture_root/.silencer-local-install"
capture_committed=false
capture_url="http://127.0.0.1:18711/#/capture?mode=${SILENCER_SETUP_MODE:-local}&resume=1"
if [[ -n "$capture_sandbox" ]]; then printf '%s\n' "browser $capture_url" >> "$capture_sandbox/operations"; elif [[ "$capture_os" == darwin ]]; then open "$capture_url" || echo "$capture_url"; else xdg-open "$capture_url" 2>/dev/null || echo "$capture_url"; fi
