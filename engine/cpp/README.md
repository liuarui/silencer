# Silencer C++ engine

Small, exception-safe C ABI used by the Go gateway. Build with:

Install PCRE2 development headers first (`libpcre2-dev` on Debian or `pcre2`
with Homebrew). Matching uses the PCRE2 interpreter with heap, depth and work
limits; it does not recurse through the native stack or use JIT. A limit failure
returns an engine error and never passes the source through as a no-match.
Pattern acceptance still validates the existing ECMAScript dialect.

```sh
cmake -S engine/cpp -B /tmp/silencer-engine-build
cmake --build /tmp/silencer-engine-build
ctest --test-dir /tmp/silencer-engine-build --output-on-failure
```

With Clang and libFuzzer installed, build the native fuzz harness with:

```sh
cmake -S engine/cpp -B /tmp/silencer-engine-fuzz \
  -DSILENCER_ENGINE_BUILD_FUZZ=ON -DCMAKE_CXX_COMPILER=clang++
cmake --build /tmp/silencer-engine-fuzz --target engine_fuzz
/tmp/silencer-engine-fuzz/engine_fuzz -max_len=1048576 corpus/
```

The harness covers arbitrary byte input, one-shot masking, and restore chunks;
the deterministic property cases remain in `engine_test.cpp`.

`engine_create` accepts an optional JSON object with a `rules` array. Each rule is
`{"needle":"literal","label":"TOKEN_LABEL"}` for literal replacement or
`{"pattern":"...","label":"TOKEN_LABEL"}` for ECMAScript regex replacement,
using a stable `{{LABEL_<16-hex-digits>}}` token. Empty config enables conservative email
and bearer-token rules. The built-in regexes use safe literal prefilters to skip
regex evaluation when an email marker or bearer prefix cannot occur.

Regex rules may set `capture_group` to replace only a captured value while retaining
the surrounding syntax (used for passwords in connection strings, assignment-style
credentials, and bearer values). Built-in semantic rules additionally set `semantic`
and validate card Luhn checks, identity-card checksums/dates, phone and email
boundaries, landlines, IBAN mod-97, JWT headers, IPv4/IPv6 ranges, Chinese plates,
and unified social-credit checksums before issuing a token; an invalid candidate
remains unchanged. Custom sensitive words prefixed with `re:` are compiled as
regular expressions by the Go control plane; malformed entries are skipped without
disabling the remaining rules.

The configuration may also include `field_paths`, an array of dotted paths (or
JSON-pointer paths beginning with `/`) to redact in structured JSON bodies.
`json_redact` parses and serializes a JSON document through the same C ABI and
then applies the ordinary literal/regex rules to the serialized document. This
keeps path redaction additive: configuring JSON paths cannot leave a secret in
an unrelated JSON field. If `rules` is omitted, the built-in email and bearer
rules remain enabled. One-shot calls derive tokens from values. Proxy request
streams use `mask_stream_begin_reusable`: random entity identifiers are reused
for the same value and label inside a route/credential scope. Different values
receive different identifiers. The matching response owns a private map and
can also resolve retained historical entities from that scope.
Paths may
select object keys, numeric array indexes, or `*` array entries; JSON-pointer
segments support the usual `~0` and `~1` escapes.

The proxy uses the reusable stream ABI; the extension keeps the structural,
request-scoped ABI. The
masking stream and its matching restore streams share one private map. Go
keeps the completed masking handle until the response is consumed so it can
create independent restore streams for logical SSE/NDJSON channels. The adapter
then releases that handle. Production binds `mask_stream_bind_mapping_resolver`
before the first push. The callback commits identities to SQL before issuance
and resolves response-only historical references on demand; it outlives the
mask owner when restore channels remain. Callback JSON buffers use malloc/free,
and the shared request map invokes the release callback exactly once. The
bounded 24-hour registry is retained for callers without this durable binding.
Structural maps remain private to their original request.
Input placeholders reserve their suffixes before new slots are assigned, so
historical tokens cannot silently become a different current-request value.

Native restoration handles token fragments across pushes. Go owns protocol
framing and separates concurrent choices, reasoning blocks and tool calls;
restoring raw SSE wire bytes cannot join fragments inside different JSON events.
`SILENCER_RESTORE_OPTION_JSON_ESCAPE` escapes restored values for the inner JSON
strings in tool arguments. Go then serializes the outer event string and rejects
unresolved tool placeholders. A restore stream is finished before its protocol
completion marker, or aborted on cancellation.

`mask_request_with_map` returns the masked payload plus an opaque `offset_map_t`.
Use `offset_map_lookup` for byte offsets in the masked payload and release it
with `offset_map_free`; the map records the source offset at each output byte
and maps the output end to the input length. This mapping is generated during
the masking pass, so literal text that happens to look like an older token is
kept as literal input. JSON field-path rewrites retain parser source spans while
serializing the tree, so this offset-map API also works for structured JSON
requests. Rewritten field values map to the source value span and the terminal
output offset maps to the input document length.

All calls return status `0` on success and `-1` on failure; no C++ exception
crosses the ABI. `last_error()` is thread-local. Output buffers from mask/restore
functions must be released by `buffer_free`. Restore streams are created with
`restore_stream_begin`, fed arbitrary chunks, then closed by `restore_stream_finish`.
Request masking streams use the parallel `mask_stream_begin`,
`mask_stream_push`, and `mask_stream_finish` calls. They retain an unsafe suffix
between pushes; JSON field paths and custom regex rules are buffered until
finish because their complete structure is required for fail-closed matching.
Call `mask_stream_abort` when the request is cancelled or the upstream rejects
the body.

The ABI reports `engine_abi_version()` and `engine_capabilities()` before a
handle is created. The current version is `4`; required capability bits are
`SILENCER_ENGINE_CAP_MASK`, `SILENCER_ENGINE_CAP_RESTORE_STREAM`,
`SILENCER_ENGINE_CAP_MASK_STREAM`, `SILENCER_ENGINE_CAP_REQUEST_SCOPED`,
`SILENCER_ENGINE_CAP_RESTORE_OPTIONS`, and `SILENCER_ENGINE_CAP_MASK_ITEMS`.
The Go adapter rejects a library with an
unknown version or missing capability instead of starting without a complete
data-plane engine.

The gateway links this shared library with cgo. Bind the header with cgo and use `C.CBytes` for inputs; copy each output
into a Go string before calling `C.buffer_free` (the returned pointer is owned by
the engine). A minimal call looks like:

```go
in := C.CBytes([]byte(text))
defer C.free(in)
var out *C.char
var n C.size_t
if C.mask_request(e, (*C.char)(in), C.size_t(len(text)), &out, &n) != 0 {
    return errors.New(C.GoString(C.last_error()))
}
masked := C.GoBytes(unsafe.Pointer(out), C.int(n))
C.buffer_free(unsafe.Pointer(out))
```
