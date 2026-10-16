# Silencer architecture

Silencer is a two-layer privacy gateway. The production runtime is a single Go
process linked with the C++ masking library; the React console is served by the
same process. The release consists of the Go gateway, C++ engine and React console.

## Runtime layers

- `gateway/` owns `/api`, authentication, static files, reverse proxying,
  bounded concurrency, storage initialization, metrics, and graceful shutdown.
- `engine/cpp/` owns literal/regex rule matching, stable placeholder mapping,
  request masking, and incremental response restoration behind `engine.h`.
  The Go adapter verifies the C ABI version and required capability bits before
  accepting traffic.
  One-shot placeholder identity is deterministic for the matched value and
  normalized rule label. Proxy streams use random entity identities inside a
  route and provider-credential scope. The same value reuses its placeholder
  across requests; different values receive different identities. Restoration
  consults a request-local map and the durable SQL reverse index on demand. JSON field-path masking
  replaces only selected value spans and preserves other request bytes,
  including key order and whitespace. Actual cache hit rate still comes from
  cache usage reported by the upstream model.
- `frontend/` is a React client whose request helper rewrites every control
  request to `/api` and sends the gateway token.
- `gateway/internal/storage/` provides SQLite and MySQL adapters, a bounded
  writer queue, request/event tables, and health statistics.
- OpenTelemetry traces are exported over OTLP/HTTP only when
  `OTEL_EXPORTER_OTLP_ENDPOINT` is configured; Prometheus metrics remain
  available at `/metrics`.
- Storage exposes writer queue depth/capacity, connection-pool usage, and a
  redacted last-write-error flag through `/readyz`, `/api/health`, and
  `/metrics`; database driver error text is never placed on public diagnostics.
- Passive `error_leak` auditing is performed in Go only when enabled in the
  control configuration. It scans 4xx/5xx restored responses for a small set
  of credential shapes and stores only a type, length, and SHA-256 digest.
- Tool-call auditing runs after restoration. Its independent
  `audit.tool_guard` policy defaults to enabled with `mode=observe` (log only).
  `rewrite` and `block` withhold every tool event from the first tool-bearing
  frame until whole-response completion, inspect complete inputs and final
  snapshots, then release safe calls or replace the held suffix. Ordinary text
  before the first tool remains streamed. JSON enforcement inspects before
  HTTP headers commit; blocking returns 403. A stream already has its headers,
  so blocking returns a protocol error event instead. Rewriting returns an
  assistant confirmation without executable tool calls; it does not implement
  an approval queue or automatic resumption. Findings retain rule IDs, input
  lengths and hashes in audit metadata, with the action attached to the request.
- Optional host configuration changes run in a separate `silencer-host-agent`
  process over a 0600 Unix socket. The agent only accepts explicitly listed
  JSON files, creates a private backup before atomic apply, and refuses
  rollback when the current file hash differs from the recorded change.

## Request flow

1. A client sends a request to `/api/requests/<path>`.
2. Go acquires the concurrency slot. Proxy clients supply provider credentials;
   the upstream authenticates them. The management token protects control APIs
   independently, and management headers and session cookies are stripped before
   proxying.
3. Go resolves `/r/<name>/...` against the named upstreams in the control
   document, or uses `SILENCER_UPSTREAM_URL` for `/api/requests/...`.
   It feeds the request body through a C++ masking session before forwarding it.
   Safe text prefixes are masked incrementally;
   JSON and custom-regex bodies use bounded session buffering so a
   rule cannot be split into an unmasked prefix.
4. Go decodes SSE/NDJSON events and keeps an independent C++ restore session
   for each logical choice, content block, reasoning channel and tool call.
   Placeholders may span multiple events as well as HTTP read boundaries.
   Tool arguments are restored with inner JSON escaping before Go encodes the
   outer event; printable Unicode escapes in argument strings are normalized
   across deltas. Pending text is flushed before completion markers. Ordinary
   JSON responses are restored by string value while protocol identity fields
   remain intact. Missing response types are sniffed after decompression.
5. Go streams complete ledger bodies to private files on the persistent data
   volume and writes bounded previews plus accounting metadata to SQLite/MySQL.
   Authenticated detail views read the complete files; dashboard statistics use
   metadata and only fetch body columns for legacy rows missing counters.

One HTTP transport per gateway instance shares upstream connection pools across
requests and named routes. Each request reads its current target and configured
`extra_headers`; credentials are not cached in the transport. Invalid headers,
connection/framing headers and gateway management credentials are excluded.
Idle connections close after requests drain during shutdown.

The settings probe distinguishes gateway availability from upstream access.
`port` checks the gateway readiness state without contacting a provider;
`models` and `chat` contact the selected upstream with its configured headers.
Only HTTP 2xx is reported as success, redirects are not followed, and failures
expose a safe error code and HTTP status without returning provider bodies.

Detailed logging is enabled by default (`record_detail_data=true`) and retains
all four stages without a body-size truncation limit: raw request, masked request,
upstream response, and restored response. Files live in `request-logs` beside the
persistent control configuration (or at `SILENCER_LOG_DIR`) with 0700 directories
and 0600 files. SQLite/MySQL previews stay at most 64 KiB per stage, further
bounded by `SILENCER_MAX_RESPONSE_BYTES` (8 MiB by default). These previews do
not limit the saved body or streamed client response. A failed or missing full
capture is marked unavailable; a prefix is never displayed as a complete body.

The independent Log management settings default to `log_retention_days=3`;
0 keeps logs indefinitely. Startup and a minute interval remove expired bodies,
plaintext log word details and per-token log detail while retaining non-text accounting
counts and usage. Kernel file leases protect live requests across gateway
processes; abandoned requests and committed cleanup jobs are retried after a
restart. Legacy data already truncated before full logging cannot be recovered.

