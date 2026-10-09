# Silencer

Repository: <https://github.com/liuarui/silencer> · Public demo: <https://silencer.5970000.xyz/demo/>

Demo credentials: `demo` / `SilencerDemo-2026!`. The demo is isolated from the private service and only enables the console tour and local masking playground; it has no real upstream credentials.

Silencer is a self-hosted privacy-masking service for AI clients. It runs on a server or another host you control. Base URL clients send requests to its proxy endpoint; the Silencer instance masks them on its host before forwarding upstream, then restores responses as they stream back. The Web console manages the instance. Telemetry is off by default, and the operator controls configuration and data.

CLI and desktop clients connect through generic Base URL/environment-variable settings. Client onboarding can inspect supported configuration files visible to the Silencer process, then preview, back up, apply, and roll back changes. A Docker container can only access files mounted into it. The browser extension is a separate integration path that connects only to a Silencer service on the browser's own host, so the browser and its bridge instance must run on the same machine.

## Why Silencer

- **One clear path:** a client calls Silencer first, Silencer masks the request, forwards it to the selected upstream (including sub2api), and restores the response as it streams back.
- **Multiple upstream routes:** one instance can keep several upstreams and rule sets, so clients switch routes instead of duplicating proxy and credential settings.
- **Stable conversation mappings:** each sensitive value gets a stable placeholder within a session; different IPv4 addresses, email addresses, and credentials receive different identifiers so their contexts do not collide.
- **Auditable with less exposure:** the request ledger separates the client body, masked upstream body, upstream response, and restored response. Credentials are represented by type, mask, length, and digest.
- **Replaceable persistence:** use SQLite for a single instance or MySQL for long-running deployments. Background writers and a bounded pool keep database commits off the forwarding path.
- **Reversible onboarding:** the client configurator creates a plan before writing, takes a backup, writes atomically, detects conflicts, and supports rollback. With Docker, use the host-side script or apply the plan manually for host clients.
- **Operator-controlled:** telemetry is off by default; the operator controls configuration, sessions, rules, and backups.

### Upstreams and clients

A client is the source of a request, such as Claude Code, Codex, Hermes, or Cursor. An upstream is the destination Silencer forwards to, such as OpenAI, Anthropic, sub2api, or an internal compatible service. Base URL clients use this path:

```text
Claude / Codex / Hermes / Cursor
              │ Base URL
              ▼
        Silencer :5802
              │ masked request
              ▼
       sub2api / another upstream
```

Silencer does not manage client API keys and does not require a custom adapter for every client. Any client that supports `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, or an equivalent Base URL can use the same route. The browser extension uses a separate local bridge: it only connects to `127.0.0.1` / `localhost:5801` on the browser's host, so the browser and bridge service must run on the same machine.

## Performance and boundaries

The forwarding path is stream-oriented. Event and request-ledger writes are queued, rule scans reuse prefilter results, and masking can use a lightweight replacement path when offsets are not needed. These are local synthetic measurements, not a production SLA:

- 100,000 events with 80% known-failed RESTORE rows: p50 fell from **345.51 ms** to **91.44 ms** (about **73.5%**).
- 500,000 events with the same distribution: p50 fell from **1,923.45 ms** to **441.13 ms** (about **77.1%**).
- For a 2 MiB body with 91,180 repeated email hits, text masking p50 improved by about **20.2%** and JSON masking by about **15.7%**.

The benchmark covers local rules, parsing, and aggregation only. It excludes network latency, model time, TLS, database commits, and NER inference. Re-run the scripts in [the performance guide](docs/PERFORMANCE.md) on the target machine.

## Quick start

### Docker and Web console

```bash
docker build -t silencer:latest .

docker run -d --name silencer --restart unless-stopped \
  -p 127.0.0.1:5801:5801 -p 127.0.0.1:5802:5802 \
  -e SILENCER_REQUIRE_HTTPS=0 \
  -v silencer_data:/data \
  silencer:latest
```

After the first start, run `docker logs -f silencer` to read the one-time setup code.

This `docker run` example binds both ports to the deployment host's loopback address. The application and Compose default `SILENCER_REQUIRE_HTTPS` to `0`, for testing from that host. Do not use these HTTP settings for network access. For remote deployments, set `SILENCER_BIND_HOST=0.0.0.0` only when needed, put the console (`5801`) behind an HTTPS reverse proxy, and separately protect the client proxy (`5802`) with TLS or a trusted private network. `SILENCER_REQUIRE_HTTPS=1` protects the management console; it does not enable TLS on `5802`. Open `http://127.0.0.1:5801/` from the deployment host, use the one-time setup code printed in the container log, and create an administrator account. Keep the same `/data` volume when recreating the container. The default Compose setup has no dependency on another project's Docker network. To reach a sub2api container by its container name, add `docker-compose.sub2api.yml` if it is attached to `sub2api_sub2api-network`.

Alternatively, build and start with `docker compose up -d --build`. Check status with `docker compose ps`, read the setup code with `docker compose logs -f silencer`, and verify `http://127.0.0.1:5801/readyz` returns `{"ok":true}` when the service and request store are ready. Repeat the build command after source changes; the `silencer_data` named volume preserves configuration and records. `docker compose down` stops the service without removing its data volume.

