#!/usr/bin/env bash
set -euo pipefail

has_cmd() { command -v "$1" >/dev/null 2>&1; }
die() { printf 'benchmark-stream: %s\n' "$*" >&2; exit 1; }

has_cmd curl || die "curl is required"
has_cmd awk || die "awk is required"
has_cmd sort || die "sort is required"
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/benchmark-common.sh"

base_url="${SILENCER_STREAM_URL:-http://127.0.0.1:8080}"
path="${SILENCER_STREAM_PATH:-/api/requests/stream}"
method="${SILENCER_STREAM_METHOD:-GET}"
requests="${SILENCER_STREAM_REQUESTS:-200}"
concurrency="${SILENCER_STREAM_CONCURRENCY:-16}"
timeout="${SILENCER_STREAM_TIMEOUT:-30}"
token="${SILENCER_STREAM_TOKEN:-}"
body_file="${SILENCER_STREAM_BODY:-}"
output_file="${SILENCER_STREAM_OUTPUT:-}"

[[ "$path" == /* ]] || die "SILENCER_STREAM_PATH must start with /"
[[ "$method" =~ ^(GET|POST|PUT|PATCH)$ ]] || die "SILENCER_STREAM_METHOD must be GET, POST, PUT, or PATCH"
[[ "$requests" =~ ^[1-9][0-9]*$ ]] || die "SILENCER_STREAM_REQUESTS must be a positive integer"
[[ "$concurrency" =~ ^[1-9][0-9]*$ ]] || die "SILENCER_STREAM_CONCURRENCY must be a positive integer"
[[ "$timeout" =~ ^[1-9][0-9]*(\.[0-9]+)?$ ]] || die "SILENCER_STREAM_TIMEOUT must be positive"
if [[ -n "$body_file" ]]; then
  [[ -r "$body_file" ]] || die "SILENCER_STREAM_BODY is not readable: $body_file"
fi

url="${base_url%/}${path}"
curl_ready=(--fail --silent --show-error --max-time 5)
if [[ -n "$token" ]]; then
  curl_ready+=( -H "X-Silencer-Token: $token" )
fi
ready="$(curl "${curl_ready[@]}" "${base_url%/}/readyz")" || die "gateway readiness check failed"
[[ "$ready" == *'"ok":true'* ]] || die "gateway is not ready: $ready"
[[ "$ready" == *'"upstream_configured":true'* ]] || die "gateway has no configured upstream: $ready"
metrics_before="$(curl "${curl_ready[@]}" "${base_url%/}/metrics")"

if [[ -z "$output_file" ]]; then
  output_file="${TMPDIR:-/tmp}/silencer-stream-bench-$(date +%Y%m%d-%H%M%S).txt"
fi
mkdir -p "$(dirname "$output_file")"
metadata_file="${output_file}.meta"
metrics_file="${output_file}.metrics"
benchmark_metadata "stream" "$url" "$body_file" >"$metadata_file"
{
  printf 'requests=%s\nconcurrency=%s\nmethod=%s\ntimeout_seconds=%s\n' "$requests" "$concurrency" "$method" "$timeout"
  printf 'path=%s\n' "${path%%\?*}"
} >>"$metadata_file"

metric_value() {
  local snapshot="$1" name="$2"
  awk -v metric="$name" '$1 == metric {print $2; found=1} END {if (!found) print 0}' <<<"$snapshot"
}

before_requests="$(metric_value "$metrics_before" silencer_gateway_requests_total)"
before_proxy="$(metric_value "$metrics_before" silencer_gateway_proxy_requests_total)"
before_mask="$(metric_value "$metrics_before" silencer_gateway_mask_requests_total)"
before_proxy_errors="$(metric_value "$metrics_before" silencer_gateway_proxy_errors_total)"
before_mask_errors="$(metric_value "$metrics_before" silencer_gateway_mask_errors_total)"
before_storage_errors="$(metric_value "$metrics_before" silencer_gateway_storage_errors_total)"
before_rejected="$(metric_value "$metrics_before" silencer_gateway_rejected_requests_total)"

result_dir="$(mktemp -d "${TMPDIR:-/tmp}/silencer-stream-bench.XXXXXX")"
cleanup() { rm -rf "$result_dir"; }
trap cleanup EXIT

run_one() {
  local id="$1" output line status elapsed ttfb bytes
  output="$result_dir/body.$id"
  local args=(--no-buffer --silent --show-error --max-time "$timeout" -X "$method" -o "$output" -w '%{http_code} %{time_starttransfer} %{time_total}')
  if [[ -n "$token" ]]; then
    args+=( -H "X-Silencer-Token: $token" )
  fi
  if [[ -n "$body_file" ]]; then
    args+=( -H 'Content-Type: application/json' --data-binary "@$body_file" )
  fi
  if ! line="$(curl "${args[@]}" "$url" 2>"$result_dir/err.$id")"; then
    line="0 0 0"
  fi
  status="${line%% *}"
  line="${line#* }"
  ttfb="${line%% *}"
  elapsed="${line#* }"
  bytes="$(wc -c <"$output" | tr -d ' ')"
  printf '%s %s %s %s %s\n' "$id" "$status" "$ttfb" "$elapsed" "$bytes"
}

export -f run_one
export result_dir url method timeout token body_file
seq 1 "$requests" | xargs -P "$concurrency" -n 1 bash -c 'run_one "$1"' stream-worker >"$result_dir/results"
cp "$result_dir/results" "$output_file"

ok="$(awk '$2 == 200 {n++} END {print n+0}' "$result_dir/results")"
failed=$((requests - ok))
times_file="$result_dir/times"
awk '$2 == 200 {print $4}' "$result_dir/results" | sort -n >"$times_file"
ttfb_file="$result_dir/ttfb"
awk '$2 == 200 {print $3}' "$result_dir/results" | sort -n >"$ttfb_file"
count="$(wc -l <"$times_file" | tr -d ' ')"
percentile() {
  local fraction="$1"
  awk -v p="$fraction" -v n="$count" 'BEGIN { if (n == 0) exit 1; rank=int(n*p+0.999999); if (rank < 1) rank=1 } NR == rank {print; found=1} END {if (!found) exit 1}' "$times_file"
}
percentile_file() {
  local file="$1" fraction="$2" count_file rank
  count_file="$(wc -l <"$file" | tr -d ' ')"
  rank="$(awk -v p="$fraction" -v n="$count_file" 'BEGIN {r=int(n*p+0.999999); if (r < 1) r=1; print r}')"
  awk -v rank="$rank" 'NR == rank {print; found=1} END {if (!found) exit 1}' "$file"
}

printf 'profile: requests=%s concurrency=%s method=%s url=%s\n' "$requests" "$concurrency" "$method" "$url"
printf 'completed: %s\n' "$ok"
printf 'failed: %s\n' "$failed"
if [[ "$count" -gt 0 ]]; then
  printf 'ttfb_seconds: p50=%s p95=%s p99=%s\n' "$(percentile_file "$ttfb_file" 0.50)" "$(percentile_file "$ttfb_file" 0.95)" "$(percentile_file "$ttfb_file" 0.99)"
  printf 'latency_seconds: p50=%s p95=%s p99=%s\n' "$(percentile 0.50)" "$(percentile 0.95)" "$(percentile 0.99)"
  printf 'response_bytes: total=%s\n' "$(awk '{sum += $5} END {print sum+0}' "$result_dir/results")"
fi
printf 'metrics_before:\n'
printf '%s\n' "$metrics_before" | awk '/silencer_gateway_(requests_total|proxy_requests_total|proxy_errors_total|mask_requests_total|mask_errors_total|storage_errors_total|rejected_requests_total|request_duration_seconds_count|response_status_total)|silencer_storage_(queue_depth|last_write_error|write_errors_total)/ {print}'
printf 'metrics_after:\n'
metrics_after="$(curl "${curl_ready[@]}" "${base_url%/}/metrics")"
printf '%s\n' "$metrics_after" | awk '/silencer_gateway_(requests_total|proxy_requests_total|proxy_errors_total|mask_requests_total|mask_errors_total|storage_errors_total|rejected_requests_total|request_duration_seconds_count|response_status_total)|silencer_storage_(queue_depth|last_write_error|write_errors_total)/ {print}'
{
  printf '%s\n' '[before]'
  printf '%s\n' "$metrics_before"
  printf '%s\n' '[after]'
  printf '%s\n' "$metrics_after"
} >"$metrics_file"

after_requests="$(metric_value "$metrics_after" silencer_gateway_requests_total)"
after_proxy="$(metric_value "$metrics_after" silencer_gateway_proxy_requests_total)"
after_mask="$(metric_value "$metrics_after" silencer_gateway_mask_requests_total)"
after_proxy_errors="$(metric_value "$metrics_after" silencer_gateway_proxy_errors_total)"
after_mask_errors="$(metric_value "$metrics_after" silencer_gateway_mask_errors_total)"
after_storage_errors="$(metric_value "$metrics_after" silencer_gateway_storage_errors_total)"
after_rejected="$(metric_value "$metrics_after" silencer_gateway_rejected_requests_total)"

# The pre-run metrics scrape is one intentional control-plane request; the
# post-run scrape is counted only after it serializes its snapshot.
expected_request_delta=$((requests + 1))
[[ $((after_requests - before_requests)) -eq "$expected_request_delta" ]] || die "gateway request counter delta does not equal $expected_request_delta (including metrics scrape)"
[[ $((after_proxy - before_proxy)) -eq "$requests" ]] || die "proxy request counter delta does not equal $requests"
expected_mask_delta=0
if [[ -n "$body_file" ]]; then
  expected_mask_delta="$requests"
fi
[[ $((after_mask - before_mask)) -eq "$expected_mask_delta" ]] || die "mask request counter delta does not equal $expected_mask_delta"
[[ "$after_proxy_errors" -eq "$before_proxy_errors" ]] || die "proxy errors increased during stream benchmark"
[[ "$after_mask_errors" -eq "$before_mask_errors" ]] || die "mask errors increased during stream benchmark"
[[ "$after_storage_errors" -eq "$before_storage_errors" ]] || die "storage errors increased during stream benchmark"
[[ "$after_rejected" -eq "$before_rejected" ]] || die "requests were rejected during stream benchmark"

printf 'raw_samples: %s\nmetadata: %s\nmetrics: %s\n' "$output_file" "$metadata_file" "$metrics_file"
[[ "$failed" -eq 0 ]] || exit 1