Each JSON response, SSE event or NDJSON record is limited to 32 MiB for
restoration, with at most 1024 active logical channels per response. This limit
is independent of ledger capture. Restoration statistics come from the logical
channels and continue to count independently of ledger previews. Unresolved
placeholders in tool arguments terminate the response instead of passing a
fake address or command to the client.

Completed protocol streams retain per-token input and remaining-output counts
for the same request. Detail views distinguish actual restorations, tokens not
repeated by the model, unresolved tokens and missing evidence. Templates such
as `{{IP_PUBLIC_...}}` do not count as sensitive tokens. Cancelled or failed
streams cannot establish successful restoration. Authenticated detail bodies
are returned in full during their retention window; expired or unavailable
bodies are explicitly identified.

The engine is fail-closed. If it cannot create, mask, or restore a stream, Go
returns an error and never forwards unmasked content. There is no passthrough
mode in the rewritten runtime.

## LLM context cache

The gateway keeps provider cache controls such as `prompt_cache_key` and
`cache_control` intact. Proxy placeholders identify entities, rather than their
positions in a request. The same value and label reuse a random 16-hex-digit
identifier inside the same host, named route, and provider-credential scope.
SQLite/MySQL commits each mapping before the native engine emits its placeholder;
a unique database entity key chooses one canonical identity across processes.
Different original values have different identifiers. JSON field-path masking
preserves bytes outside selected values. Unchanged history keeps its masked
prefix across restarts, cache eviction, log expiry and explicit log clearing. Changing an original value also changes
its identifier and may invalidate the provider cache from that point. Actual
cache hits remain determined by provider-reported usage.

For append-only multi-turn conversations whose earlier serialized bytes and
rule matches stay unchanged, existing masked messages keep their placeholder
identities and newly appended sensitive values receive new identities. This keeps the
forwarded history prefix byte-stable across turns. Inserting or reordering
content, changing rule labels or matches, changing JSON serialization, or
changing the provider cache key can invalidate the corresponding cache prefix.

Input placeholders are reserved, including decoded JSON strings. Retained
historical identities can be restored even after their original text leaves
the current context window. Protocol mappings live in separate SQL tables with
no TTL or log-retention dependency; they contain sensitive originals and require
the same database access controls and backups as other private data. Clearing
logs removes diagnostic bodies and details, while these mappings remain to keep
ongoing conversations usable. Database copies include mappings even when their
logs are gone. SQLite backups include them as part of the database; MySQL needs
normal database backups.

Startup imports retained six-consonant entity-v1 aliases without an age or 5,000-row
cutoff. Legacy structural slots are excluded because they may have represented
different originals. Conflicting aliases are marked ambiguous and are never
reissued or guessed. Already deleted originals cannot be recovered retroactively.
The bounded in-memory reusable registry remains only for non-durable ABI users;
production proxy requests bind the SQL authority. Empty-body requests also bind
it so provider-side historical references can be restored. A request-local shared
map pins resolved originals for every logical response channel; its resolver
handle remains alive until the last channel is released. SQL failures fail closed.

The configured request-body limit still applies (64 MiB by default), and a native
request may hold at most 100,000 distinct mappings. Credential, host or named-route
changes create a different scope by design. Configuration files use a synced undo
journal with a common disk lock; startup recovers all participants before reading
them. Successful commits sync every replacement and journal removal. Authentication
changes use the same durability path. Multi-instance runtime configuration still
requires external coordination; this is not a distributed configuration service.

The first request after enabling masking or changing a rule still warms a
cache for the masked representation. This is a different provider cache key
from a plaintext direct connection, so an already-warmed direct-client cache
cannot be reused retroactively. The gateway preserves the provider controls
and stable masked prefix from that point forward; actual cache read/write and
hit-rate figures continue to come from provider-reported usage. Placeholder
reuse and request-byte diagnostics are supporting indicators, not a substitute
for upstream usage data.

## Build and deployment

The root `Dockerfile` builds the React assets, compiles the Go binary with the
`silencer_engine` cgo tag, and packages the binary with the C++ runtime. The
container listens on `8080`; `/healthz`, `/readyz`, and `/metrics` are public
diagnostic endpoints, while control `/api/*` routes use the configured gateway token.

With `SILENCER_CONTROL_CONFIG_FILE` configured, runtime settings and changed
administrator passwords are saved in private sidecars next to the control file.
The Docker default places them under `/data`. Explicit environment settings
override persisted runtime values on restart. An explicitly configured
`SILENCER_CONFIG_FILE` must exist; implicit sidecars are optional on first start.

SQLite is the default single-instance store (`/data/silencer.db`). Set
`SILENCER_STORAGE_MODE=mysql` and `SILENCER_MYSQL_DSN` for a MySQL deployment.
For blue-green deployments, run the two candidates against shared MySQL and use
`scripts/silencer-blue-green.sh`; it enables strict upstream readiness and
atomically updates the Nginx upstream include before an optional reload.


Browser extension release is paused. Production images set
`SILENCER_EXTENSION_DISABLED=1`: extension APIs/downloads are unavailable and
configuration edits cannot re-enable the bridge. Browser extension source and
installation guides are excluded from the public release. The gateway proxy
is the supported data path for this release.
JSON masking scans decoded string values and numeric scalars; matched numeric
scalars become quoted placeholders, while untouched bytes and object keys stay
unchanged. An early upstream response seals the masking session without emitting
its pending input, retaining its mapping until response completion.
