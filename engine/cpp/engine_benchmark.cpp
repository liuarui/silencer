#include "engine.h"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <iostream>
#include <numeric>
#include <stdexcept>
#include <string>
#include <vector>

#if defined(__unix__) || defined(__APPLE__)
#include <sys/resource.h>
#endif

namespace {

constexpr char kEmail[] = "perf.user@example.test";
constexpr char kFiller[] = "常规文本 neutral prompt without personal data. ";

size_t count_substring(const std::string& text, const std::string& needle) {
  size_t count = 0;
  size_t offset = 0;
  while ((offset = text.find(needle, offset)) != std::string::npos) {
    ++count;
    offset += needle.size();
  }
  return count;
}

std::string quote_json(const std::string& value) {
  std::string quoted = "\"";
  for (unsigned char byte : value) {
    if (byte == '"' || byte == '\\') quoted.push_back('\\');
    if (byte < 0x20) throw std::runtime_error("benchmark config contains a control byte");
    quoted.push_back(static_cast<char>(byte));
  }
  quoted.push_back('"');
  return quoted;
}

std::string default_enabled_rules_config() {
  const std::vector<std::pair<std::string, std::string>> rules = {
      {"API_KEY", R"((ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{20,})"},
      {"API_KEY", R"(github_pat_[A-Za-z0-9_]{50,})"},
      {"API_KEY", R"(AIza[0-9A-Za-z_-]{35,38})"},
      {"API_KEY", R"(xox[baprs]-[0-9A-Za-z-]{10,})"},
      {"API_KEY", R"([sr]k_(live|test)_[0-9A-Za-z]{20,})"},
      {"API_KEY", R"(cli_[a-z0-9]{16,})"},
      {"API_KEY", R"(ding[a-z0-9]{6,})"},
      {"CARD", R"([3-6][0-9]{12,18})"},
      {"CONNSTR", R"([a-z][a-z0-9+.-]{0,63}://[^\s:@/]+:[^\s@/]{4,}@)"},
      {"EMAIL", R"(([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}))"},
      {"IDCARD", R"(((1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{13}|(1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{15}[0-9Xx]))"},
      {"LANDLINE", R"(0(10|2[0-9]|[3-9][0-9]{2})[- ]?[2-9][0-9]{6,7})"},
      {"PHONE", R"(1[3-9][0-9]{9})"},
      {"PHONE", R"(\+861[3-9][0-9]{9})"},
      {"PHONE", R"(00861[3-9][0-9]{9})"},
      {"PHONE", R"(1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4})"},
      {"PHONE", R"((\+86|0086)[ -]?1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4})"},
  };
  std::string config = R"({"rules":[)";
  for (size_t i = 0; i < rules.size(); ++i) {
    if (i) config.push_back(',');
    config += "{\"pattern\":" + quote_json(rules[i].second) +
        ",\"label\":" + quote_json(rules[i].first) + "}";
  }
  config += "]}";
  return config;
}

std::string fit_filler(size_t byte_count) {
  if (byte_count == 0) return {};
  const std::string unit(kFiller);
  std::string expanded;
  expanded.reserve(byte_count + unit.size());
  while (expanded.size() < byte_count + unit.size()) expanded += unit;
  size_t end = byte_count;
  while (end > 0 && end < expanded.size() &&
         (static_cast<unsigned char>(expanded[end]) & 0xc0) == 0x80) {
    --end;
  }
  return expanded.substr(0, end) + std::string(byte_count - end, 'x');
}

size_t midpoint_codepoint(const std::string& text) {
  size_t codepoints = 0;
  for (unsigned char byte : text) {
    if ((byte & 0xc0) != 0x80) ++codepoints;
  }
  const size_t target = codepoints / 2;
  size_t seen = 0;
  for (size_t offset = 0; offset < text.size(); ++offset) {
    if ((static_cast<unsigned char>(text[offset]) & 0xc0) == 0x80) continue;
    if (seen++ == target) return offset;
  }
  return text.size();
}

std::string make_text(size_t byte_count, const std::string& density) {
  const std::string email(kEmail);
  if (density == "none") return fit_filler(byte_count);
  if (density == "sparse") {
    if (byte_count < email.size() + 2) return email.substr(0, byte_count);
    std::string base = fit_filler(byte_count - email.size() - 2);
    const size_t middle = midpoint_codepoint(base);
    base.insert(middle, " " + email + " ");
    return base;
  }
  if (density == "dense") {
    const std::string unit = email + " ";
    const size_t repeats = byte_count / unit.size();
    std::string result;
    result.reserve(byte_count);
    for (size_t i = 0; i < repeats; ++i) result += unit;
    result += fit_filler(byte_count - result.size());
    return result;
  }
  throw std::runtime_error("unsupported density: " + density);
}

std::string make_json(const std::string& text) {
  return "{\"model\":\"benchmark-model\",\"messages\":[{\"role\":\"user\",\"content\":\"" +
      text + "\"}],\"stream\":false}";
}

double percentile(const std::vector<double>& samples, double fraction) {
  std::vector<double> ordered = samples;
  std::sort(ordered.begin(), ordered.end());
  const size_t index = static_cast<size_t>(std::ceil(fraction * ordered.size())) - 1;
  return ordered[std::min(index, ordered.size() - 1)];
}

struct ProcessUsage {
  double cpu_ms = 0.0;
  double max_rss_mib = 0.0;
};

ProcessUsage process_usage() {
#if defined(__unix__) || defined(__APPLE__)
  struct rusage usage {};
  if (getrusage(RUSAGE_SELF, &usage) != 0) return {};
  const double cpu_ms =
      (static_cast<double>(usage.ru_utime.tv_sec) * 1000.0) +
      (static_cast<double>(usage.ru_utime.tv_usec) / 1000.0) +
      (static_cast<double>(usage.ru_stime.tv_sec) * 1000.0) +
      (static_cast<double>(usage.ru_stime.tv_usec) / 1000.0);
#if defined(__APPLE__)
  const double rss_mib = static_cast<double>(usage.ru_maxrss) / (1024.0 * 1024.0);
#else
  const double rss_mib = static_cast<double>(usage.ru_maxrss) / 1024.0;
#endif
  return {cpu_ms, rss_mib};
#else
  return {};
#endif
}

bool run_case(size_t size, const std::string& density, bool json, int rounds) {
  const std::string text = make_text(size, density);
  const std::string input = json ? make_json(text) : text;
  const size_t expected_hits = count_substring(text, kEmail);
  const std::string config = default_enabled_rules_config();
  engine_t* engine = engine_create(config.data(), config.size());
  if (!engine) {
    std::cerr << "engine_create: " << last_error() << '\n';
    return false;
  }

  std::vector<double> samples;
  std::string last_output;
  const ProcessUsage usage_before = process_usage();
  for (int iteration = -2; iteration < rounds; ++iteration) {
    char* output = nullptr;
    size_t output_len = 0;
    const auto start = std::chrono::steady_clock::now();
    const int rc = mask_request(engine, input.data(), input.size(), &output, &output_len);
    const auto end = std::chrono::steady_clock::now();
    if (rc != 0) {
      std::cerr << "mask_request: " << last_error() << '\n';
      engine_destroy(engine);
      return false;
    }
    last_output.assign(output, output_len);
    buffer_free(output);
    if (iteration >= 0) {
      samples.push_back(std::chrono::duration<double, std::milli>(end - start).count());
    }
  }
  const ProcessUsage usage_after = process_usage();

  const size_t observed_hits = count_substring(last_output, "{{EMAIL_");
  if (observed_hits != expected_hits || last_output.find(kEmail) != std::string::npos) {
    std::cerr << "verification failed for " << (json ? "json" : "text") << '/' << density
              << ": expected " << expected_hits << " hits, observed " << observed_hits << '\n';
    engine_destroy(engine);
    return false;
  }

  const double mean_ms = std::accumulate(samples.begin(), samples.end(), 0.0) / samples.size();
  const double mib_s = input.size() / (mean_ms / 1000.0) / (1024.0 * 1024.0);
  const double cpu_ms_per_call = (usage_after.cpu_ms - usage_before.cpu_ms) / (rounds + 2);
  std::cout << "mask." << (json ? "json" : "text") << '.' << size << '.' << density
            << " input_bytes=" << input.size() << " hits=" << observed_hits
            << " samples=" << samples.size() << " verified=true p50_ms=" << percentile(samples, 0.50)
            << " p95_ms=" << percentile(samples, 0.95)
            << " p99_ms=" << percentile(samples, 0.99)
            << " mean_ms=" << mean_ms << " MiB_s=" << mib_s << '\n';
  std::cout << "mask." << (json ? "json" : "text") << '.' << size << '.' << density
            << " cpu_ms_per_call=" << cpu_ms_per_call
            << " max_rss_mib=" << usage_after.max_rss_mib << '\n';
  engine_destroy(engine);
  return true;
}

}  // namespace

int main(int argc, char** argv) {
  size_t size = 256 * 1024;
  int rounds = 101;
  for (int i = 1; i < argc; ++i) {
    const std::string arg = argv[i];
    if (arg == "--size" && i + 1 < argc) {
      size = static_cast<size_t>(std::strtoull(argv[++i], nullptr, 10));
    } else if (arg == "--rounds" && i + 1 < argc) {
      rounds = std::atoi(argv[++i]);
    } else {
      std::cerr << "usage: engine_benchmark [--size BYTES] [--rounds COUNT]\n";
      return 2;
    }
  }
  if (size == 0 || rounds < 1) {
    std::cerr << "size and rounds must be positive\n";
    return 2;
  }

  std::cout << std::fixed << std::setprecision(4);
  bool ok = true;
  for (const std::string& density : {"none", "sparse", "dense"}) {
    ok = run_case(size, density, false, rounds) && ok;
  }
  for (const std::string& density : {"none", "sparse", "dense"}) {
    ok = run_case(size, density, true, rounds) && ok;
  }
  return ok ? 0 : 1;
}
