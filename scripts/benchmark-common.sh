#!/usr/bin/env bash

# Shared, secret-safe environment metadata for local benchmark reports.
benchmark_metadata() {
  local profile="$1" url="$2" body_file="${3:-}" safe_url revision dirty cpu_count memory_total memory_limit cpu_max memory_bytes
  safe_url="$(printf '%s' "$url" | sed -E 's#(https?://)[^/@[:space:]]+:[^/@[:space:]]+@#\1[redacted]@#; s/[?].*$//')"
  local root
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  revision="$(git -C "$root" rev-parse HEAD 2>/dev/null || printf unknown)"
  if git -C "$root" diff --quiet --ignore-submodules HEAD -- 2>/dev/null; then dirty=false; else dirty=true; fi
  cpu_count="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || printf unknown)"
  memory_total=unknown
  if [[ -r /proc/meminfo ]]; then
    memory_total="$(awk '/^MemTotal:/ {printf "%d MiB", $2 / 1024}' /proc/meminfo)"
  elif command -v sysctl >/dev/null 2>&1; then
    memory_bytes="$(sysctl -n hw.memsize 2>/dev/null || true)"
    [[ "$memory_bytes" =~ ^[0-9]+$ ]] && memory_total="$((memory_bytes / 1048576)) MiB"
  fi
  memory_limit=unknown
  for file in /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory/memory.limit_in_bytes; do
    if [[ -r "$file" ]]; then
      memory_limit="$(cat "$file")"
      [[ "$memory_limit" == max ]] && memory_limit=unlimited
      break
    fi
  done
  cpu_max=unknown
  if [[ -r /sys/fs/cgroup/cpu.max ]]; then
    cpu_max="$(cat /sys/fs/cgroup/cpu.max)"
  elif [[ -r /sys/fs/cgroup/cpu/cpu.cfs_quota_us && -r /sys/fs/cgroup/cpu/cpu.cfs_period_us ]]; then
    cpu_max="$(cat /sys/fs/cgroup/cpu/cpu.cfs_quota_us) $(cat /sys/fs/cgroup/cpu/cpu.cfs_period_us)"
  fi
  printf 'profile=%s\n' "$profile"
  printf 'started_at_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'endpoint=%s\n' "$safe_url"
  printf 'git_revision=%s\ngit_worktree_dirty=%s\n' "$revision" "$dirty"
  printf 'build_id=%s\n' "${SILENCER_BENCH_BUILD_ID:-unknown}"
  printf 'os=%s\nhost=%s\n' "$(uname -srm)" "$(hostname 2>/dev/null || printf unknown)"
  printf 'logical_cpus=%s\nhost_memory=%s\n' "$cpu_count" "$memory_total"
  printf 'cgroup_cpu_max=%s\ncgroup_memory_limit_bytes=%s\n' "$cpu_max" "$memory_limit"
  printf 'storage_mode=%s\n' "${SILENCER_STORAGE_MODE:-unknown}"
  if command -v ab >/dev/null 2>&1; then
    printf 'ab_version=%s\n' "$(ab -V 2>&1 | head -1)"
  else
    printf 'ab_version=unavailable\n'
  fi
  printf 'curl_version=%s\n' "$(curl --version 2>&1 | head -1 || printf unavailable)"
  if [[ -n "$body_file" && -r "$body_file" ]]; then
    if command -v sha256sum >/dev/null 2>&1; then
      printf 'body_sha256=%s\n' "$(sha256sum "$body_file" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      printf 'body_sha256=%s\n' "$(shasum -a 256 "$body_file" | awk '{print $1}')"
    else
      printf 'body_sha256=unavailable\n'
    fi
    printf 'body_bytes=%s\n' "$(wc -c <"$body_file" | tr -d ' ')"
  else
    printf 'body_sha256=none\nbody_bytes=0\n'
  fi
  [[ -z "${SILENCER_BENCH_IMAGE:-}" ]] || printf 'image=%s\n' "$SILENCER_BENCH_IMAGE"
  if [[ -n "${SILENCER_BENCH_IMAGE:-}" ]] && command -v docker >/dev/null 2>&1; then
    image_id="$(docker image inspect "$SILENCER_BENCH_IMAGE" --format '{{.Id}}' 2>/dev/null || true)"
    [[ -z "$image_id" ]] || printf 'image_id=%s\n' "$image_id"
  fi
  if [[ -n "${SILENCER_BENCH_BINARY:-}" && -r "${SILENCER_BENCH_BINARY}" ]]; then
    if command -v sha256sum >/dev/null 2>&1; then
      printf 'binary_sha256=%s\n' "$(sha256sum "$SILENCER_BENCH_BINARY" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      printf 'binary_sha256=%s\n' "$(shasum -a 256 "$SILENCER_BENCH_BINARY" | awk '{print $1}')"
    fi
  fi
}
