#include "engine.h"
#include <algorithm>
#include <cassert>
#include <cstring>
#include <iostream>
#include <random>
#include <regex>
#include <string>
#include <thread>
#include <vector>
int main() {
  // Linux's old recursive std::regex matcher crashed in mask_stream_finish
  // on this ordinary long JSON string, including when no email was present.
  const std::string long_email_config = R"JSON({"rules":[{"pattern":"([^\\s@]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,})","label":"EMAIL","semantic":true}]})JSON";
  engine_t* long_engine = engine_create(long_email_config.data(), long_email_config.size());
  assert(long_engine);
  const std::string long_body = "{\"messages\":[{\"content\":\"" + std::string(2 << 20, 'x') + " synthetic@example.test\"}]}";
  const uint64_t long_stream = mask_stream_begin_scoped(long_engine);
  assert(long_stream);
  char* long_out = nullptr; size_t long_size = 0;
  assert(mask_stream_push(long_engine, long_stream, long_body.data(), long_body.size(), &long_out, &long_size) == 0);
  buffer_free(long_out);
  assert(mask_stream_finish(long_engine, long_stream, &long_out, &long_size) == 0);
  const std::string long_masked(long_out, long_size);
  assert(long_masked.find(std::string(2 << 20, 'x')) != std::string::npos);
  assert(long_masked.find("synthetic@example.test") == std::string::npos);
  assert(long_masked.find("{{EMAIL_") != std::string::npos);
  buffer_free(long_out);
  assert(mask_stream_release(long_engine, long_stream) == 0);
  engine_destroy(long_engine);

  const std::string limited_config = R"JSON({"rules":[{"pattern":"(a|aa)+$","label":"LIMIT"}]})JSON";
  engine_t* limited_engine = engine_create(limited_config.data(), limited_config.size());
  assert(limited_engine);
  const std::string limited_body = std::string(1000, 'a') + "b";
  long_out = nullptr; long_size = 0;
  assert(mask_request(limited_engine, limited_body.data(), limited_body.size(), &long_out, &long_size) != 0);
  assert(long_out == nullptr && long_size == 0); // Limit failures must fail closed.
  assert(mask_request(limited_engine, "safe", 4, &long_out, &long_size) == 0);
  buffer_free(long_out);
  engine_destroy(limited_engine);

  assert(engine_abi_version() == SILENCER_ENGINE_ABI_VERSION);
  const uint64_t capabilities = engine_capabilities();
  assert((capabilities & SILENCER_ENGINE_CAP_MASK) != 0);
  assert((capabilities & SILENCER_ENGINE_CAP_RESTORE_STREAM) != 0);
  assert((capabilities & SILENCER_ENGINE_CAP_MASK_STREAM) != 0);
  assert((capabilities & SILENCER_ENGINE_CAP_REQUEST_SCOPED) != 0);
  assert((capabilities & SILENCER_ENGINE_CAP_MASK_ITEMS) != 0);

  const char cfg[] = R"({"rules":[{"needle":"secret","label":"SECRET"},{"needle":"alice@example.com","label":"EMAIL"}]})";
  engine_t* e = engine_create(cfg, sizeof(cfg)-1); assert(e);
  char* out = nullptr; size_t n = 0;
  const std::string in = "secret for alice@example.com";
  assert(mask_request(e, in.data(), in.size(), &out, &n) == 0); std::string masked(out, n); buffer_free(out);
  assert(masked.find("{{SECRET_") != std::string::npos); assert(masked.find("{{EMAIL_") != std::string::npos);

  // Request-scoped streaming exposes exact originals without retaining the
  // full request body in the control-plane metadata.
  const uint64_t item_mask_stream = mask_stream_begin_scoped(e); assert(item_mask_stream);
  char* stream_out = nullptr; size_t stream_len = 0;
  assert(mask_stream_push(e, item_mask_stream, in.data(), in.size(), &stream_out, &stream_len) == 0);
  buffer_free(stream_out);
  assert(mask_stream_finish(e, item_mask_stream, &stream_out, &stream_len) == 0);
  buffer_free(stream_out);
  char* items_json = nullptr; size_t items_len = 0;
  assert(mask_stream_items_json(e, item_mask_stream, &items_json, &items_len) == 0);
  const std::string items(items_json, items_len); buffer_free(items_json);
  assert(items.find("alice@example.com") != std::string::npos);
  assert(items.find("secret") != std::string::npos);
  assert(mask_stream_release(e, item_mask_stream) == 0);

  // Token identity is deterministic across engine reconstruction. A gateway
  // config reload must not turn the same sensitive value into a new prompt
  // token and invalidate an otherwise reusable upstream context prefix.
  engine_t* rebuilt = engine_create(cfg, sizeof(cfg) - 1); assert(rebuilt);
  char* rebuilt_out = nullptr; size_t rebuilt_len = 0;
  assert(mask_request(rebuilt, in.data(), in.size(), &rebuilt_out, &rebuilt_len) == 0);
  assert(std::string(rebuilt_out, rebuilt_len) == masked);
  buffer_free(rebuilt_out);

  // Token identity is stable regardless of the order in which worker-local
  // maps first encounter the same value and label.
  const char reversed_cfg[] = R"({"rules":[{"needle":"alice@example.com","label":"EMAIL"},{"needle":"secret","label":"SECRET"}]})";
  engine_t* reversed = engine_create(reversed_cfg, sizeof(reversed_cfg) - 1); assert(reversed);
  const std::string email_first = "alice@example.com then secret";
  char* reversed_out = nullptr; size_t reversed_len = 0;
  assert(mask_request(reversed, email_first.data(), email_first.size(), &reversed_out, &reversed_len) == 0);
  const std::string reversed_masked(reversed_out, reversed_len); buffer_free(reversed_out);
  const size_t email_in_masked = masked.find("{{EMAIL_");
  const size_t email_in_reversed = reversed_masked.find("{{EMAIL_");
  assert(email_in_masked != std::string::npos && email_in_reversed != std::string::npos);
  assert(masked.substr(email_in_masked, masked.find("}}", email_in_masked) + 2 - email_in_masked) ==
      reversed_masked.substr(email_in_reversed, reversed_masked.find("}}", email_in_reversed) + 2 - email_in_reversed));
  engine_destroy(reversed);

  // A value may be classified by different rules in different requests. Its
  // placeholder must depend on both value and label, not whichever classifier
  // first populated a worker's reverse map.
  const char classify_cfg[] = R"({"field_paths":["user.value"],"rules":[{"needle":"secret","label":"SECRET"}]})";
  engine_t* classified_after_text = engine_create(classify_cfg, sizeof(classify_cfg) - 1); assert(classified_after_text);
  const std::string plain_secret = "secret";
  char* classified_out = nullptr; size_t classified_len = 0;
  assert(mask_request(classified_after_text, plain_secret.data(), plain_secret.size(), &classified_out, &classified_len) == 0);
  buffer_free(classified_out);
  const std::string classified_json = R"({"user":{"value":"secret"}})";
  assert(mask_request(classified_after_text, classified_json.data(), classified_json.size(), &classified_out, &classified_len) == 0);
  const std::string after_text_json(classified_out, classified_len); buffer_free(classified_out);
  engine_destroy(classified_after_text);

  engine_t* classified_json_first = engine_create(classify_cfg, sizeof(classify_cfg) - 1); assert(classified_json_first);
  assert(mask_request(classified_json_first, classified_json.data(), classified_json.size(), &classified_out, &classified_len) == 0);
  const std::string json_first(classified_out, classified_len); buffer_free(classified_out);
  assert(after_text_json == json_first);
  engine_destroy(classified_json_first);
  engine_destroy(rebuilt);

  const std::string same_prefix = "stable system prefix: ";
  const std::string first_value = same_prefix + "secret one";
  const std::string second_value = same_prefix + "secret two";
  assert(mask_request(e, first_value.data(), first_value.size(), &out, &n) == 0);
  std::string first_masked(out, n); buffer_free(out);
  assert(mask_request(e, second_value.data(), second_value.size(), &out, &n) == 0);
  std::string second_masked(out, n); buffer_free(out);
  assert(first_masked.substr(0, same_prefix.size()) == same_prefix);
  assert(second_masked.substr(0, same_prefix.size()) == same_prefix);

  offset_map_t* offsets = nullptr;
  assert(mask_request_with_map(e, in.data(), in.size(), &out, &n, &offsets) == 0);
  std::string mapped(out, n); buffer_free(out);
  assert(mapped == masked); assert(offsets);
  size_t original_offset = 0;
  assert(offset_map_lookup(offsets, 0, &original_offset) == 0 && original_offset == 0);
  const size_t separator = mapped.find(" for ");
  assert(separator != std::string::npos);
  assert(offset_map_lookup(offsets, separator, &original_offset) == 0 && original_offset == 6);
  assert(offset_map_lookup(offsets, mapped.size(), &original_offset) == 0 && original_offset == in.size());
  assert(offset_map_lookup(offsets, mapped.size() + 1, &original_offset) != 0);
  offset_map_free(offsets);
  // Existing placeholders remain literal input on later calls and must retain
  // identity offsets instead of being looked up in the engine token table.
  assert(mask_request_with_map(e, masked.data(), masked.size(), &out, &n, &offsets) == 0);
  buffer_free(out); assert(offsets);
  const size_t email_token = masked.find("{{EMAIL_");
  assert(email_token != std::string::npos);
  assert(offset_map_lookup(offsets, email_token, &original_offset) == 0 && original_offset == email_token);
  offset_map_free(offsets);
  assert(offset_map_lookup(nullptr, 0, &original_offset) != 0);
  assert(last_error()[0] != '\0');
  uint64_t s = restore_stream_begin(e); assert(s);
  std::string a = masked.substr(0, masked.size()/2), b = masked.substr(masked.size()/2);
  assert(restore_stream_push(e,s,a.data(),a.size(),&out,&n)==0); std::string restored(out,n); buffer_free(out);
  assert(restore_stream_push(e,s,b.data(),b.size(),&out,&n)==0); restored.append(out,n); buffer_free(out);
  assert(restore_stream_finish(e,s,&out,&n)==0); restored.append(out,n); buffer_free(out);
  assert(restored == in); engine_destroy(e);

  // Restoration accepts the spellings that the old Python path handled when
  // an upstream model normalizes template syntax: case/whitespace changes,
  // escaped braces, and stripped braces. Unknown placeholders remain literal.
  engine_t* tolerant = engine_create(cfg, sizeof(cfg) - 1); assert(tolerant);
  char* tolerant_masked_bytes = nullptr; size_t tolerant_masked_len = 0;
  const std::string tolerant_input = "alice@example.com";
  assert(mask_request(tolerant, tolerant_input.data(), tolerant_input.size(), &tolerant_masked_bytes, &tolerant_masked_len) == 0);
  const std::string tolerant_masked(tolerant_masked_bytes, tolerant_masked_len); buffer_free(tolerant_masked_bytes);
  const size_t token_begin = tolerant_masked.find("{{EMAIL_");
  assert(token_begin != std::string::npos);
  const size_t token_end = tolerant_masked.find("}}", token_begin);
  assert(token_end != std::string::npos);
  const std::string token_body = tolerant_masked.substr(token_begin + 2, token_end - token_begin - 2);
  const std::string suffix = token_body.substr(token_body.find('_') + 1);
  const std::vector<std::string> tolerant_forms = {
      "{{ email_" + suffix + " }}",
      "EMAIL_" + suffix,
      "\\{\\{EMAIL_" + suffix + "\\}\\}",
  };
  for (const auto& form : tolerant_forms) {
    uint64_t restore = restore_stream_begin(tolerant); assert(restore);
    assert(restore_stream_push(tolerant, restore, form.data(), form.size(), &out, &n) == 0);
    std::string got(out, n); buffer_free(out);
    assert(restore_stream_finish(tolerant, restore, &out, &n) == 0);
    got.append(out, n); buffer_free(out);
    assert(got == tolerant_input);
  }
  uint64_t unknown_restore = restore_stream_begin(tolerant); assert(unknown_restore);
  const std::string unknown = "{{EMAIL_0000000000000000}}";
  assert(restore_stream_push(tolerant, unknown_restore, unknown.data(), unknown.size(), &out, &n) == 0);
  std::string unknown_output(out, n); buffer_free(out);
  assert(restore_stream_finish(tolerant, unknown_restore, &out, &n) == 0);
  unknown_output.append(out, n); buffer_free(out);
  assert(unknown_output == unknown);
  engine_destroy(tolerant);

  // JSON/SSE restoration must escape quotes and control characters exactly as
  // the old Python implementation did, otherwise the response JSON breaks.
  const char json_escape_cfg[] = R"({"rules":[{"needle":"a\"b","label":"TERM"}]})";
  engine_t* json_escape_engine = engine_create(json_escape_cfg, sizeof(json_escape_cfg) - 1); assert(json_escape_engine);
  const std::string json_sensitive = "a\"b";
  assert(mask_request(json_escape_engine, json_sensitive.data(), json_sensitive.size(), &out, &n) == 0);
  const std::string json_token(out, n); buffer_free(out);
  uint64_t json_restore = restore_stream_begin(json_escape_engine); assert(json_restore);
  assert(restore_stream_set_options(json_escape_engine, json_restore, SILENCER_RESTORE_OPTION_JSON_ESCAPE) == 0);
  const std::string json_response = "{\"value\":\"" + json_token + "\"}";
  assert(restore_stream_push(json_escape_engine, json_restore, json_response.data(), json_response.size(), &out, &n) == 0);
  std::string escaped_json(out, n); buffer_free(out);
  assert(restore_stream_finish(json_escape_engine, json_restore, &out, &n) == 0);
  escaped_json.append(out, n); buffer_free(out);
  assert(escaped_json == R"({"value":"a\"b"})");

  uint64_t split_json_restore = restore_stream_begin(json_escape_engine); assert(split_json_restore);
  assert(restore_stream_set_options(json_escape_engine, split_json_restore, SILENCER_RESTORE_OPTION_JSON_ESCAPE) == 0);
  const std::string json_prefix = R"({"value":)";
  assert(restore_stream_push(json_escape_engine, split_json_restore, json_prefix.data(), json_prefix.size(), &out, &n) == 0);
  std::string split_json(out, n); buffer_free(out);
  const std::string json_value = "\"" + json_token + "\"}";
  assert(restore_stream_push(json_escape_engine, split_json_restore, json_value.data(), json_value.size(), &out, &n) == 0);
  split_json.append(out, n); buffer_free(out);
  assert(restore_stream_finish(json_escape_engine, split_json_restore, &out, &n) == 0);
  split_json.append(out, n); buffer_free(out);
  assert(split_json == R"({"value":"a\"b"})");

  // event-stream is a transport type, not proof that every data field is JSON.
  // Escape only placeholders lexically inside a JSON string; raw SSE text must
  // still receive the original bytes.
  uint64_t raw_sse_restore = restore_stream_begin(json_escape_engine); assert(raw_sse_restore);
  assert(restore_stream_set_options(json_escape_engine, raw_sse_restore, SILENCER_RESTORE_OPTION_JSON_ESCAPE) == 0);
  const std::string raw_sse = "data: " + json_token + "\n\n";
  assert(restore_stream_push(json_escape_engine, raw_sse_restore, raw_sse.data(), 5, &out, &n) == 0);
  std::string raw_sse_restored(out, n); buffer_free(out);
  assert(restore_stream_push(json_escape_engine, raw_sse_restore, raw_sse.data() + 5, raw_sse.size() - 5, &out, &n) == 0);
  raw_sse_restored.append(out, n); buffer_free(out);
  assert(restore_stream_finish(json_escape_engine, raw_sse_restore, &out, &n) == 0);
  raw_sse_restored.append(out, n); buffer_free(out);
  assert(raw_sse_restored == "data: a\"b\n\n");
  engine_destroy(json_escape_engine);

  engine_t* property_engine = engine_create(cfg, sizeof(cfg) - 1); assert(property_engine);
  std::mt19937 random(0x51A3U);
  const std::string alphabet = "abcdefghijklmnopqrstuvwxyz0123456789 -_./";
  for (int sample = 0; sample < 500; ++sample) {
    std::string original;
    const size_t prefix_size = 1 + random() % 96;
    original.reserve(prefix_size + 64);
    for (size_t i = 0; i < prefix_size; ++i) original += alphabet[random() % alphabet.size()];
    original += " secret alice@example.com ";
    const size_t suffix_size = random() % 96;
    for (size_t i = 0; i < suffix_size; ++i) original += alphabet[random() % alphabet.size()];

    char* masked_bytes = nullptr; size_t masked_size = 0;
    assert(mask_request(property_engine, original.data(), original.size(), &masked_bytes, &masked_size) == 0);
    std::string masked_sample(masked_bytes, masked_size); buffer_free(masked_bytes);
    uint64_t mask_stream = mask_stream_begin(property_engine); assert(mask_stream);
    std::string streamed_masked;
    for (size_t offset = 0; offset < original.size();) {
      const size_t count = std::min<size_t>(1 + random() % 17, original.size() - offset);
      char* chunk = nullptr; size_t chunk_size = 0;
      assert(mask_stream_push(property_engine, mask_stream, original.data() + offset, count, &chunk, &chunk_size) == 0);
      streamed_masked.append(chunk, chunk_size); buffer_free(chunk);
      offset += count;
    }
    char* masked_tail = nullptr; size_t masked_tail_size = 0;
    assert(mask_stream_finish(property_engine, mask_stream, &masked_tail, &masked_tail_size) == 0);
    streamed_masked.append(masked_tail, masked_tail_size); buffer_free(masked_tail);
    if (streamed_masked != masked_sample) {
      std::cerr << "mask stream mismatch sample=" << sample << "\noriginal=" << original << "\none-shot=" << masked_sample << "\nstream=" << streamed_masked << "\n";
      return 1;
    }
    uint64_t stream = restore_stream_begin(property_engine); assert(stream);
    std::string property_restored;
    for (size_t offset = 0; offset < masked_sample.size();) {
      const size_t count = std::min<size_t>(1 + random() % 17, masked_sample.size() - offset);
      char* chunk = nullptr; size_t chunk_size = 0;
      assert(restore_stream_push(property_engine, stream, masked_sample.data() + offset, count, &chunk, &chunk_size) == 0);
      property_restored.append(chunk, chunk_size); buffer_free(chunk);
      offset += count;
    }
    char* tail = nullptr; size_t tail_size = 0;
    assert(restore_stream_finish(property_engine, stream, &tail, &tail_size) == 0);
    property_restored.append(tail, tail_size); buffer_free(tail);
    if (property_restored != original) {
      std::cerr << "property mismatch sample=" << sample << "\noriginal=" << original << "\nmasked=" << masked_sample << "\nrestored=" << property_restored << "\n";
      return 1;
    }
  }
  engine_destroy(property_engine);

  const char json_cfg[] = R"({"field_paths":["user.email","items.*.token"]})";
  engine_t* json_engine = engine_create(json_cfg, sizeof(json_cfg)-1); assert(json_engine);
  const std::string json_in = R"({"user":{"email":"alice@example.com","name":"Alice"},"items":[{"token":"abc"}]})";
  assert(json_redact(json_engine, json_in.data(), json_in.size(), &out, &n) == 0);
  std::string json_masked(out, n); buffer_free(out);
  assert(json_masked.find("{{JSON_") != std::string::npos);
  assert(json_masked.find("\"name\":\"Alice\"") != std::string::npos);
  const std::string formatted_json = "{\n  \"note\" : \"keep\", \"user\" : { \"email\" : \"alice@example.com\" }\n}";
  assert(json_redact(json_engine, formatted_json.data(), formatted_json.size(), &out, &n) == 0);
  std::string formatted_masked(out, n); buffer_free(out);
  const size_t formatted_token_start = formatted_masked.find("{{JSON_");
  assert(formatted_token_start != std::string::npos);
  const size_t formatted_token_end = formatted_masked.find("}}", formatted_token_start);
  assert(formatted_token_end != std::string::npos);
  const std::string formatted_token = formatted_masked.substr(formatted_token_start, formatted_token_end + 2 - formatted_token_start);
  std::string expected_formatted_json = formatted_json;
  const std::string email_literal = "\"alice@example.com\"";
  const size_t email_offset = expected_formatted_json.find(email_literal);
  assert(email_offset != std::string::npos);
  expected_formatted_json.replace(email_offset, email_literal.size(), "\"" + formatted_token + "\"");
  assert(formatted_masked == expected_formatted_json);
  engine_destroy(json_engine);

  // A failed buffered stream is terminal: callers must not be able to reuse
  // a partially parsed handle after the ABI has reported the error.
  const char invalid_json_cfg[] = R"({"field_paths":["user.email"]})";
  engine_t* invalid_stream_engine = engine_create(invalid_json_cfg, sizeof(invalid_json_cfg) - 1); assert(invalid_stream_engine);
  uint64_t invalid_stream = mask_stream_begin(invalid_stream_engine); assert(invalid_stream);
  const std::string invalid_json = R"({"user":)";
  assert(mask_stream_push(invalid_stream_engine, invalid_stream, invalid_json.data(), invalid_json.size(), &out, &n) == 0);
  buffer_free(out);
  assert(mask_stream_finish(invalid_stream_engine, invalid_stream, &out, &n) != 0);
  assert(mask_stream_abort(invalid_stream_engine, invalid_stream) != 0);
  buffer_free(out);
  engine_destroy(invalid_stream_engine);

  const char* json_combined_cfg = R"({"rules":[{"needle":"secret","label":"SECRET"}],"field_paths":["user.email"]})";
  engine_t* combined_engine = engine_create(json_combined_cfg, strlen(json_combined_cfg)); assert(combined_engine);
  const std::string combined_json = R"({"user":{"email":"alice@example.com"},"note":"secret"})";
  assert(json_redact(combined_engine, combined_json.data(), combined_json.size(), &out, &n) == 0);
  std::string combined_masked(out, n); buffer_free(out);
  assert(combined_masked.find("alice@example.com") == std::string::npos);
  assert(combined_masked.find("secret") == std::string::npos);
  assert(combined_masked.find("{{JSON_") != std::string::npos);
  assert(combined_masked.find("{{SECRET_") != std::string::npos);
  engine_destroy(combined_engine);

  const char* json_defaults_cfg = R"({"field_paths":["user.name"]})";
  engine_t* defaults_engine = engine_create(json_defaults_cfg, strlen(json_defaults_cfg)); assert(defaults_engine);
  const std::string defaults_json = R"({"user":{"name":"Alice","email":"alice@example.com"}})";
  assert(json_redact(defaults_engine, defaults_json.data(), defaults_json.size(), &out, &n) == 0);
  std::string defaults_masked(out, n); buffer_free(out);
  assert(defaults_masked.find("alice@example.com") == std::string::npos);
  engine_destroy(defaults_engine);

  const char json_index_cfg[] = R"({"field_paths":["/items/0/token"]})";
  engine_t* index_engine = engine_create(json_index_cfg, sizeof(json_index_cfg) - 1); assert(index_engine);
  const std::string indexed_json = R"({"items":[{"token":"first"},{"token":"second"}]})";
  assert(json_redact(index_engine, indexed_json.data(), indexed_json.size(), &out, &n) == 0);
  std::string indexed_masked(out, n); buffer_free(out);
  assert(indexed_masked.find("{{JSON_") != std::string::npos);
  assert(indexed_masked.find("\"token\":\"second\"") != std::string::npos);
  offset_map_t* json_offsets = nullptr;
  assert(mask_request_with_map(index_engine, indexed_json.data(), indexed_json.size(), &out, &n, &json_offsets) == 0);
  std::string indexed_mapped(out, n); buffer_free(out);
  assert(indexed_mapped == indexed_masked);
  size_t mapped_offset = 0;
  assert(offset_map_lookup(json_offsets, indexed_mapped.size(), &mapped_offset) == 0);
  assert(mapped_offset == indexed_json.size());
  offset_map_free(json_offsets);
  engine_destroy(index_engine);

  // Request-scoped masking keeps placeholder bytes stable even when the
  // sensitive value changes, while each response still restores its own value.
  const char scoped_cfg[] = R"({"field_paths":["user.email"]})";
  engine_t* scoped_engine = engine_create(scoped_cfg, sizeof(scoped_cfg) - 1); assert(scoped_engine);
  const std::string scoped_one = R"({"model":"m","user":{"email":"alice@example.com"},"note":"alice@example.com bob@example.com","prompt":"stable"})";
  const std::string scoped_two = R"({"model":"m","user":{"email":"carol@example.com"},"note":"dave@example.com dave@example.com","prompt":"stable"})";
  auto scoped_mask = [&](const std::string& source) {
    uint64_t id = mask_stream_begin_scoped(scoped_engine); assert(id);
    char* chunk = nullptr; size_t chunk_size = 0;
    assert(mask_stream_push(scoped_engine, id, source.data(), source.size(), &chunk, &chunk_size) == 0);
    buffer_free(chunk);
    assert(mask_stream_finish(scoped_engine, id, &chunk, &chunk_size) == 0);
    std::string masked_value(chunk, chunk_size); buffer_free(chunk);
    return std::pair<uint64_t, std::string>{id, std::move(masked_value)};
  };
  auto scoped_a = scoped_mask(scoped_one);
  auto scoped_b = scoped_mask(scoped_two);
  assert(scoped_a.second == scoped_b.second);
  assert(scoped_a.second.find("alice@example.com") == std::string::npos);
  assert(scoped_a.second.find("bob@example.com") == std::string::npos);
  const size_t email_token_start = scoped_a.second.find("{{EMAIL_");
  assert(email_token_start != std::string::npos);
  const size_t email_token_end = scoped_a.second.find("}}", email_token_start);
  assert(email_token_end != std::string::npos);
  const std::string scoped_email_token = scoped_a.second.substr(email_token_start, email_token_end + 2 - email_token_start);
  const size_t second_email_token = scoped_a.second.find("{{EMAIL_", email_token_end + 2);
  assert(second_email_token != std::string::npos);
  const size_t second_email_token_end = scoped_a.second.find("}}", second_email_token);
  assert(second_email_token_end != std::string::npos);
  const std::string scoped_second_email_token = scoped_a.second.substr(second_email_token, second_email_token_end + 2 - second_email_token);
  assert(scoped_second_email_token != scoped_email_token);
  assert(scoped_a.second.find("{{EMAIL_", second_email_token_end + 2) == std::string::npos);
  auto scoped_restore = [&](uint64_t mask_id, const std::string& masked_value) {
    uint64_t restore_id = restore_stream_begin_for_mask(scoped_engine, mask_id); assert(restore_id);
    assert(mask_stream_release(scoped_engine, mask_id) == 0);
    char* chunk = nullptr; size_t chunk_size = 0;
    assert(restore_stream_push(scoped_engine, restore_id, masked_value.data(), masked_value.size(), &chunk, &chunk_size) == 0);
    std::string restored_value(chunk, chunk_size); buffer_free(chunk);
    assert(restore_stream_finish(scoped_engine, restore_id, &chunk, &chunk_size) == 0);
    restored_value.append(chunk, chunk_size); buffer_free(chunk);
    return restored_value;
  };
  std::string scoped_restored_a, scoped_restored_b;
  std::thread scoped_worker_a([&] { scoped_restored_a = scoped_restore(scoped_a.first, scoped_a.second); });
  std::thread scoped_worker_b([&] { scoped_restored_b = scoped_restore(scoped_b.first, scoped_b.second); });
  scoped_worker_a.join();
  scoped_worker_b.join();
  assert(scoped_restored_a == scoped_one);
  assert(scoped_restored_b == scoped_two);
  engine_destroy(scoped_engine);

  engine_t* concurrent = engine_create(cfg, sizeof(cfg)-1); assert(concurrent);
  std::vector<std::thread> workers;
  for (int i = 0; i < 8; ++i) {
    workers.emplace_back([concurrent, &in] {
      for (int j = 0; j < 100; ++j) {
        char* masked_out = nullptr; size_t masked_len = 0;
        assert(mask_request(concurrent, in.data(), in.size(), &masked_out, &masked_len) == 0);
        const std::string expected(masked_out, masked_len);
        uint64_t stream = restore_stream_begin(concurrent); assert(stream);
        char* restored_out = nullptr; size_t restored_len = 0;
        assert(restore_stream_push(concurrent, stream, expected.data(), expected.size(), &restored_out, &restored_len) == 0);
        buffer_free(masked_out); buffer_free(restored_out);
        assert(restore_stream_finish(concurrent, stream, &restored_out, &restored_len) == 0);
        buffer_free(restored_out);

        uint64_t mask = mask_stream_begin(concurrent); assert(mask);
        std::string streamed;
        assert(mask_stream_push(concurrent, mask, in.data(), in.size(), &restored_out, &restored_len) == 0);
        streamed.append(restored_out, restored_len); buffer_free(restored_out);
        assert(mask_stream_finish(concurrent, mask, &restored_out, &restored_len) == 0);
        streamed.append(restored_out, restored_len); buffer_free(restored_out);
        assert(streamed == expected);
      }
    });
  }
  for (auto& worker : workers) worker.join();
  engine_destroy(concurrent);

  engine_t* defaults = engine_create(nullptr, 0); assert(defaults);
  const std::string email = "alice@example.com";
  assert(mask_request(defaults, email.data(), email.size(), &out, &n) == 0);
  assert(std::string(out, n).find("{{EMAIL_") != std::string::npos); buffer_free(out);
  const std::string multi = "alice@sub.example.com Bearer abcdefghijkl==";
  assert(mask_request(defaults, multi.data(), multi.size(), &out, &n) == 0);
  const std::string multi_masked(out, n); buffer_free(out);
  assert(multi_masked.find("alice@sub.example.com") == std::string::npos);
  assert(multi_masked.find("Bearer abcdefghijkl==") == std::string::npos);
  const std::string invalid_bearer = "Bearer ============";
  assert(mask_request(defaults, invalid_bearer.data(), invalid_bearer.size(), &out, &n) == 0);
  assert(std::string(out, n) == invalid_bearer); buffer_free(out);

  const std::string stream_input = "prefix alice@example.com secret tail";
  engine_t* stream_engine = engine_create(cfg, sizeof(cfg) - 1); assert(stream_engine);
  uint64_t mask_stream = mask_stream_begin(stream_engine); assert(mask_stream);
  std::string stream_masked;
  const std::string stream_a = "prefix alice@exa";
  const std::string stream_b = "mple.com secret tail";
  assert(mask_stream_push(stream_engine, mask_stream, stream_a.data(), stream_a.size(), &out, &n) == 0);
  stream_masked.append(out, n); buffer_free(out);
  assert(stream_masked.find("alice@") == std::string::npos);
  assert(mask_stream_push(stream_engine, mask_stream, stream_b.data(), stream_b.size(), &out, &n) == 0);
  stream_masked.append(out, n); buffer_free(out);
  assert(mask_stream_finish(stream_engine, mask_stream, &out, &n) == 0);
  stream_masked.append(out, n); buffer_free(out);
  assert(stream_masked.find("alice@example.com") == std::string::npos);
  assert(stream_masked.find("secret") == std::string::npos);
  uint64_t stream_restore = restore_stream_begin(stream_engine); assert(stream_restore);
  assert(restore_stream_push(stream_engine, stream_restore, stream_masked.data(), stream_masked.size(), &out, &n) == 0);
  std::string stream_restored(out, n); buffer_free(out);
  assert(restore_stream_finish(stream_engine, stream_restore, &out, &n) == 0);
  stream_restored.append(out, n); buffer_free(out);
  assert(stream_restored == stream_input);
  engine_destroy(stream_engine);

  uint64_t bearer_stream = mask_stream_begin(defaults); assert(bearer_stream);
  const std::string bearer_a = "Bearer abc";
  const std::string bearer_b = "defghijkl==";
  std::string bearer_masked;
  assert(mask_stream_push(defaults, bearer_stream, bearer_a.data(), bearer_a.size(), &out, &n) == 0);
  bearer_masked.append(out, n); buffer_free(out);
  assert(mask_stream_push(defaults, bearer_stream, bearer_b.data(), bearer_b.size(), &out, &n) == 0);
  bearer_masked.append(out, n); buffer_free(out);
  assert(mask_stream_finish(defaults, bearer_stream, &out, &n) == 0);
  bearer_masked.append(out, n); buffer_free(out);
  assert(bearer_masked.find("Bearer abcdefghijkl==") == std::string::npos);

  const std::string split_bearer_input =
      "prefix xalice@example.com Bearer abcdefghijkl== suffix";
  assert(mask_request(defaults, split_bearer_input.data(), split_bearer_input.size(), &out, &n) == 0);
  const std::string split_bearer_expected(out, n); buffer_free(out);
  uint64_t split_bearer_stream = mask_stream_begin(defaults); assert(split_bearer_stream);
  std::string split_bearer_actual;
  size_t split_bearer_offset = 0;
  const std::vector<size_t> split_bearer_chunks = {11, 2, 2, 4, 1, 5, 6, 4, 2, 2, 2, 5, 3, 5};
  for (size_t count : split_bearer_chunks) {
    char* chunk = nullptr; size_t chunk_size = 0;
    assert(mask_stream_push(defaults, split_bearer_stream, split_bearer_input.data() + split_bearer_offset,
                            count, &chunk, &chunk_size) == 0);
    split_bearer_actual.append(chunk, chunk_size); buffer_free(chunk);
    split_bearer_offset += count;
  }
  assert(split_bearer_offset == split_bearer_input.size());
  assert(mask_stream_finish(defaults, split_bearer_stream, &out, &n) == 0);
  split_bearer_actual.append(out, n); buffer_free(out);
  assert(split_bearer_actual == split_bearer_expected);
  assert(split_bearer_actual.find("Bearer abcdefghijkl==") == std::string::npos);

  // A continuous built-in candidate must not retain an unbounded streaming
  // suffix. The safe behavior is to fail closed and retire the stream rather
  // than emit a potentially sensitive prefix.
  uint64_t oversized_candidate_stream = mask_stream_begin(defaults); assert(oversized_candidate_stream);
  std::string oversized_candidate((1u << 20) + 1, 'a');
  assert(mask_stream_push(defaults, oversized_candidate_stream, oversized_candidate.data(), oversized_candidate.size(), &out, &n) != 0);
  buffer_free(out);
  assert(mask_stream_abort(defaults, oversized_candidate_stream) != 0);

  for (int sample = 0; sample < 200; ++sample) {
    const std::string default_input = "prefix " + std::string(sample % 3, 'x') +
        "alice@example.com Bearer abcdefghijkl== suffix";
    assert(mask_request(defaults, default_input.data(), default_input.size(), &out, &n) == 0);
    std::string expected(out, n); buffer_free(out);
    uint64_t random_stream = mask_stream_begin(defaults); assert(random_stream);
    std::string actual;
    for (size_t offset = 0; offset < default_input.size();) {
      const size_t count = std::min<size_t>(1 + random() % 11, default_input.size() - offset);
      char* chunk = nullptr; size_t chunk_size = 0;
      assert(mask_stream_push(defaults, random_stream, default_input.data() + offset, count, &chunk, &chunk_size) == 0);
      actual.append(chunk, chunk_size); buffer_free(chunk);
      offset += count;
    }
    assert(mask_stream_finish(defaults, random_stream, &out, &n) == 0);
    actual.append(out, n); buffer_free(out);
    if (actual != expected) {
      std::cerr << "default mask stream mismatch sample=" << sample << "\n";
      return 1;
    }
  }

  const char stream_json_cfg[] = R"({"field_paths":["user.email"]})";
  engine_t* stream_json = engine_create(stream_json_cfg, sizeof(stream_json_cfg) - 1); assert(stream_json);
  const std::string stream_json_input = R"({"user":{"email":"alice@example.com"}})";
  assert(mask_request(stream_json, stream_json_input.data(), stream_json_input.size(), &out, &n) == 0);
  const std::string one_shot_json_masked(out, n); buffer_free(out);
  uint64_t json_mask_stream = mask_stream_begin(stream_json); assert(json_mask_stream);
  assert(mask_stream_push(stream_json, json_mask_stream, stream_json_input.data(), 12, &out, &n) == 0);
  assert(n == 0); buffer_free(out);
  assert(mask_stream_push(stream_json, json_mask_stream, stream_json_input.data() + 12, stream_json_input.size() - 12, &out, &n) == 0);
  assert(n == 0); buffer_free(out);
  assert(mask_stream_finish(stream_json, json_mask_stream, &out, &n) == 0);
  std::string stream_json_masked(out, n); buffer_free(out);
  assert(stream_json_masked == one_shot_json_masked);
  assert(stream_json_masked.find("alice@example.com") == std::string::npos);
  assert(stream_json_masked.find("{{JSON_") != std::string::npos);
  engine_destroy(stream_json);

  s = restore_stream_begin(defaults); assert(s);
  assert(restore_stream_abort(defaults, s) == 0);
  assert(restore_stream_finish(defaults, s, &out, &n) != 0);
  engine_destroy(defaults);
  const char invalid[] = R"({"rules":[{"pattern":"["}]})";
  assert(engine_create(invalid, sizeof(invalid) - 1) == nullptr);
  const char nested_quantifier[] = R"({"rules":[{"pattern":"(a+)+"}]})";
  assert(engine_create(nested_quantifier, sizeof(nested_quantifier) - 1) == nullptr);
  const char repeated_quantifier[] = R"({"rules":[{"pattern":"a++"}]})";
  assert(engine_create(repeated_quantifier, sizeof(repeated_quantifier) - 1) == nullptr);
  std::string oversized_pattern = R"({"rules":[{"pattern":")";
  oversized_pattern.append(4097, 'a');
  oversized_pattern += R"("}]})";
  assert(engine_create(oversized_pattern.data(), oversized_pattern.size()) == nullptr);
  const char custom_label[] = R"({"rules":[{"pattern":"foo[0-9]+","label":"EMAIL"}]})";
  engine_t* custom = engine_create(custom_label, sizeof(custom_label) - 1); assert(custom);
  const std::string custom_input = "foo123";
  assert(mask_request(custom, custom_input.data(), custom_input.size(), &out, &n) == 0);
  assert(std::string(out, n).find("{{EMAIL_") != std::string::npos); buffer_free(out);
  engine_destroy(custom);

  const char custom_regex_cfg[] = R"JSON({"rules":[{"pattern":"EMP-[0-9]{6}","label":"CUSTOM"}]})JSON";
  engine_t* custom_regex = engine_create(custom_regex_cfg, sizeof(custom_regex_cfg) - 1); assert(custom_regex);
  const std::string custom_regex_input = "EMP-123456";
  assert(mask_request(custom_regex, custom_regex_input.data(), custom_regex_input.size(), &out, &n) == 0);
  assert(std::string(out, n).find("{{CUSTOM_") != std::string::npos); buffer_free(out);
  engine_destroy(custom_regex);

  auto mask_with_config = [](const std::string& config, const std::string& input) {
    engine_t* engine = engine_create(config.data(), config.size()); assert(engine);
    char* result = nullptr; size_t result_size = 0;
    assert(mask_request(engine, input.data(), input.size(), &result, &result_size) == 0);
    std::string masked(result, result_size); buffer_free(result); engine_destroy(engine);
    return masked;
  };

  const std::string card_config = R"({"rules":[{"pattern":"[3-6][0-9]{12,18}","label":"CARD","semantic":true}]})";
  assert(mask_with_config(card_config, "card 4111111111111111") != "card 4111111111111111");
  assert(mask_with_config(card_config, "card 4111111111111112") == "card 4111111111111112");
  assert(mask_with_config(card_config, "card 0000000000000026") == "card 0000000000000026");
  assert(mask_with_config(card_config, "size 313524224 2023") == "size 313524224 2023");
  const std::string chinese_email = mask_with_config(R"JSON({"rules":[{"pattern":"([^\\s@]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,})","label":"EMAIL","semantic":true}]})JSON", "联系张三@qq.com");
  assert(chinese_email.find("张三@qq.com") == std::string::npos);
  for (const std::string& separator : {std::string("："), std::string("，"), std::string("；")}) {
    const std::string prefix = "请只原样返回这个合成测试邮箱" + separator;
    const std::string probe = "silencer-probe-20261019@example.com";
    const std::string prose_masked = mask_with_config(long_email_config, prefix + probe);
    assert(prose_masked.rfind(prefix, 0) == 0);
    assert(prose_masked.find(probe) == std::string::npos);
    assert(prose_masked.find("{{EMAIL_") != std::string::npos);
  }
  const std::string conn_as_email = mask_with_config(R"JSON({"rules":[{"pattern":"([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,})","label":"EMAIL","semantic":true}]})JSON", "postgres://user:secret123@db.internal");
  assert(conn_as_email.find("secret123@db.internal") != std::string::npos);

  const std::string id_config = R"JSON({"rules":[{"pattern":"((1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{15}[0-9Xx]|(1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{13})","label":"IDCARD","semantic":true}]})JSON";
  assert(mask_with_config(id_config, "id 110101199003074514") != "id 110101199003074514");
  assert(mask_with_config(id_config, "id 11010119900307451X") == "id 11010119900307451X");
  assert(mask_with_config(id_config, "id 065217391304348") == "id 065217391304348");

  const std::string private_ip_config = R"JSON({"rules":[{"pattern":"(192\\.168|169\\.254)\\.[0-9]{1,3}\\.[0-9]{1,3}","label":"IP_PRIVATE","semantic":true}]})JSON";
  assert(mask_with_config(private_ip_config, "host 192.168.1.1") != "host 192.168.1.1");
  assert(mask_with_config(private_ip_config, "host 192.168.999.1") == "host 192.168.999.1");
  assert(mask_with_config(private_ip_config, "host 192.168.1.1.1") == "host 192.168.1.1.1");

  const std::string internal_ip_config = R"JSON({"rules":[{"pattern":"(10\\.[0-9]{1,3}|172\\.(1[6-9]|2[0-9]|3[01]))\\.[0-9]{1,3}\\.[0-9]{1,3}","label":"IP_INTERNAL","semantic":true}]})JSON";
  assert(mask_with_config(internal_ip_config, "version 10.2.3.4").find("10.2.3.4") == std::string::npos);
  assert(mask_with_config(internal_ip_config, "host 10.1.2.3").find("10.1.2.3") == std::string::npos);

  const std::string public_ip_config = R"JSON({"rules":[{"pattern":"([0-9]\\.|[0-9][0-9]\\.|[0-9][0-9][0-9]\\.){3}[0-9]{1,3}","label":"IP_PUBLIC","semantic":true}]})JSON";
  assert(mask_with_config(public_ip_config, "dns 8.8.8.8 1.1.1.1 114.114.114.114") == "dns 8.8.8.8 1.1.1.1 114.114.114.114");
  assert(mask_with_config(public_ip_config, "host 1.2.3.45").find("1.2.3.45") == std::string::npos);

  const std::string secret_config = R"JSON({"rules":[{"pattern":"(password[=:]([A-Za-z0-9!@#$%^&*_~+=-]{6,64}))","label":"SECRET","capture_group":2}]})JSON";
  const std::string secret_masked = mask_with_config(secret_config, "password=Abc123!e");
  assert(secret_masked.find("password=") == 0 && secret_masked.find("Abc123!e") == std::string::npos);
  assert(secret_masked.find("{{SECRET_") != std::string::npos);
  const std::string credential_false_positive_config = R"JSON({"rules":[{"pattern":"((?:^|[^A-Za-z0-9_.])(?:password|passwd|pwd|secret|token|api[_-]?key|密码|口令|令牌|密钥)[\"'“”]?\\s*[:=：＝]\\s*[\"'“”]?(?!/)(?=[A-Za-z0-9!@#$%^&*_~+=-]*[0-9!@#$%^&*])([A-Za-z0-9!@#$%^&*_~+=-]{6,64})(?![A-Za-z0-9!@#$%^&*_~+=-]))","label":"SECRET","capture_group":2}]})JSON";
  assert(mask_with_config(credential_false_positive_config, "password=Hn8x!qW2zLm9pR").find("Hn8x!qW2zLm9pR") == std::string::npos);
  assert(mask_with_config(credential_false_positive_config, "mypassword=Hn8x!qW2zLm9pR").find("Hn8x!qW2zLm9pR") != std::string::npos);
  assert(mask_with_config(credential_false_positive_config, "token=abcdefgh").find("token=abcdefgh") != std::string::npos);
  assert(mask_with_config(credential_false_positive_config, "obj.secret = getSecret()").find("getSecret()") != std::string::npos);
  assert(mask_with_config(credential_false_positive_config, "/token=/api_key=").find("/token=/api_key=") != std::string::npos);

  const std::string conn_config = R"JSON({"rules":[{"pattern":"[a-z]+://[^:@/]+:([^@/]{4,})@","label":"CONNSTR","capture_group":1}]})JSON";
  const std::string conn_masked = mask_with_config(conn_config, "postgres://user:secret123@db.internal:5432/app");
  assert(conn_masked.find("postgres://user:") == 0);
  assert(conn_masked.find("secret123") == std::string::npos);
  assert(conn_masked.find("@db.internal:5432/app") != std::string::npos);

  const std::string bearer_config = R"JSON({"rules":[{"pattern":"[Bb][Ee][Aa][Rr][Ee][Rr]\\s+([A-Za-z0-9._~+/-]{20,}=*)","label":"BEARER","capture_group":1}]})JSON";
  const std::string bearer_capture_masked = mask_with_config(bearer_config, "Bearer abcdefghijklmnopqrst");
  assert(bearer_capture_masked.find("Bearer ") == 0 && bearer_capture_masked.find("abcdefghijklmnopqrst") == std::string::npos);

  const std::string phone_config = R"JSON({"rules":[{"pattern":"(\\+?86|0086|\\(\\+?86\\)|（\\+?86）)([ -])?1[3-9][0-9]{9}","label":"PHONE","semantic":true}]})JSON";
  assert(mask_with_config(phone_config, "phone (86)13800138000").find("(86)13800138000") == std::string::npos);
  assert(mask_with_config(phone_config, "phone （+86）13800138000").find("（+86）13800138000") == std::string::npos);

  const std::string email_config = R"JSON({"rules":[{"pattern":"([A-Za-z0-9_\\x80-\\xFF][-A-Za-z0-9._%+\\x80-\\xFF]{0,63}@[-A-Za-z0-9.\\x80-\\xFF]+\\.[A-Za-z\\x80-\\xFF]{2,})","label":"EMAIL","semantic":true}]})JSON";
  assert(mask_with_config(email_config, "mail 张三@qq.com").find("张三@qq.com") == std::string::npos);
  assert(mask_with_config(email_config, "short a@b.c and real ab@cd.ef").find("a@b.c") != std::string::npos);

  const std::string iban_config = R"JSON({"rules":[{"pattern":"[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}","label":"IBAN","semantic":true}]})JSON";
  assert(mask_with_config(iban_config, "GB82WEST12345698765432").find("GB82WEST12345698765432") == std::string::npos);
  assert(mask_with_config(iban_config, "GB00WEST12345698765432") == "GB00WEST12345698765432");

  const std::string jwt_config = R"JSON({"rules":[{"pattern":"eyJ[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}\\.[A-Za-z0-9_-]{8,}","label":"JWT","semantic":true}]})JSON";
  assert(mask_with_config(jwt_config, "eyJhbGciOiJIUzI1NiJ9.abcdefgh.abcdefgh").find("eyJhbGciOiJIUzI1NiJ9") == std::string::npos);
  assert(mask_with_config(jwt_config, "eyJub3RfanNvbiI6MX0.abcdefgh.abcdefgh") == "eyJub3RfanNvbiI6MX0.abcdefgh.abcdefgh");
  assert(mask_with_config(jwt_config, "eyJ4IjoiYWxnIn0.abcdefgh.abcdefgh") == "eyJ4IjoiYWxnIn0.abcdefgh.abcdefgh");

  const std::string ipv6_config = R"JSON({"rules":[{"pattern":"(fe[89AaBb]|fc|fd)[0-9A-Fa-f:]{1,44}","label":"IPV6_PRIVATE","semantic":true}]})JSON";
  assert(mask_with_config(ipv6_config, "fe80::1").find("fe80::1") == std::string::npos);
  assert(mask_with_config(ipv6_config, "fd12:3456::1").find("fd12:3456::1") == std::string::npos);
  assert(mask_with_config(ipv6_config, "2001:db8::1") == "2001:db8::1");

  const std::string uscc_config = R"JSON({"rules":[{"pattern":"[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}","label":"USCC","semantic":true}]})JSON";
  assert(mask_with_config(uscc_config, "91100000100003962T").find("91100000100003962T") == std::string::npos);
  assert(mask_with_config(uscc_config, "91100000100003962A") == "91100000100003962A");

  const std::string plate_config = R"JSON({"rules":[{"pattern":"(京|津|沪|渝|冀|豫|云|辽|黑|湘|皖|鲁|新|苏|浙|赣|鄂|桂|甘|晋|蒙|陕|吉|闽|贵|粤|青|藏|川|宁|琼|使|领)[A-Z](?=[A-Z0-9]{0,5}[0-9])[A-Z0-9]{5,6}","label":"PLATE","semantic":true}]})JSON";
  assert(mask_with_config(plate_config, "京A12345").find("京A12345") == std::string::npos);
  assert(mask_with_config(plate_config, "京AD12345").find("京AD12345") == std::string::npos);
  assert(mask_with_config(plate_config, "新README") == "新README");

  const std::string vertical_config = R"JSON({"rules":[{"pattern":"\\v","label":"VERTICAL"}]})JSON";
  assert(mask_with_config(vertical_config, "\n\r\f") == "\n\r\f");
  assert(mask_with_config(vertical_config, "\v").find("{{VERTICAL_") != std::string::npos);
  const std::string dot_config = R"JSON({"rules":[{"pattern":"a.b","label":"DOT"}]})JSON";
  assert(mask_with_config(dot_config, "a\rb a\nb") == "a\rb a\nb");
  const std::string identity_config = R"JSON({"rules":[{"pattern":"\\q\\R","label":"IDENTITY"}]})JSON";
  // libstdc++ accepts these ECMAScript identity escapes; libc++ may reject
  // them during the unchanged syntax validation stage.
  bool identity_supported = true;
  try { const std::regex reference(R"(\q\R)"); (void)reference; }
  catch (const std::regex_error&) { identity_supported = false; }
  if (identity_supported) assert(mask_with_config(identity_config, "qR").find("{{IDENTITY_") != std::string::npos);

  // Empty classes, NUL escapes and literal escapes retain ECMAScript spans.
  const std::string any_byte_config = R"JSON({"rules":[{"pattern":"([^])","label":"ANY","capture_group":1}]})JSON";
  assert(mask_with_config(any_byte_config, "\n").find("{{ANY_") != std::string::npos);
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"[]","label":"EMPTY"}]})JSON", "unchanged") == "unchanged");
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"[^]x","label":"ANY"}]})JSON", "\nx").find("{{ANY_") != std::string::npos);
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"\\[\\^\\]","label":"LITERAL"}]})JSON", "[^]").find("{{LITERAL_") != std::string::npos);
  const std::string nul_config = R"JSON({"rules":[{"pattern":"\\01","label":"NUL"}]})JSON";
  assert(mask_with_config(nul_config, std::string("\0" "1", 2)).find("{{NUL_") != std::string::npos);
  assert(mask_with_config(nul_config, std::string(1, '\1')) == std::string(1, '\1'));
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"[\\01]","label":"NUL"}]})JSON", "1").find("{{NUL_") != std::string::npos);
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"\\\\01","label":"LITERAL"}]})JSON", "\\01").find("{{LITERAL_") != std::string::npos);
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"(a)\\1","label":"BACKREF"}]})JSON", "aa").find("{{BACKREF_") != std::string::npos);
  // Preserve std::regex's failure for a backreference whose group never
  // participated, rather than making it match an empty string.
  assert(!std::regex_search(std::string("a"), std::regex(R"((a|(b))+\2)")));
  assert(mask_with_config(R"JSON({"rules":[{"pattern":"(a|(b))+\\2","label":"BACKREF"}]})JSON", "a") == "a");

  // Non-semantic email patterns follow their expression, including odd but
  // matched local/domain strings. Semantic rules keep their PII validation.
  const std::string ascii_pattern = R"(([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}))";
  const std::string ascii_email_config = R"JSON({"rules":[{"pattern":"([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,})","label":"EMAIL"}]})JSON";
  const std::regex email_reference(ascii_pattern);
  const std::regex email_token_reference(R"(\{\{EMAIL_[^}]+\}\})");
  for (const std::string& input : std::vector<std::string>{":user@example.com", "mail:user@example.com", "a..b@example.com", "a@-example.com", "éuser@example.com", "a@example.com.b@example.org", "a@example.com.", "postgres://user:secret123@db.internal"}) {
    const std::string expected = std::regex_replace(input, email_reference, "<mask>");
    const std::string actual = std::regex_replace(mask_with_config(ascii_email_config, input), email_token_reference, "<mask>");
    assert(actual == expected);
  }
}
