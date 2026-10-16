# Go + C++ deployment tutorial

The release runs one gateway process on port `8080`. The React console and
data plane share that listener; there is no separate panel or proxy process.

## Start with Compose

```bash
export SILENCER_UPSTREAM_URL=https://api.example.test
export SILENCER_GATEWAY_TOKEN=change-me
docker compose up -d --build
curl http://127.0.0.1:8080/readyz
```

The response is ready only when the Go process and configured storage are
healthy. Open `http://127.0.0.1:8080/` for the console. Keep the default
loopback binding for local use; put a trusted TLS reverse proxy in front of
the single port before exposing it remotely.

## Send traffic

Use the new data-plane routes:

```bash
curl -H 'Authorization: Bearer <provider-api-key>' \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hello"}]}' \
  http://127.0.0.1:8080/api/requests/v1/chat/completions
```

The gateway token protects the console and control APIs. Proxy requests use the
provider's credentials; the management token is not needed on the data plane.
The gateway removes `X-Silencer-Token` and the console session cookie before
forwarding, while preserving the provider's `Authorization` header.

The `/r/<route>/...` form is also available for a named route. Add a named
upstream through `/api/config/patch`; its `target` and optional `base_path`
are resolved on every request, so edits take effect without a restart:

```bash
curl -H "X-Silencer-Token: $SILENCER_GATEWAY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"key":"upstreams","op":"list_upsert","value":{"name":"demo","target":"https://api.example.test","base_path":"/v1"}}' \
  http://127.0.0.1:8080/api/config/patch
```

Then send traffic to `/r/demo/...`. Responses with
JSON, NDJSON, or SSE content types are restored across arbitrary stream chunk
boundaries. Unsupported compressed text encodings fail closed; binary and
non-text responses are streamed without text restoration.

## Storage

SQLite is the default and stores data under `/data/silencer.db` in the
container. Set `SILENCER_STORAGE_MODE=mysql` and `SILENCER_MYSQL_DSN` for a
multi-instance deployment. `SILENCER_MYSQL_POOL_SIZE` controls the maximum open
connections (default 32), while `SILENCER_MYSQL_MAX_IDLE_CONNS` controls the
idle pool (default min(8, pool size)). The gateway initializes the configured
schema at startup. **Settings → Log management** enables detailed logging by
default and keeps complete raw, masked, upstream and restored bodies for 3 days.
Set retention to 0 for permanent storage. Expiry removes bodies and plaintext
word detail while keeping request and token counters. Existing explicitly
disabled detail settings are preserved. Conversation restoration mappings remain
independently in the database after log expiry or clearing; protect and back up
them with the database. Already deleted historical originals cannot be recreated.

Complete bodies stream to private files beside the persistent control config,
or under `SILENCER_LOG_DIR`. Keep that directory on a persistent volume, including
when the accounting database is MySQL. MySQL `max_allowed_packet` must cover
the largest individual database field; legacy body imports can exceed small
server defaults (for example 4 MiB). Raise it on the destination server before
importing such rows; the gateway does not alter server-wide MySQL settings. Database previews and memory buffers stay
bounded (at most 64 KiB when full logging is enabled); `SILENCER_MAX_RESPONSE_BYTES`
controls those previews, not complete saved bodies. Missing or failed captures
are marked unavailable. Previously truncated historical bodies remain truncated.

`/readyz` and `/api/health` include storage mode, writer queue depth and
capacity, and database connection counts. `/metrics` exposes the same values
for load tests; a generic `storage_error` appears when the database is not
healthy without exposing driver or DSN details.

To enable passive error-leak auditing, patch the control configuration with
`audit.enabled=true`, `audit.passive=true`, and
`audit.signals.error_leak=true`. Matching 4xx/5xx responses create an audit
event containing only redacted evidence. Host-level client discovery and
config writes require a separate host agent; active probes are not included in
the current gateway build. The gateway never claims unavailable operations
completed inside a container.

The Safety Audit page also provides dangerous tool-call protection. Its
independent switch defaults to on with **Log only**, preserving delivered
tool calls. Choose **Replace with confirmation** to remove the response's tool
calls and return a message naming the detected operation, or **Block request**
to return a safety error (403 for JSON; an error event for streams). Configuration
is `audit.tool_guard={"enabled":true,"mode":"observe"}`; other modes are
`rewrite` and `block`. Page edits persist and take effect on subsequent responses.

The detector checks restored inputs for explicit recursive/bulk deletion,
database drops and unbounded mutations, destructive disk/Git/infrastructure
commands, and remote script execution. It excludes ordinary answer and reasoning
text. Shell syntax is parsed to distinguish quoted command names from `echo`
examples; SQL literals/comments do not count as WHERE filters. Enforcing modes
hold tool events until completion, including parallel tools and repeated
snapshots. A held suffix is limited to 32 MiB / 8192 frames, with 128 tools,
512 snapshots and 2 MiB per tool input; exceeding inspection limits fails closed
in enforcing modes and records a diagnostic in Log only mode. Detection is a
rule-based safety check, not arbitrary program interpretation or a sandbox.
Confirmations do not grant permission or resume execution: an intended operation
must be approved in the client and retried under an appropriate gateway policy.
Audit event evidence contains only rule IDs, lengths and hashes. The confirmation
sent to the client includes a command preview; if detailed logging is enabled,
that confirmation is also part of the restored-response body log and follows its
retention and console plaintext-display settings.

For a local MySQL stack, use the supplied Compose profile after setting strong
passwords in `.env` (copy `.env.mysql.example` first):

```bash
export MYSQL_PASSWORD='replace-me'
export MYSQL_ROOT_PASSWORD='replace-root-me'
export SILENCER_GATEWAY_TOKEN='change-me'
docker compose -f docker-compose.mysql.yml up -d --build
```

The MySQL Compose file refuses empty database passwords. Its DSN defaults to
the local `mysql` service and the configured `MYSQL_PASSWORD`; set
`SILENCER_MYSQL_DSN` only when connecting to an external database.

## Build the native engine

```bash
cmake -S engine -B build
cmake --build build
ctest --test-dir build --output-on-failure
```

The production gateway is built with the `silencer_engine` cgo tag and links
`libsilencer_engine`. If engine startup, masking, or streaming restoration
fails, the request is rejected rather than forwarded in plaintext.
