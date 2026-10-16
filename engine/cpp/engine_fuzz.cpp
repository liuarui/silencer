#include "engine.h"

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <string>

// The harness exercises one-shot masking and arbitrary restore chunk
// boundaries. Every malformed input must become an ABI status code; fuzzing
// must never observe an exception or an invalid owned buffer.
extern "C" int LLVMFuzzerTestOneInput(const uint8_t* data, size_t size) {
  if (!data || size > (1u << 20)) return 0;
  engine_t* engine = engine_create(nullptr, 0);
  if (!engine) return 0;

  char* masked_bytes = nullptr;
  size_t masked_size = 0;
  if (mask_request(engine, reinterpret_cast<const char*>(data), size,
                   &masked_bytes, &masked_size) == 0) {
    const std::string masked(masked_bytes ? masked_bytes : "", masked_size);
    buffer_free(masked_bytes);

    const uint64_t stream = restore_stream_begin(engine);
    if (stream != 0) {
      size_t offset = 0;
      while (offset < masked.size()) {
        const size_t chunk = std::min<size_t>(1 + (masked[offset] & 0x1f), masked.size() - offset);
        char* restored = nullptr;
        size_t restored_size = 0;
        if (restore_stream_push(engine, stream, masked.data() + offset, chunk,
                                &restored, &restored_size) != 0) {
          buffer_free(restored);
          restore_stream_abort(engine, stream);
          break;
        }
        buffer_free(restored);
        offset += chunk;
      }
      if (offset == masked.size()) {
        char* tail = nullptr;
        size_t tail_size = 0;
        if (restore_stream_finish(engine, stream, &tail, &tail_size) != 0) {
          buffer_free(tail);
          restore_stream_abort(engine, stream);
        } else {
          buffer_free(tail);
        }
      }
    }
  } else {
    buffer_free(masked_bytes);
  }
  engine_destroy(engine);
  return 0;
}
