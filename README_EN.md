# Silencer 1.0.0

[中文](README.md) · [Website](https://silencer.5970000.xyz/) · [Live demo](https://silencer.5970000.xyz/demo/) · [Docker Hub](https://hub.docker.com/r/liuarui/silencer) · [Deployment](docs/TUTORIAL.md) · [Architecture](docs/ARCHITECTURE.md) · [Benchmarks](docs/PERFORMANCE.md)

Silencer is a privacy gateway for AI clients. It replaces sensitive request text with placeholders before forwarding and restores ordinary replies and tool-call arguments on the way back. It runs a Go gateway, a C++ masking engine and a React console.

## Features

- JSON, SSE and NDJSON restoration, with separate logical streaming channels.
- Built-in rules, custom words and regex rules with console switches and hot updates.
- Durable mappings keep placeholders stable across conversation turns, retained history, restarts and log clearing.
- Named upstream routes, extra provider headers and shared connection pools.
- Request ledger, anomaly filters, exact 404 noise allowlists and masked-by-default original-data views.
- Dangerous tool-call detection with log-only, confirmation rewrite and block modes; log-only is the default.
- SQLite or MySQL storage, configurable log retention, Prometheus and optional OpenTelemetry.

**The browser extension is deferred.** Plugin source, extension-only pages and installation guides are excluded from this repository. Release images disable extension APIs and downloads.

See [website/README.md](website/README.md) for the static website source and deployment of a separate demo instance. The public demo uses a shared account; use synthetic data only. Its Nginx configuration disables password changes.

## Quick start

Published images support `linux/amd64` and `linux/arm64`. Both `1.0.0` and `latest` automatically select the host architecture:

```bash
docker pull liuarui/silencer:1.0.0
export SILENCER_GATEWAY_TOKEN="$(openssl rand -hex 24)"
docker run -d --name silencer --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v silencer_data:/data \
  -e SILENCER_GATEWAY_TOKEN \
  -e SILENCER_CONTROL_CONFIG_FILE=/data/control.json \
  liuarui/silencer:1.0.0
curl http://127.0.0.1:8080/readyz
```

Open <http://127.0.0.1:8080/>. The default administrator is `admin`; its password is the configured `SILENCER_GATEWAY_TOKEN`. The volume keeps control settings, runtime settings, changed administrator passwords, SQLite data and log files. Use a trusted HTTPS reverse proxy for remote access.

Build and run from source:

```bash
docker compose up -d --build
```

For MySQL:

```bash
cp .env.mysql.example .env
# Set database passwords and a management token before public deployment.
docker compose -f docker-compose.mysql.yml up -d --build
```

## Connect a client

Add a named upstream in the console and use this client API base URL:

```text
http://127.0.0.1:8080/r/<upstream-name>/v1
```

Clients keep their provider API keys. The management password protects the console and control APIs; management headers and session cookies are stripped before proxying.

Alternatively, set `SILENCER_UPSTREAM_URL` and forward through `/api/requests/<path>`, such as `/api/requests/v1/chat/completions`.

The Gateway probe checks local availability. Only models/chat probes contact the provider, and only HTTP 2xx counts as success.

## Data and behavior

Detailed logs retain raw, masked, upstream and restored bodies for three days by default. Disable or adjust retention in Settings. Logs contain original text; the console display mask does not remove stored data. Restoration mappings are stored independently and survive log expiry and clearing. Protect and back them up with the database.

Tool protection inspects tool arguments, not ordinary replies or reasoning. Rewrite mode returns a confirmation message; it does not approve or resume execution in the client. Rule-based detection does not replace execution permissions.

Unavailable engines, masking errors and restoration failures return errors. Binary responses do not undergo text restoration. See [architecture](docs/ARCHITECTURE.md) for protocol and resource limits.

## Development and verification

Use Go 1.22+, Node.js 22.12+, a C++17 compiler, PCRE2 development headers and pkg-config.

```bash
# Debian/Ubuntu
sudo apt-get install g++ libpcre2-dev pkg-config
npm --prefix frontend ci
bash scripts/verify-all.sh
```

The public gate covers Go, strict C++, native engine integration, frontend, log/privacy tests, migrations and deployment scripts. MySQL tests need a disposable database and `SILENCER_TEST_MYSQL_DSN`; they report a skip without one. To verify the image:

```bash
docker build -t silencer:test .
bash scripts/test-image.sh silencer:test
```

See [contributing](CONTRIBUTING.md), [release notes](docs/releases/1.0.0.md) and [third-party notices](THIRD_PARTY_NOTICES.md). Licensed under [AGPL-3.0-only](LICENSE).

## Sponsors

Sponsored by [ForZTN](https://sponsorship.forztn.com/github.com/liuarui/silencer).
