# Blue-green deployment

`scripts/silencer-blue-green.sh` starts two isolated Compose projects, waits for
`/readyz` with strict upstream/storage checks, optionally runs a real data-plane
smoke request, and atomically writes the Nginx upstream include before reload.
The previous color is retained for one-command rollback.

Production blue-green requires a shared MySQL database so both instances see the
same control and request state. SQLite is rejected by default because two
containers must not write the same file or maintain divergent ledgers. SQLite
can be explicitly enabled for isolated staging with
`SILENCER_BLUE_GREEN_ALLOW_SQLITE=true`.

Example:

```bash
export SILENCER_STORAGE_MODE=mysql
export SILENCER_MYSQL_DSN='silencer:password@tcp(db:3306)/silencer?parseTime=true'
export SILENCER_UPSTREAM_URL='https://api.example.test'
export SILENCER_UPSTREAM_FILE=/etc/silencer/silencer-upstream.conf
export SILENCER_NGINX_TEST_CMD='nginx -t'
export SILENCER_NGINX_RELOAD_CMD='nginx -s reload'

scripts/silencer-blue-green.sh up blue
scripts/silencer-blue-green.sh cutover blue
scripts/silencer-blue-green.sh up green
scripts/silencer-blue-green.sh cutover green
scripts/silencer-blue-green.sh rollback
```

`SILENCER_MYSQL_DSN` must be explicit for blue-green runs. The candidate
Compose projects intentionally use `--no-deps`, so a per-project `mysql` service
is not a shared database and is rejected by the script. The hostname in the DSN
must be reachable from both candidate containers through their Docker network.

The public Nginx configuration must include
`deploy/nginx/silencer-upstream.conf.example` from its `http{}` block and use
`deploy/nginx/silencer-location.conf.example` (or an equivalent location).
The script does not expose candidate ports as the public listener and does not
reload Nginx unless `SILENCER_NGINX_RELOAD_CMD` is explicitly set.
Cutover and rollback operations are serialized with a lock at
`<SILENCER_ACTIVE_FILE>.lock`. When a reload command is configured, the script
runs `SILENCER_NGINX_TEST_CMD` (default `nginx -t`) before reloading and restores
the previous state if either check fails.

## Observability

The optional `observability` Compose profile starts Prometheus and an OTLP/HTTP
collector with the checked-in scrape, alert, and collector configurations:

```bash
docker compose --profile observability up -d
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
docker compose --profile observability up -d --force-recreate silencer
```

Prometheus is bound to `127.0.0.1:9090` and the collector to
`127.0.0.1:4318` by default. Alerts cover gateway availability, proxy errors,
5xx rate, storage write failures, and queue saturation. Keep `/metrics` behind
the private Docker network or an authenticated reverse proxy when exposing the
public listener.

## Multi-architecture images

Release images support `linux/amd64` and `linux/arm64`. The version tag and
`latest` share a manifest so Docker selects the matching image automatically.
The Go gateway, SQLite driver and C++ engine are compiled for each target;
setting `GOARCH` alone is insufficient for this CGO image.

Build from a reviewed, clean public checkout with a Buildx builder that supports
both platforms, through native nodes or emulation. Authenticate with Docker Hub
using `docker login` before publishing. Run these commands in Bash to build and
verify a candidate before promoting it:

```bash
set -euo pipefail
release_image=liuarui/silencer
release_version=1.1.0
release_revision="$(git rev-parse HEAD)"
release_candidate="$release_image:build-$release_version-${release_revision:0:12}"
release_metadata="$(mktemp)"
docker buildx build --platform linux/amd64,linux/arm64 \
  --label org.opencontainers.image.source=https://github.com/liuarui/silencer \
  --label org.opencontainers.image.revision="$release_revision" \
  --label org.opencontainers.image.version="$release_version" \
  --label org.opencontainers.image.licenses=AGPL-3.0-only \
  --tag "$release_candidate" --metadata-file "$release_metadata" --push .
release_digest="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "$release_metadata")"
release_ref="$release_image@$release_digest"
rm -f "$release_metadata"
docker buildx imagetools inspect "$release_ref"
bash scripts/test-image.sh "$release_ref" linux/amd64
bash scripts/test-image.sh "$release_ref" linux/arm64
docker buildx imagetools create \
  --tag "$release_image:$release_version" --tag "$release_image:latest" \
  "$release_ref"
docker buildx imagetools inspect "$release_image:$release_version"
docker buildx imagetools inspect "$release_image:latest"
```

Run the smoke commands in Docker environments that can execute the selected
platform. With separate native machines, run each matching command on its host
before promoting the same candidate digest. The smoke test uses disposable
containers and volumes to check readiness, native masking, disabled extension
endpoints, and configuration/password persistence after recreation.

Verify both final tags expose AMD64 and ARM64 and resolve to the same index
digest. OCI attestation descriptors may also appear as `unknown/unknown`; they
are build metadata rather than executable platforms. CI runs the full core,
MySQL and image checks on native AMD64 and ARM64 runners.
