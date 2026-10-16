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
