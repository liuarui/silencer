#ifndef SILENCER_ENGINE_H
#define SILENCER_ENGINE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct engine engine_t;
typedef struct offset_map offset_map_t;

/*
 * The ABI is C-only and opaque. Bump the version when a caller-visible
 * signature or ownership rule changes. Capability bits let adapters reject an
 * older library before creating a live engine handle.
 */
#define SILENCER_ENGINE_ABI_VERSION 4u
#define SILENCER_ENGINE_CAP_MASK (1ull << 0)
#define SILENCER_ENGINE_CAP_RESTORE_STREAM (1ull << 1)
#define SILENCER_ENGINE_CAP_MASK_STREAM (1ull << 2)
#define SILENCER_ENGINE_CAP_REQUEST_SCOPED (1ull << 3)
#define SILENCER_ENGINE_CAP_RESTORE_OPTIONS (1ull << 4)
#define SILENCER_ENGINE_CAP_MASK_ITEMS (1ull << 5)
#define SILENCER_ENGINE_CAP_REUSABLE_TOKENS (1ull << 6)
#define SILENCER_ENGINE_CAP_DURABLE_MAPPINGS (1ull << 7)

uint32_t engine_abi_version(void);
uint64_t engine_capabilities(void);

/* Create an engine. config_json may be NULL/empty for the default rules. */
engine_t* engine_create(const char* config_json, size_t config_len);
/*
 * Destroy an engine after the caller has stopped submitting new calls. Calls
 * already in progress are drained by the implementation. The Go adapter
 * serializes this lifetime boundary; native callers must not invoke an ABI
 * function with the same raw handle concurrently with engine_destroy.
 */
void engine_destroy(engine_t* engine);

/*
 * Mask input. When field_paths are configured and input is JSON, path redaction
 * and ordinary literal/regex rules are applied together. On success returns 0
 * and sets output pointer/length. Free with buffer_free.
 */
int mask_request(engine_t* engine, const char* input, size_t input_len,
                 char** out, size_t* out_len);
/* Mask while returning a byte offset map for incremental consumers. */
int mask_request_with_map(engine_t* engine, const char* input, size_t input_len,
                          char** out, size_t* out_len, offset_map_t** map);
int offset_map_lookup(const offset_map_t* map, size_t masked_offset, size_t* original_offset);
void offset_map_free(offset_map_t* map);
/* Redact configured JSON field paths in a JSON document. */
int json_redact(engine_t* engine, const char* input, size_t input_len,
                char** out, size_t* out_len);

/* Begin/push/finish a restore stream. Handles are opaque and owned by engine. */
uint64_t restore_stream_begin(engine_t* engine);
/* Begin restoration against a completed request-scoped masking stream. */
uint64_t restore_stream_begin_for_mask(engine_t* engine, uint64_t mask_stream);
int restore_stream_push(engine_t* engine, uint64_t stream,
                        const char* input, size_t input_len,
                        char** out, size_t* out_len);
/* Configure JSON-string escaping before the first push. Unknown option bits fail. */
#define SILENCER_RESTORE_OPTION_JSON_ESCAPE (1u << 0)
int restore_stream_set_options(engine_t* engine, uint64_t stream, uint32_t options);
int restore_stream_finish(engine_t* engine, uint64_t stream,
                          char** out, size_t* out_len);
/* Abort and release a stream without producing output. */
int restore_stream_abort(engine_t* engine, uint64_t stream);

/* Begin/push/finish/abort an incremental request masking stream. */
uint64_t mask_stream_begin(engine_t* engine);
/* Create an isolated structural placeholder map for one gateway request. */
uint64_t mask_stream_begin_scoped(engine_t* engine);
/* Stable entity identities and historical restoration inside one trust scope. */
uint64_t mask_stream_begin_reusable(engine_t* engine, const char* scope, size_t scope_len);
/* Bind durable authority before the first push. Resolver returns a malloc-owned
 * JSON reply; release owns the handle only after this call succeeds. Its
 * lifetime follows the shared request map, including all restore channels. */
typedef char* (*mapping_resolver_fn)(uintptr_t, const char*, size_t);
typedef void (*mapping_release_fn)(uintptr_t);
int mask_stream_bind_mapping_resolver(engine_t*, uint64_t, uintptr_t,
                                      mapping_resolver_fn, mapping_release_fn);
int engine_import_reusable_tokens(engine_t* engine, const char* scope, size_t scope_len,
                                  const char* items_json, size_t items_len);
int engine_clear_reusable_tokens(engine_t* engine);
int mask_stream_push(engine_t* engine, uint64_t stream,
                     const char* input, size_t input_len,
                     char** out, size_t* out_len);
int mask_stream_finish(engine_t* engine, uint64_t stream,
                       char** out, size_t* out_len);
/* Return the completed request's per-placeholder original-value map as JSON. */
int mask_stream_items_json(engine_t* engine, uint64_t stream,
                           char** out, size_t* out_len);
int mask_stream_abort(engine_t* engine, uint64_t stream);
/* Stop an upload without emitting its pending suffix; retain scoped mappings. */
int mask_stream_seal(engine_t* engine, uint64_t stream);
/* Release a completed request-scoped masking stream and its private mapping. */
int mask_stream_release(engine_t* engine, uint64_t stream);

/* Release an output returned by this ABI. NULL is accepted. */
void buffer_free(void* buffer);

/* Thread-local diagnostic for the most recent failed call (never throws). */
const char* last_error(void);

#ifdef __cplusplus
}
#endif
#endif
