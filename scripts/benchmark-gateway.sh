#!/usr/bin/env bash
set -euo pipefail

# Run a repeatable short-request profile against an already configured gateway.
# The upstream is deliberately external to this script so the same harness can
# be used with a local mock, a staging service, or a production-like endpoint.
has_cmd() { command -v "$1" >/dev/null 2>&1; }
die() { printf 'benchmark-gateway: %s\n' "$*" >&2; exit 1; }

has_cmd ab || die "ApacheBench (ab) is required"
has_cmd curl || die "curl is required"
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/benchmark-common.sh"

base_url="${SILENCER_BENCH_URL:-http://127.0.0.1:8080}"
path="${SILENCER_BENCH_PATH:-/api/requests/v1/chat}"
requests="${SILENCER_BENCH_REQUESTS:-10000}"
concurrency="${SILENCER_BENCH_CONCURRENCY:-32}"
body_file="${SILENCER_BENCH_BODY:-}"
output_file="${SILENCER_BENCH_OUTPUT:-}"
token="${SILENCER_BENCH_TOKEN:-}"
pid="${SILENCER_BENCH_PID:-}"

[[ "$requests" =~ ^[1-9][0-9]*$ ]] || die "SILENCER_BENCH_REQUESTS must be a positive integer"
[[ "$concurrency" =~ ^[1-9][0-9]*$ ]] || die "SILENCER_BENCH_CONCURRENCY must be a positive integer"
[[ "$path" == /* ]] || die "SILENCER_BENCH_PATH must start with /"
if [[ -n "$pid" ]]; then
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || die "SILENCER_BENCH_PID must be a positive integer"
  has_cmd ps || die "ps is required when SILENCER_BENCH_PID is set"
  kill -0 "$pid" 2>/dev/null || die "benchmark process is not running: $pid"
fi

process_snapshot() {
  local line cpu rss cpu_seconds
  line="$(ps -p "$pid" -o cputime= -o rss= 2>/dev/null | awk 'NF {print $1, $2; exit}')"
  [[ -n "$line" ]] || return 1
  read -r cpu rss <<<"$line"
  cpu_seconds="$(awk -F: '{if (NF == 2) print ($1 * 60) + $2; if (NF == 3) print ($1 * 3600) + ($2 * 60) + $3}' <<<"$cpu")"
  [[ -n "$cpu_seconds" && -n "$rss" ]] || return 1
  printf '%s %s\n' "$cpu_seconds" "$rss"
}

temporary_body=false
if [[ -z "$body_file" ]]; then
  body_file="$(mktemp "${TMPDIR:-/tmp}/silencer-bench-body.XXXXXX")"
  temporary_body=true
  printf '%s\n' '{"message":"contact alice@example.com and secret"}' >"$body_file"
fi
[[ -f "$body_file" && -r "$body_file" ]] || die "benchmark body is not readable: $body_file"
cleanup() {
  if [[ "$temporary_body" == true ]]; then
    rm -f "$body_file"
  fi
}
trap cleanup EXIT

url="${base_url%/}${path}"
curl_args=(--fail --silent --show-error --max-time 5)
if [[ -n "$token" ]]; then
  curl_args+=( -H "X-Silencer-Token: $token" )
fi
ready="$(curl "${curl_args[@]}" "${base_url%/}/readyz")" || die "gateway readiness check failed"
[[ "$ready" == *'"ok":true'* ]] || die "gateway is not ready: $ready"
[[ "$ready" == *'"upstream_configured":true'* ]] || die "gateway has no configured upstream: $ready"
metrics_before="$(curl "${curl_args[@]}" "${base_url%/}/metrics")"
process_before=""
if [[ -n "$pid" ]]; then
  process_before="$(process_snapshot)" || die "could not sample benchmark process before run"
fi

if [[ -z "$output_file" ]]; then
  output_file="${TMPDIR:-/tmp}/silencer-gateway-bench-$(date +%Y%m%d-%H%M%S).txt"
fi
mkdir -p "$(dirname "$output_file")"
metadata_file="${output_file}.meta"
metrics_file="${output_file}.metrics"
benchmark_metadata "gateway" "$url" "$body_file" >"$metadata_file"
{
  printf 'requests=%s\nconcurrency=%s\nmethod=POST\n' "$requests" "$concurrency"
  printf 'path=%s\n' "${path%%\?*}"
} >>"$metadata_file"

ab_args=(-n "$requests" -c "$concurrency" -p "$body_file" -T application/json)
if [[ -n "$token" ]]; then
  ab_args+=( -H "X-Silencer-Token: $token" )
fi
if ! ab "${ab_args[@]}" "$url" >"$output_file"; then
  die "ApacheBench failed; report: $output_file"
fi

complete_requests="$(awk -F': *' '/^Complete requests:/ {print $2+0}' "$output_file")"
failed_requests="$(awk -F': *' '/^Failed requests:/ {print $2+0}' "$output_file")"
non2xx_responses="$(awk -F': *' '/^Non-2xx responses:/ {print $2+0}' "$output_file")"
[[ "$complete_requests" -eq "$requests" ]] || die "ApacheBench completed $complete_requests/$requests requests; report: $output_file"
[[ "$failed_requests" -eq 0 ]] || die "ApacheBench reported $failed_requests failed requests; report: $output_file"
[[ "$non2xx_responses" -eq 0 ]] || die "ApacheBench reported $non2xx_responses non-2xx responses; report: $output_file"

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

printf '%s\n' "profile: requests=$requests concurrency=$concurrency url=${url%%\?*}"
awk '/Complete requests:|Failed requests:|Requests per second:| 50%| 95%| 99%| 100%/ {print}' "$output_file"
printf '%s\n' "report: $output_file"
printf '%s\n' "metadata: $metadata_file"
printf '%s\n' "metrics_before:"
printf '%s\n' "$metrics_before" | awk '/silencer_gateway_(requests_total|proxy_requests_total|proxy_errors_total|mask_requests_total|mask_errors_total|storage_errors_total|rejected_requests_total|request_duration_seconds_count|response_status_total)|silencer_storage_(queue_depth|last_write_error|write_errors_total)/ {print}'
printf '%s\n' "metrics_after:"
metrics_after="$(curl "${curl_args[@]}" "${base_url%/}/metrics")"
process_after=""
if [[ -n "$pid" ]]; then
  process_after="$(process_snapshot)" || die "could not sample benchmark process after run"
fi
printf '%s\n' "$metrics_after" | awk '/silencer_gateway_(requests_total|proxy_requests_total|proxy_errors_total|mask_requests_total|mask_errors_total|storage_errors_total|rejected_requests_total|request_duration_seconds_count|response_status_total)|silencer_storage_(queue_depth|last_write_error|write_errors_total)/ {print}'
{
  printf '%s\n' '[before]'
  printf '%s\n' "$metrics_before"
  printf '%s\n' '[after]'
  printf '%s\n' "$metrics_after"
} >"$metrics_file"
printf '%s\n' "metrics: $metrics_file"
if [[ -n "$pid" ]]; then
  cpu_before="${process_before%% *}"
  rss_before="${process_before#* }"
  cpu_after="${process_after%% *}"
  rss_after="${process_after#* }"
  cpu_delta="$(awk -v before="$cpu_before" -v after="$cpu_after" 'BEGIN {print after - before}')"
  cpu_per_request="$(awk -v cpu="$cpu_delta" -v count="$requests" 'BEGIN {if (count > 0) print cpu / count; else print 0}')"
  rss_mib="$(awk -v rss="$rss_after" 'BEGIN {print rss / 1024}')"
  printf 'process: pid=%s cpu_seconds=%s cpu_seconds_per_request=%s rss_mib=%s\n' "$pid" "$cpu_delta" "$cpu_per_request" "$rss_mib"
  printf 'process_pid=%s\nprocess_cpu_seconds=%s\nprocess_cpu_seconds_per_request=%s\nprocess_post_run_rss_mib=%s\n' \
    "$pid" "$cpu_delta" "$cpu_per_request" "$rss_mib" >>"$metadata_file"
fi
after_requests="$(metric_value "$metrics_after" silencer_gateway_requests_total)"
after_proxy="$(metric_value "$metrics_after" silencer_gateway_proxy_requests_total)"
after_mask="$(metric_value "$metrics_after" silencer_gateway_mask_requests_total)"
after_proxy_errors="$(metric_value "$metrics_after" silencer_gateway_proxy_errors_total)"
after_mask_errors="$(metric_value "$metrics_after" silencer_gateway_mask_errors_total)"
after_storage_errors="$(metric_value "$metrics_after" silencer_gateway_storage_errors_total)"
after_rejected="$(metric_value "$metrics_after" silencer_gateway_rejected_requests_total)"
# The pre-run metrics scrape itself is an HTTP request, and the post-run scrape
# is counted only after it serializes the snapshot. Account for that one
# intentional control-plane request while requiring exact data-plane deltas.
expected_request_delta=$((requests + 1))
[[ $((after_requests - before_requests)) -eq "$expected_request_delta" ]] || die "gateway request counter delta does not equal $expected_request_delta (including metrics scrape)"
[[ $((after_proxy - before_proxy)) -eq "$requests" ]] || die "proxy request counter delta does not equal $requests"
[[ $((after_mask - before_mask)) -eq "$requests" ]] || die "mask request counter delta does not equal $requests"
[[ "$after_proxy_errors" -eq "$before_proxy_errors" ]] || die "proxy errors increased during benchmark"
[[ "$after_mask_errors" -eq "$before_mask_errors" ]] || die "mask errors increased during benchmark"
[[ "$after_storage_errors" -eq "$before_storage_errors" ]] || die "storage errors increased during benchmark"
[[ "$after_rejected" -eq "$before_rejected" ]] || die "requests were rejected during benchmark"