To run multiple instances on one host during a staged rollout, use a unique Compose project name and host ports for each instance, for example `docker compose -p silencer-next -f docker-compose.yml up -d --build`. Compose then isolates the containers and named volumes.

### Run from source

From a source checkout, build the console and install the Python service:

```bash
npm --prefix frontend ci
npm --prefix frontend run build
python -m pip install -e './backend[ner]'
python -m silencer
```

The first-run setup code is printed by the service. Data is stored in the current user's data directory by default; set `SILENCER_DATA_DIR` to choose another location. To build a distributable wheel that includes the web console, run `python scripts/build-wheel.py`, install the generated `silencer-0.0.2-*.whl` from `dist/packages/` on the target host, and run `silencer`.

### Storage backends

SQLite is the default. `SILENCER_STORAGE_MODE=file` stores a SQLite database
file in the instance data directory; `SILENCER_STORAGE_MODE=sqlite` uses the explicit SQLite
mode and supports a custom `SILENCER_SQLITE_PATH`. For long-running deployments, use MySQL. Instances sharing a database also share the same event, audit, and request-ledger tables; use separate databases when their records need isolation:

```bash
SILENCER_STORAGE_MODE=mysql
SILENCER_MYSQL_DSN='mysql://silencer:password@mysql:3306/silencer'
SILENCER_MYSQL_POOL_SIZE=8
```

URL-encode `@`, `:` or `/` characters in a DSN password. The Compose example
uses separate host, user, password, and database variables, so its password
can contain those characters. Copy `.env.mysql.example` to `.env` and replace
both placeholder passwords before starting it.

The MySQL adapter reuses a bounded connection pool; event, audit, and request-ledger
writes are queued in the background. The proxy path does not wait for a database
transaction on every request. Instances connected to the same database write to
the same tables, with no instance or tenant isolation. Use a separate database for
each instance when records must remain isolated. Each instance keeps its own
upstream, rules, account, and backups in `/data`. `docker-compose.mysql.yml` contains a complete MySQL example.
Both file-backed modes use the SQLite format; `file` defaults to
`silencer-data.sqlite3`, while `sqlite` defaults to `silencer-events.sqlite3`.

## Connect clients

First add an upstream under **Upstreams and clients** in Settings, then copy its Base URL from **Client onboarding**. The page can discover supported configuration files visible to the current Silencer process, preview a plan, create a backup, write atomically, and roll back changes. It refuses to overwrite a file that changed after the plan was created. Discovery support is limited to the listed client formats; generic Base URL support is broader.

OpenAI-compatible clients can use the following route. The HTTP example is for localhost or a trusted private network; for internet access, replace it with your TLS-protected proxy URL:

```text
http://<silencer-server>:5802/r/<route-id>/v1
```

Anthropic-compatible clients use the route root, under the same transport requirements:

```text
http://<silencer-server>:5802/r/<route-id>
```

Use `127.0.0.1` when the client runs on the same host as Silencer. For clients on another server, use an HTTPS-protected proxy address or a trusted private network. The API key remains configured in the client and is forwarded to the selected upstream; request records redact credential values. HTTPS enforcement on the management console does not add TLS to port `5802`.

## Logs, rules, and security

The request ledger can retain bounded request and response bodies at four stages: client request, masked upstream request, upstream response, and restored response. Lists omit bodies until a request is opened. Credential headers are redacted, and entries exceeding the configured body limit are marked as truncated. Set retention to match your needs and restrict console access.

The Sensitive Words page supports presets, custom terms, regular expressions, exceptions, and local draft tests. Draft tests never call an upstream and do not modify a live conversation mapping.

The browser extension can protect supported Web AI sites. It connects only to the Silencer bridge at `127.0.0.1` / `localhost:5801`; run Silencer on the same host as the browser, enable the bridge in Settings, and copy its token into the extension. The extension does not connect to a remote Silencer server. For network-connected CLI clients, use HTTPS or a trusted private network. Confirm that a site supports attachment masking before uploading files. If the masking proxy is unavailable, the default fail-closed mode returns 503 instead of bypassing masking; plaintext passthrough is an explicit advanced setting.

## Host boundary

Docker discovery only sees files inside the container. Silencer does not mount the host HOME or use the Docker socket. To configure clients installed on the Docker host, run `scripts/silencer-client.py` on that host; point it at `127.0.0.1` if Silencer is also on that host, or at the Silencer server's reachable address otherwise. The generated plan can also be copied to the host and applied manually.

## Doctor and troubleshooting

Run `docker compose ps` and `docker compose logs -f silencer` to inspect a Docker deployment. The host-side diagnostic script is available from the source directory as `./scripts/silencer-doctor.sh --json`. Check port reachability and `/data` permissions when troubleshooting.

## Development and release checks

Run the repository gate after changes. It covers Python, frontend, extension,
version, and packaging checks:

```bash
python scripts/verify-all.py
git diff --check
```

The current release is `0.0.2`. Docker builds the frontend in a multi-stage
image, so `frontend/dist` does not need to be committed. Before publishing,
make sure `/data`, database credentials, and runtime logs contain no real
secrets.

## License

Silencer's original code is licensed under the [GNU Affero General Public License v3.0 only](LICENSE) (AGPL-3.0-only). Third-party notices are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and `licenses/`. Silencer cannot guarantee that every sensitive value is detected; you remain responsible for the selected upstream, transmitted content, credentials, and compliance requirements.
