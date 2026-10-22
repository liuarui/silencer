#!/usr/bin/env bash
set -euo pipefail

image_ref="${1:-silencer:test}"
if [[ -n "${2:-}" ]]; then
  export DOCKER_DEFAULT_PLATFORM="$2"
fi
smoke_name="silencer-smoke-$(date +%s)-$$"
data_volume="$smoke_name-data"
extension_response_file="$(mktemp)"
smoke_token=synthetic-image-test-token
changed_token=synthetic-changed-image-test-token
cleanup() {
  docker rm -f "$smoke_name" >/dev/null 2>&1 || true
  docker volume rm "$data_volume" >/dev/null 2>&1 || true
  rm -f "$extension_response_file"
}
trap cleanup EXIT
docker volume create "$data_volume" >/dev/null

start_gateway() {
  docker run -d --name "$smoke_name" \
    -p 127.0.0.1::8080 -v "$data_volume:/data" \
    -e "SILENCER_GATEWAY_TOKEN=$smoke_token" "$image_ref" >/dev/null
  smoke_address="http://$(docker port "$smoke_name" 8080/tcp)"
  for attempt in $(seq 1 60); do
    if curl -fsS "$smoke_address/readyz" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  echo 'image smoke: readiness failed' >&2
  return 1
}

start_gateway
curl -fsS -H "X-Silencer-Token: $smoke_token" \
  -H 'Content-Type: application/json' \
  -d '{"text":"Contact image-test@example.com or 13800138000"}' \
  "$smoke_address/api/demo/mask" \
  | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["ok"] and r["count"] >= 2 and "image-test@example.com" not in r["masked"] and "13800138000" not in r["masked"]'
curl -fsS -H "X-Silencer-Token: $smoke_token" \
  -H 'Content-Type: application/json' -d '{"session_ttl":777,"max_concurrency":7,"max_response_bytes":1048576}' \
  "$smoke_address/api/config" >/dev/null
docker exec "$smoke_name" test -s /data/control.json
docker exec "$smoke_name" test -s /data/control.json.runtime.json
curl -fsS -H "X-Silencer-Token: $smoke_token" \
  -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$smoke_token\",\"new_password\":\"$changed_token\"}" \
  "$smoke_address/api/auth/change-password" >/dev/null
curl -fsS "$smoke_address/api/ext/ping" \
  | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["enabled"] is False and r["ext_protocol"] == 3'
status="$(curl -sS -o "$extension_response_file" -w '%{http_code}' \
  -H 'Content-Type: application/json' -d '{"text":"image-test@example.com"}' \
  "$smoke_address/api/ext/mask")"
[[ "$status" == 403 ]] || { echo 'image smoke: extension bridge was not opt-in' >&2; exit 1; }
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); assert r.get("error") in ("management_token_required", "ext_bridge_disabled"), r' "$extension_response_file"
docker rm -f "$smoke_name" >/dev/null
start_gateway
curl -fsS -H "X-Silencer-Token: $changed_token" "$smoke_address/api/config" \
  | python3 -c 'import json,sys; c=json.load(sys.stdin); assert c["session_ttl"] == 777 and c["max_concurrency"] == 7 and c["max_response_bytes"] == 1048576'
status="$(curl -sS -o /dev/null -w '%{http_code}' -H "X-Silencer-Token: $smoke_token" "$smoke_address/api/config")"
[[ "$status" == 401 ]] || { echo 'image smoke: old password still accepted' >&2; exit 1; }
echo 'image smoke: readiness, native masking, opt-in extension bridge, settings and password persistence passed'
