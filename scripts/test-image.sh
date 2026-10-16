#!/usr/bin/env bash
set -euo pipefail

image_ref="${1:-silencer:test}"
smoke_name="silencer-smoke-$(date +%s)-$$"
data_volume="$smoke_name-data"
smoke_token=synthetic-image-test-token
changed_token=synthetic-changed-image-test-token
cleanup() {
  docker rm -f "$smoke_name" >/dev/null 2>&1 || true
  docker volume rm "$data_volume" >/dev/null 2>&1 || true
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
  -H 'Content-Type: application/json' -d '{"session_ttl":777,"max_concurrency":7,"max_response_bytes":1048576}' \
  "$smoke_address/api/config" >/dev/null
docker exec "$smoke_name" test -s /data/control.json
docker exec "$smoke_name" test -s /data/control.json.runtime.json
curl -fsS -H "X-Silencer-Token: $smoke_token" \
  -H 'Content-Type: application/json' \
  -d "{\"current_password\":\"$smoke_token\",\"new_password\":\"$changed_token\"}" \
  "$smoke_address/api/auth/change-password" >/dev/null
for endpoint in /api/ext/ping /downloads/extension.zip; do
  status="$(curl -sS -o /dev/null -w '%{http_code}' "$smoke_address$endpoint")"
  [[ "$status" == 404 ]] || { echo 'image smoke: extension endpoint available' >&2; exit 1; }
done
docker rm -f "$smoke_name" >/dev/null
start_gateway
curl -fsS -H "X-Silencer-Token: $changed_token" "$smoke_address/api/config" \
  | python3 -c 'import json,sys; c=json.load(sys.stdin); assert c["session_ttl"] == 777 and c["max_concurrency"] == 7 and c["max_response_bytes"] == 1048576'
status="$(curl -sS -o /dev/null -w '%{http_code}' -H "X-Silencer-Token: $smoke_token" "$smoke_address/api/config")"
[[ "$status" == 401 ]] || { echo 'image smoke: old password still accepted' >&2; exit 1; }
echo 'image smoke: readiness, extension shutdown, settings and password persistence passed'
