#include "engine.h"
#include "reusable_tokens.h"

#include <algorithm>
#if defined(_WIN32)
#include <ws2tcpip.h>
#else
#include <arpa/inet.h>
#endif
#include <cctype>
#include <condition_variable>
#include <cstdlib>
#include <ctime>
#include <cstring>
#include <mutex>
#include <memory>
#include <new>
#include <regex>
#include <shared_mutex>
#include <stdexcept>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>
#define PCRE2_CODE_UNIT_WIDTH 8
#include <pcre2.h>

namespace {
silencer::ReusableTokenRegistry reusable_tokens;
thread_local std::string g_last_error;
void clear_error() { g_last_error.clear(); }
int fail(const char* msg) { g_last_error = msg ? msg : "unknown error"; return -1; }

struct Rule { std::string needle; std::string label; std::string replacement; std::string prefilter; bool regex = false; std::shared_ptr<pcre2_code> compiled; int capture_group = 0; bool semantic = false; };

std::shared_ptr<pcre2_code> compile_rule_pattern(const std::string& pattern) {
  // Preserve the accepted ECMAScript pattern dialect while using a heap-backed
  // matcher. libstdc++'s recursive matcher can overflow the native stack on a
  // long, ordinary JSON string before a match (or no-match) is returned.
  std::regex validate(pattern, std::regex_constants::ECMAScript);
  (void)validate;
  std::string translated;
  for (size_t i = 0; i < pattern.size(); ++i) {
    if (pattern[i] != '\\' || i + 1 == pattern.size()) { translated += pattern[i]; continue; }
    const char escaped = pattern[++i];
    // ECMAScript \0 consumes only the zero, even before another digit.
    // A fixed-width hex escape prevents PCRE2 from reading \01 as octal.
    if (escaped == '0') { translated += "\\x00"; continue; }
    if (escaped == 'v') { translated += "\\x0b"; continue; }
    // ECMAScript identity escapes must not acquire PCRE-only semantics.
    if (std::isalpha(static_cast<unsigned char>(escaped)) &&
        std::string("bBdDfnrsStwWcxu").find(escaped) == std::string::npos) {
      translated += escaped;
    } else { translated += '\\'; translated += escaped; }
  }
  int error = 0;
  PCRE2_SIZE offset = 0;
  std::unique_ptr<pcre2_compile_context, decltype(&pcre2_compile_context_free)> context(
      pcre2_compile_context_create(nullptr), pcre2_compile_context_free);
  if (!context) throw std::runtime_error("regex allocation failed");
  pcre2_set_newline(context.get(), PCRE2_NEWLINE_ANYCRLF);
  auto* code = pcre2_compile(reinterpret_cast<PCRE2_SPTR>(translated.data()), translated.size(),
      PCRE2_ALLOW_EMPTY_CLASS | PCRE2_ALT_BSUX | PCRE2_DOLLAR_ENDONLY, &error, &offset, context.get());
  if (!code) throw std::runtime_error("invalid regex pattern");
  return std::shared_ptr<pcre2_code>(code, pcre2_code_free);
}
struct JsonValue {
  enum Type { Null, Boolean, Number, String, Array, Object } type = Null;
  std::string scalar;
  std::vector<JsonValue> array;
  std::vector<std::pair<std::string, JsonValue>> object;
  size_t source_begin = 0;
  size_t source_end = 0;
};
struct MappingResolver {
  uintptr_t handle;
  mapping_resolver_fn resolve;
  mapping_release_fn release;
  MappingResolver(uintptr_t h, mapping_resolver_fn r, mapping_release_fn d) : handle(h), resolve(r), release(d) {}
  ~MappingResolver() { release(handle); }
};
struct RequestTokens {
  std::mutex mu;
  std::unordered_map<std::string, std::string> values_to_tokens;
  std::unordered_map<std::string, std::string> tokens_to_originals;
  // Model-tolerant restoration may accept a changed label when the opaque
  // suffix still identifies exactly one token in this request.
  std::unordered_map<std::string, std::string> unique_suffix_to_token;
  std::unordered_map<std::string, size_t> next_slot;
  std::unordered_set<std::string> input_suffixes;
  std::unordered_set<std::string> labels;
  bool structural_slots = false;
  bool reusable = false;
  std::string reuse_scope;
  std::unique_ptr<MappingResolver> resolver;
  std::unordered_map<std::string, size_t> occurrences;
};
struct PlaceholderMatch {
  size_t end = 0;
  size_t suffix_begin = 0;
  size_t suffix_size = 0;
  std::string label;
  std::string suffix;
  bool braced = false;
};
bool parse_restore_placeholder(const std::string&, size_t, PlaceholderMatch&);
struct Stream {
  std::mutex mu;
  std::string pending;
  std::shared_ptr<RequestTokens> request_tokens;
  bool closed = false;
  bool started = false;
  bool json_escape = false;
  bool json_in_string = false;
  bool json_escape_pending = false;
  bool previous_input_word = false;
};
struct MaskStream {
  std::mutex mu;
  std::string pending;
  std::shared_ptr<RequestTokens> request_tokens;
  bool decided = false;
  bool buffered = false;
  bool closed = false;
};

struct EngineImpl {
  std::vector<Rule> rules;
  std::vector<std::string> field_paths;
  RequestTokens legacy_tokens;
  std::unordered_map<uint64_t, std::shared_ptr<Stream>> streams;
  std::unordered_map<uint64_t, std::shared_ptr<MaskStream>> mask_streams;
  std::shared_mutex token_mu;
  std::mutex streams_mu;
  uint64_t next_stream = 1;
  // The C ABI exposes a raw opaque handle. Keep destruction synchronized with
  // every operation so a graceful shutdown cannot free the implementation
  // while a Go call is still inside the engine.
  std::mutex lifecycle_mu;
  std::condition_variable lifecycle_cv;
  size_t active_calls = 0;
  bool closing = false;
};

struct CallGuard {
  EngineImpl* engine = nullptr;
  bool active = false;

  explicit CallGuard(EngineImpl* value) : engine(value) {
    std::lock_guard<std::mutex> lock(engine->lifecycle_mu);
    if (!engine->closing) {
      ++engine->active_calls;
      active = true;
    }
  }

  CallGuard(const CallGuard&) = delete;
  CallGuard& operator=(const CallGuard&) = delete;

  ~CallGuard() {
    if (!active) return;
    std::lock_guard<std::mutex> lock(engine->lifecycle_mu);
    --engine->active_calls;
    if (engine->closing && engine->active_calls == 0) engine->lifecycle_cv.notify_all();
  }

  explicit operator bool() const { return active; }
};

class JsonParser {
 public:
  explicit JsonParser(const std::string& s): s_(s) {}
  bool parse(JsonValue& v) { ws(); if (!value(v, 0)) return false; ws(); if (p_ != s_.size()) return false; return true; }
 private:
  std::string s_; size_t p_ = 0;
  void ws() { while (p_ < s_.size() && (s_[p_]==' '||s_[p_]=='\n'||s_[p_]=='\r'||s_[p_]=='\t')) ++p_; }
  bool hex4(unsigned& x) { x=0; for(int i=0;i<4;i++){ if(p_>=s_.size()) return false; char c=s_[p_++]; int n=c>='0'&&c<='9'?c-'0':c>='a'&&c<='f'?c-'a'+10:c>='A'&&c<='F'?c-'A'+10:-1; if(n<0)return false; x=(x<<4)|n;} return true; }
  static void utf8(std::string& o, unsigned x) { if(x<0x80)o+=char(x); else if(x<0x800){o+=char(0xc0|(x>>6));o+=char(0x80|(x&63));} else if(x<0x10000){o+=char(0xe0|(x>>12));o+=char(0x80|((x>>6)&63));o+=char(0x80|(x&63));} else {o+=char(0xf0|(x>>18));o+=char(0x80|((x>>12)&63));o+=char(0x80|((x>>6)&63));o+=char(0x80|(x&63));} }
  bool string(std::string& o) { if(p_>=s_.size()||s_[p_++]!='"')return false; while(p_<s_.size()){unsigned char c=s_[p_++]; if(c=='"')return true; if(c<0x20)return false; if(c!='\\'){o+=char(c);continue;} if(p_>=s_.size())return false; char e=s_[p_++]; switch(e){case '"':o+='"';break;case '\\':o+='\\';break;case '/':o+='/';break;case 'b':o+='\b';break;case 'f':o+='\f';break;case 'n':o+='\n';break;case 'r':o+='\r';break;case 't':o+='\t';break;case 'u':{unsigned x;if(!hex4(x))return false;if(x>=0xd800&&x<=0xdbff){if(p_+2>s_.size()||s_[p_]!='\\'||s_[p_+1]!='u')return false;p_+=2;unsigned y;if(!hex4(y)||y<0xdc00||y>0xdfff)return false;x=0x10000+((x-0xd800)<<10)+(y-0xdc00);}else if(x>=0xdc00&&x<=0xdfff)return false;utf8(o,x);break;}default:return false;} } return false; }
  bool value(JsonValue& v,int depth) {
    ws();
    const size_t start = p_;
    if (!value_impl(v, depth)) return false;
    v.source_begin = start;
    v.source_end = p_;
    return true;
  }
  bool value_impl(JsonValue& v,int depth) {
    if (depth > 128) return false;
    if (p_ >= s_.size()) return false;
    char c = s_[p_];
    if (c == '"') { v.type = JsonValue::String; return string(v.scalar); }
    if (c == '{') {
      v.type = JsonValue::Object; ++p_; ws();
      if (p_ < s_.size() && s_[p_] == '}') { ++p_; return true; }
      while (true) {
        ws(); std::string key; if (!string(key)) return false;
        ws(); if (p_ >= s_.size() || s_[p_] != ':') return false; ++p_;
        JsonValue child; if (!value(child, depth + 1)) return false;
        for (const auto& old : v.object) if (old.first == key) return false;
        v.object.emplace_back(std::move(key), std::move(child));
        ws(); if (p_ >= s_.size()) return false;
        if (s_[p_] == '}') { ++p_; return true; }
        if (s_[p_] != ',') return false;
        ++p_;
      }
    }
    if (c == '[') {
      v.type = JsonValue::Array; ++p_; ws();
      if (p_ < s_.size() && s_[p_] == ']') { ++p_; return true; }
      while (true) {
        JsonValue child; if (!value(child, depth + 1)) return false;
        v.array.push_back(std::move(child)); ws();
        if (p_ >= s_.size()) return false;
        if (s_[p_] == ']') { ++p_; return true; }
        if (s_[p_] != ',') return false;
        ++p_;
        ws();
      }
    }
    if(c=='-'||(c>='0'&&c<='9')){size_t b=p_;if(s_[p_]=='-')++p_;if(p_>=s_.size())return false;if(s_[p_]=='0')++p_;else {if(s_[p_]<'1'||s_[p_]>'9')return false;while(p_<s_.size()&&isdigit((unsigned char)s_[p_]))++p_;}if(p_<s_.size()&&s_[p_]=='.'){++p_;size_t d=p_;while(p_<s_.size()&&isdigit((unsigned char)s_[p_]))++p_;if(d==p_)return false;}if(p_<s_.size()&&(s_[p_]=='e'||s_[p_]=='E')){++p_;if(p_<s_.size()&&(s_[p_]=='+'||s_[p_]=='-'))++p_;size_t d=p_;while(p_<s_.size()&&isdigit((unsigned char)s_[p_]))++p_;if(d==p_)return false;}v.type=JsonValue::Number;v.scalar=s_.substr(b,p_-b);return true;}
    for(auto x:{std::pair<const char*,JsonValue::Type>("true",JsonValue::Boolean),{"false",JsonValue::Boolean},{"null",JsonValue::Null}}){size_t n=strlen(x.first);if(s_.compare(p_,n,x.first)==0){p_+=n;v.type=x.second;v.scalar=x.first;return true;}}return false; }
};
void quote_json(const std::string& s,std::string& o){o+='"';for(unsigned char c:s){switch(c){case '"':o+="\\\"";break;case '\\':o+="\\\\";break;case '\b':o+="\\b";break;case '\f':o+="\\f";break;case '\n':o+="\\n";break;case '\r':o+="\\r";break;case '\t':o+="\\t";break;default:if(c<0x20){char b[7];snprintf(b,sizeof(b),"\\u%04x",c);o+=b;}else o+=char(c);}}o+='"';}
void serialize_json(const JsonValue& v,std::string& o){switch(v.type){case JsonValue::Null:o+="null";break;case JsonValue::Boolean:case JsonValue::Number:o+=v.scalar;break;case JsonValue::String:quote_json(v.scalar,o);break;case JsonValue::Array:o+='[';for(size_t i=0;i<v.array.size();++i){if(i)o+=',';serialize_json(v.array[i],o);}o+=']';break;case JsonValue::Object:o+='{';for(size_t i=0;i<v.object.size();++i){if(i)o+=',';quote_json(v.object[i].first,o);o+=':';serialize_json(v.object[i].second,o);}o+='}';}}
std::string make_token(RequestTokens&, const std::string&, const std::string&);

bool durable_mapping(RequestTokens& tokens, bool issue, const std::string& value,
                     const std::string& label, std::string& token, std::string& original) {
  std::string request = "{\"issue\":" + std::string(issue ? "true" : "false") + ",\"scope\":";
  quote_json(tokens.reuse_scope, request);
  request += ",\"value\":"; quote_json(value, request);
  request += ",\"label\":"; quote_json(label, request); request += '}';
  std::unique_ptr<char, decltype(&std::free)> reply(
      tokens.resolver->resolve(tokens.resolver->handle, request.data(), request.size()), &std::free);
  if (!reply) throw std::runtime_error("durable mapping unavailable");
  JsonValue result;
  const std::string raw(reply.get());
  if (!JsonParser(raw).parse(result) || result.type != JsonValue::Object) throw std::runtime_error("invalid durable mapping reply");
  bool ok = false, found = false;
  for (const auto& field : result.object) {
    if (field.first == "ok") ok = field.second.type == JsonValue::Boolean && field.second.scalar == "true";
    else if (field.first == "found") found = field.second.type == JsonValue::Boolean && field.second.scalar == "true";
    else if (field.first == "token" && field.second.type == JsonValue::String) token = field.second.scalar;
    else if (field.first == "original" && field.second.type == JsonValue::String) original = field.second.scalar;
  }
  if (!ok || (issue && !found)) throw std::runtime_error("durable mapping unavailable");
  return found;
}

struct MatchSpan {
  size_t begin = 0;
  size_t end = 0;
  size_t replace_begin = 0;
  size_t replace_end = 0;
};

bool valid_ipv4(const std::string& value, unsigned octets[4]) {
  size_t begin = 0;
  for (size_t i = 0; i < 4; ++i) {
    const size_t end = value.find('.', begin);
    if ((i < 3 && end == std::string::npos) || (i == 3 && end != std::string::npos)) return false;
    const size_t stop = end == std::string::npos ? value.size() : end;
    const size_t length = stop - begin;
    if (length == 0 || length > 3 || (length > 1 && value[begin] == '0')) return false;
    unsigned number = 0;
    for (size_t j = begin; j < stop; ++j) {
      if (value[j] < '0' || value[j] > '9') return false;
      number = number * 10 + static_cast<unsigned>(value[j] - '0');
    }
    if (number > 255) return false;
    octets[i] = number;
    begin = stop + 1;
  }
  return true;
}

bool ipv4_candidate_ok(const std::string& text, size_t begin, size_t end,
                       const std::string& value, const std::string& label) {
  unsigned ip[4]{};
  if (!valid_ipv4(value, ip)) return false;
  if ((begin > 0 && (std::isalnum(static_cast<unsigned char>(text[begin - 1])) || text[begin - 1] == '.')) ||
      (end < text.size() && (std::isalnum(static_cast<unsigned char>(text[end])) || text[end] == '.'))) return false;
  const bool private192 = ip[0] == 192 && ip[1] == 168;
  const bool linkLocal = ip[0] == 169 && ip[1] == 254;
  const bool cgnat = ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127;
  const bool private10 = ip[0] == 10;
  const bool private172 = ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31;
  if (label == "IP_PRIVATE") return private192 || linkLocal || cgnat;
  if (label == "IP_INTERNAL") return private10 || private172;
  if (label != "IP_PUBLIC") return true;
  static const char* dns[] = {"8.8.8.8", "8.8.4.4", "1.1.1.1", "1.0.0.1", "4.2.2.1", "4.2.2.2", "4.2.2.3",
      "114.114.114.114", "114.114.115.115", "223.5.5.5", "223.6.6.6", "119.29.29.29", "180.76.76.76",
      "9.9.9.9", "149.112.112.112", "208.67.222.222", "208.67.220.220"};
  for (const char* known : dns) if (value == known) return false;
  // Avoid common dotted version/build identifiers accepted by the Python engine.
  size_t dots = 0;
  bool all_single_digit = true;
  size_t part_len = 0;
  for (char c : value) {
    if (c == '.') { ++dots; all_single_digit = all_single_digit && part_len == 1; part_len = 0; }
    else ++part_len;
  }
  all_single_digit = all_single_digit && part_len == 1;
  if (dots == 3 && all_single_digit) return false;
  const size_t d1 = value.find('.'), d2 = value.find('.', d1 + 1), d3 = value.find('.', d2 + 1);
  if (d1 == 1 && d2 != std::string::npos && d3 != std::string::npos && ip[2] == 0 && value.size() - d3 - 1 == 3) return false;
  const bool reserved = ip[0] == 0 || private10 || ip[0] == 127 || private192 || linkLocal || cgnat ||
      private172 || ip[0] >= 224 || (ip[0] == 100 && ip[1] < 64) ||
      (ip[0] == 192 && (ip[1] == 0 || ip[1] == 168)) ||
      (ip[0] == 192 && ip[1] == 88 && ip[2] == 99) ||
      (ip[0] == 198 && (ip[1] == 18 || ip[1] == 19 || ip[1] == 51)) ||
      (ip[0] == 203 && ip[1] == 0) || ip[0] == 240;
  return !reserved;
}

bool luhn_valid(const std::string& value) {
  int sum = 0;
  bool double_digit = false;
  size_t digits = 0;
  for (auto it = value.rbegin(); it != value.rend(); ++it) {
    if (*it == ' ' || *it == '-') continue;
    if (*it < '0' || *it > '9') return false;
    int digit = *it - '0';
    if (double_digit) { digit *= 2; if (digit > 9) digit -= 9; }
    sum += digit;
    double_digit = !double_digit;
    ++digits;
  }
  if (digits < 13 || digits > 19 || value.empty() || value.front() == '0') return false;
  // Grouped cards must use one separator consistently and plausible groups;
  // this prevents joining unrelated prose numbers across arbitrary spaces.
  const bool grouped = value.find(' ') != std::string::npos || value.find('-') != std::string::npos;
  if (!grouped) return sum % 10 == 0;
  const char separator = value.find(' ') != std::string::npos ? ' ' : '-';
  if (value.find(' ') != std::string::npos && value.find('-') != std::string::npos) return false;
  size_t begin = 0;
  size_t group = 0;
  while (begin <= value.size()) {
    const size_t end = value.find(separator, begin);
    const size_t stop = end == std::string::npos ? value.size() : end;
    const size_t length = stop - begin;
    if (length == 0 || length > 6 || (group == 0 && (length < 3 || length > 6))) return false;
    ++group;
    if (end == std::string::npos) break;
    begin = end + 1;
  }
  return group >= 3 && group <= 5 && sum % 10 == 0;
}

bool leap_year(int year) { return year % 4 == 0 && (year % 100 != 0 || year % 400 == 0); }

bool valid_idcard(const std::string& value) {
  static const int weights[17] = {7,9,10,5,8,4,2,1,6,3,7,9,10,5,8,4,2};
  static const char checks[] = "10X98765432";
  static const int month_days[12] = {31,28,31,30,31,30,31,31,30,31,30,31};
  if (value.size() != 15 && value.size() != 18) return false;
  const std::string province = value.substr(0, 2);
  static const char* provinces[] = {"11","12","13","14","15","21","22","23","31","32","33","34","35","36","37","41","42","43","44","45","46","50","51","52","53","54","61","62","63","64","65","71","81","82"};
  if (std::find(std::begin(provinces), std::end(provinces), province) == std::end(provinces)) return false;
  for (size_t i = 0; i < value.size(); ++i) if (!(value[i] >= '0' && value[i] <= '9') && !(i == 17 && (value[i] == 'X' || value[i] == 'x'))) return false;
  const int year = value.size() == 18 ? std::stoi(value.substr(6, 4)) : 1900 + std::stoi(value.substr(6, 2));
  const int month = std::stoi(value.substr(value.size() == 18 ? 10 : 8, 2));
  const int day = std::stoi(value.substr(value.size() == 18 ? 12 : 10, 2));
  const int current_year = [] {
    const std::time_t now = std::time(nullptr);
    const std::tm* parts = std::gmtime(&now);
    return parts ? parts->tm_year + 1900 : 9999;
  }();
  if (year < (value.size() == 18 ? 1880 : 1900) || year > (value.size() == 18 ? current_year : 1999) || month < 1 || month > 12) return false;
  const int max_day = month_days[month - 1] + (month == 2 && leap_year(year) ? 1 : 0);
  if (day < 1 || day > max_day) return false;
  if (value.size() == 15) return true;
  int checksum = 0;
  for (size_t i = 0; i < 17; ++i) checksum += (value[i] - '0') * weights[i];
  return static_cast<char>(std::toupper(static_cast<unsigned char>(value[17]))) == checks[checksum % 11];
}

bool valid_phone(const std::string& value) {
  std::string normalized = value;
  for (const std::string& punctuation : {std::string("（"), std::string("）")}) {
    size_t position = 0;
    while ((position = normalized.find(punctuation, position)) != std::string::npos) normalized.replace(position, punctuation.size(), 1, punctuation == "（" ? '(' : ')');
  }
  std::string digits;
  char group_separator = 0;
  for (char c : normalized) {
    if (c >= '0' && c <= '9') digits += c;
    else if (c == ' ' || c == '-') {
      if (group_separator != 0 && group_separator != c) return false;
      group_separator = c;
    } else if (c != '+' && c != '(' && c != ')') return false;
  }
  if (digits.rfind("0086", 0) == 0) digits.erase(0, 4);
  else if (digits.rfind("86", 0) == 0 && digits.size() == 13) digits.erase(0, 2);
  if (digits.size() != 11 || digits[0] != '1' || digits[1] < '3' || digits[1] > '9') return false;
  return std::any_of(digits.begin() + 1, digits.end(), [&](char c) { return c != digits[0]; });
}

bool valid_email(const std::string& value) {
  const size_t marker = value.find('@');
  if (marker == std::string::npos || marker == 0 || marker + 1 >= value.size() ||
      value.find('@', marker + 1) != std::string::npos) return false;
  const std::string local = value.substr(0, marker);
  const std::string domain = value.substr(marker + 1);
  if (local.size() > 64 || domain.size() < 3 || local.front() == '.' || local.back() == '.' ||
      local.find("..") != std::string::npos || domain.find("..") != std::string::npos) return false;
  const size_t dot = domain.rfind('.');
  if (dot == std::string::npos || domain.size() - dot - 1 < 2) return false;
  size_t begin = 0;
  while (begin < domain.size()) {
    const size_t separator = domain.find('.', begin);
    const size_t end = separator == std::string::npos ? domain.size() : separator;
    if (end == begin || end - begin > 63 || domain[begin] == '-' || domain[end - 1] == '-') return false;
    begin = end + 1;
  }
  return true;
}

bool valid_iban(const std::string& value) {
  if (value.size() < 15 || value.size() > 34 || value.size() < 4 ||
      !std::isalpha(static_cast<unsigned char>(value[0])) ||
      !std::isalpha(static_cast<unsigned char>(value[1])) ||
      !std::isdigit(static_cast<unsigned char>(value[2])) ||
      !std::isdigit(static_cast<unsigned char>(value[3])) ||
      value.find_first_not_of("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != std::string::npos) return false;
  // Calculate mod 97 incrementally; the expanded IBAN can exceed machine integer width.
  unsigned remainder = 0;
  auto consume = [&](char c) {
    if (c >= '0' && c <= '9') {
      remainder = (remainder * 10 + static_cast<unsigned>(c - '0')) % 97;
    } else {
      const unsigned number = static_cast<unsigned>(c - 'A' + 10);
      remainder = (remainder * 10 + number / 10) % 97;
      remainder = (remainder * 10 + number % 10) % 97;
    }
  };
  for (size_t i = 4; i < value.size(); ++i) consume(value[i]);
  for (size_t i = 0; i < 4; ++i) consume(value[i]);
  return remainder == 1;
}

bool valid_jwt(const std::string& value) {
  const size_t dot = value.find('.');
  const size_t second_dot = dot == std::string::npos ? std::string::npos : value.find('.', dot + 1);
  if (dot == std::string::npos || dot == 0 || second_dot == std::string::npos || second_dot == dot + 1 || second_dot + 1 >= value.size()) return false;
  const std::string alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const std::string header = value.substr(0, dot);
  std::string decoded;
  decoded.reserve(header.size() * 3 / 4);
  unsigned accumulator = 0;
  int bits = 0;
  for (char c : header) {
    const size_t digit = alphabet.find(c);
    if (digit == std::string::npos) return false;
    accumulator = (accumulator << 6) | static_cast<unsigned>(digit);
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      decoded.push_back(static_cast<char>((accumulator >> bits) & 0xff));
    }
  }
  JsonParser parser(decoded);
  JsonValue header_value;
  if (!parser.parse(header_value) || header_value.type != JsonValue::Object) return false;
  for (const auto& field : header_value.object) {
    if (field.first == "alg" && field.second.type == JsonValue::String && !field.second.scalar.empty()) return true;
  }
  return false;
}

bool valid_ipv6_private(const std::string& value) {
  const size_t zone = value.find('%');
  const std::string address = value.substr(0, zone);
  struct in6_addr parsed {};
  #if defined(_WIN32)
  if (InetPtonA(AF_INET6, address.c_str(), &parsed) != 1) return false;
  #else
  if (inet_pton(AF_INET6, address.c_str(), &parsed) != 1) return false;
  #endif
  const bool link_local = parsed.s6_addr[0] == 0xfe && (parsed.s6_addr[1] & 0xc0) == 0x80;
  const bool unique_local = (parsed.s6_addr[0] & 0xfe) == 0xfc;
  return link_local || unique_local;
}

bool valid_uscc(const std::string& value) {
  static constexpr char alphabet[] = "0123456789ABCDEFGHJKLMNPQRTUWXY";
  static constexpr int weights[] = {1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28};
  if (value.size() != 18) return false;
  int total = 0;
  for (size_t i = 0; i < 17; ++i) {
    const char* found = std::strchr(alphabet, value[i]);
    if (!found) return false;
    total += weights[i] * static_cast<int>(found - alphabet);
  }
  const char* check = std::strchr(alphabet, value[17]);
  return check && value[17] == alphabet[(31 - total % 31) % 31];
}

bool valid_plate(const std::string& value) {
  static const std::vector<std::string> provinces = {
      "京", "津", "沪", "渝", "冀", "豫", "云", "辽", "黑", "湘", "皖", "鲁", "新", "苏", "浙",
      "赣", "鄂", "桂", "甘", "晋", "蒙", "陕", "吉", "闽", "贵", "粤", "青", "藏", "川", "宁", "琼", "使", "领"};
  if (value.size() < 9 || value.size() > 10) return false;
  bool province_found = false;
  for (const std::string& province : provinces) {
    if (value.compare(0, province.size(), province) == 0) { province_found = true; break; }
  }
  if (!province_found || value[3] < 'A' || value[3] > 'Z') return false;
  const std::string body = value.substr(4);
  if (body.size() < 5 || body.size() > 6) return false;
  bool has_digit = false;
  for (char c : body) {
    if (c >= '0' && c <= '9') has_digit = true;
    else if (c < 'A' || c > 'Z') return false;
  }
  return has_digit;
}

bool valid_landline(const std::string& value) {
  std::string digits;
  for (char c : value) if (c >= '0' && c <= '9') digits += c;
  if (digits.rfind("0086", 0) == 0) digits.erase(0, 4);
  else if (digits.rfind("86", 0) == 0) digits.erase(0, 2);
  if (digits.size() < 10 || digits.size() > 17 || digits[0] != '0') return false;
  size_t local = 0;
  if (digits.size() >= 4 && (digits[1] == '1' || digits[1] == '2')) local = 3;
  else if (digits.size() >= 5 && digits[1] >= '3' && digits[1] <= '9') local = 4;
  else return false;
  return local < digits.size() && digits[local] >= '2' && digits[local] <= '9';
}

bool semantic_match_valid(const std::string& text, size_t begin, size_t end,
                          size_t value_begin, size_t value_end, const std::string& value,
                          const std::string& label) {
  if (label == "IP_PRIVATE" || label == "IP_INTERNAL" || label == "IP_PUBLIC")
    return ipv4_candidate_ok(text, begin, end, value, label);
  if (label == "CARD") {
    if ((begin > 0 && std::isalnum(static_cast<unsigned char>(text[begin - 1]))) ||
        (end < text.size() && std::isalnum(static_cast<unsigned char>(text[end]))) ||
        (value_begin > 0 && text[value_begin - 1] == '.') ||
        (value_end < text.size() && text[value_end] == '.')) return false;
    return luhn_valid(value);
  }
  if (label == "IDCARD") {
    if ((begin > 0 && std::isalnum(static_cast<unsigned char>(text[begin - 1]))) ||
        (end < text.size() && std::isalnum(static_cast<unsigned char>(text[end])))) return false;
    return valid_idcard(value);
  }
  if (label == "PHONE") {
    if ((begin > 0 && std::isalnum(static_cast<unsigned char>(text[begin - 1]))) ||
        (end < text.size() && std::isalnum(static_cast<unsigned char>(text[end])))) return false;
    return valid_phone(value);
  }
  if (label == "EMAIL") {
    if (begin > 0) {
      const unsigned char previous = static_cast<unsigned char>(text[begin - 1]);
      if (std::isalnum(previous) || previous >= 0x80 || previous == '.' || previous == '_' ||
          previous == '%' || previous == '+' || previous == '-' || previous == ':') return false;
    }
    const size_t scheme = text.rfind("://", begin);
    if (scheme != std::string::npos) {
      const size_t authority_end = text.find_first_of("/?#", scheme + 3);
      const size_t authority_limit = authority_end == std::string::npos ? text.size() : authority_end;
      if (begin < authority_limit && text.find(':', scheme + 3) < begin) return false;
    }
    return valid_email(value);
  }
  if (label == "IBAN") return valid_iban(value);
  if (label == "JWT") return valid_jwt(value);
  if (label == "IPV6_PRIVATE") return valid_ipv6_private(value);
  if (label == "USCC") return valid_uscc(value);
  if (label == "PLATE") {
    if ((begin > 0 && std::isalnum(static_cast<unsigned char>(text[begin - 1]))) ||
        (end < text.size() && std::isalnum(static_cast<unsigned char>(text[end]))) ||
        (value_begin > 0 && text[value_begin - 1] == '.') ||
        (value_end < text.size() && text[value_end] == '.')) return false;
    return valid_plate(value);
  }
  if (label == "LANDLINE") return valid_landline(value);
  if (label == "SECRET") {
    bool has_digit_or_symbol = false;
    for (unsigned char c : value) {
      if (std::isdigit(c) || std::strchr("!@#$%^&*", c) != nullptr) {
        has_digit_or_symbol = true;
        break;
      }
    }
    return has_digit_or_symbol && value.size() >= 6;
  }
  return true;
}

struct JsonReplacement {
  size_t begin = 0;
  size_t end = 0;
  std::string value;
};

void collect_json_replacements(const JsonValue& v, const std::vector<std::string>& parts,
                              size_t i, EngineImpl& e, RequestTokens& tokens,
                              std::vector<JsonReplacement>& replacements) {
  if (i == parts.size()) {
    std::string raw = v.type == JsonValue::String ? v.scalar : std::string();
    if (raw.empty() && v.type != JsonValue::String) serialize_json(v, raw);
    std::string token = make_token(tokens, raw, "JSON");
    std::string encoded;
    quote_json(token, encoded);
    replacements.push_back(JsonReplacement{v.source_begin, v.source_end, std::move(encoded)});
    return;
  }
  if (v.type == JsonValue::Object) {
    for (const auto& kv : v.object) {
      if (kv.first == parts[i]) collect_json_replacements(kv.second, parts, i + 1, e, tokens, replacements);
    }
    return;
  }
  if (v.type != JsonValue::Array) return;
  if (parts[i] == "*") {
    for (const auto& item : v.array) collect_json_replacements(item, parts, i + 1, e, tokens, replacements);
    return;
  }
  size_t index = 0;
  if (parts[i].empty()) return;
  for (char c : parts[i]) {
    if (c < '0' || c > '9') return;
    const size_t digit = static_cast<size_t>(c - '0');
    if (index > (static_cast<size_t>(-1) - digit) / 10) return;
    index = index * 10 + digit;
  }
  if (index < v.array.size()) collect_json_replacements(v.array[index], parts, i + 1, e, tokens, replacements);
}

void add_json_replacement(std::vector<JsonReplacement>& replacements, JsonReplacement replacement) {
  if (replacement.end < replacement.begin) return;
  for (auto it = replacements.begin(); it != replacements.end();) {
    if (replacement.begin >= it->begin && replacement.end <= it->end) return;
    if (it->begin >= replacement.begin && it->end <= replacement.end) {
      it = replacements.erase(it);
      continue;
    }
    ++it;
  }
  replacements.push_back(std::move(replacement));
}

void apply_json_replacements(const std::string& input, std::vector<JsonReplacement>& replacements,
                            std::string& output, std::vector<size_t>* origins) {
  std::sort(replacements.begin(), replacements.end(), [](const JsonReplacement& a, const JsonReplacement& b) {
    if (a.begin != b.begin) return a.begin < b.begin;
    return a.end < b.end;
  });
  output.clear();
  if (origins) origins->clear();
  size_t cursor = 0;
  for (const JsonReplacement& replacement : replacements) {
    if (replacement.begin < cursor || replacement.end > input.size()) continue;
    const std::string unchanged = input.substr(cursor, replacement.begin - cursor);
    output += unchanged;
    if (origins) {
      for (size_t i = 0; i < unchanged.size(); ++i) origins->push_back(cursor + i);
      for (size_t i = 0; i < replacement.value.size(); ++i) origins->push_back(replacement.begin);
    }
    output += replacement.value;
    cursor = replacement.end;
  }
  if (origins) {
    const std::string unchanged = input.substr(cursor);
    output += unchanged;
    for (size_t i = 0; i < unchanged.size(); ++i) origins->push_back(cursor + i);
  } else {
    output.append(input, cursor, std::string::npos);
  }
}

// Deliberately small parser: accepts {"rules":[{"needle":"...","label":"..."}, ...]}.
// A pattern field enables ECMAScript regular expressions.
constexpr size_t kMaxCustomRegexPatternLength = 4096;
// Built-in email/Bearer matching needs a small look-behind buffer so a token
// split across input chunks is never emitted in plaintext. Keep that buffer
// bounded as well: a pathological continuous candidate must fail closed
// instead of retaining the whole request in a streaming session.
constexpr size_t kMaxBuiltinStreamCandidateBytes = 1 << 20;

// Reject patterns with common catastrophic-backtracking shapes before handing
// them to std::regex. This is deliberately conservative and only applies to
// user-provided patterns; built-in patterns are trusted and remain unchanged.
bool safe_custom_regex_pattern(const std::string& pattern) {
  if (pattern.size() > kMaxCustomRegexPatternLength) return false;
  std::vector<bool> group_quantifier;
  bool in_class = false;
  bool escaped = false;
  bool previous_quantifier = false;
  for (size_t i = 0; i < pattern.size(); ++i) {
    const char c = pattern[i];
    if (escaped) { escaped = false; previous_quantifier = false; continue; }
    if (c == '\\') { escaped = true; continue; }
    if (in_class) {
      if (c == ']') in_class = false;
      continue;
    }
    if (c == '[') { in_class = true; previous_quantifier = false; continue; }
    if (c == '(') { group_quantifier.push_back(false); previous_quantifier = false; continue; }
    if (c == ')') {
      const bool had_quantifier = !group_quantifier.empty() && group_quantifier.back();
      if (!group_quantifier.empty()) group_quantifier.pop_back();
      if (i + 1 < pattern.size() && (pattern[i + 1] == '*' || pattern[i + 1] == '+' || pattern[i + 1] == '?')) {
        if (had_quantifier) return false;
      } else if (i + 1 < pattern.size() && pattern[i + 1] == '{') {
        if (had_quantifier) return false;
      }
      previous_quantifier = false;
      continue;
    }
    const bool quantifier = c == '*' || c == '+' || c == '?' || c == '{';
    if (quantifier) {
      if (previous_quantifier) return false;
      if (!group_quantifier.empty()) group_quantifier.back() = true;
      previous_quantifier = true;
      continue;
    }
    if (c == '}') { previous_quantifier = true; continue; }
    previous_quantifier = false;
  }
  return !in_class && !escaped;
}

bool has_digit_sequence(const std::string& text, size_t minimum_digits, const char* starters) {
  for (size_t i = 0; i < text.size(); ++i) {
    if (std::strchr(starters, static_cast<unsigned char>(text[i])) == nullptr) continue;
    size_t digits = 0;
    for (size_t j = i; j < text.size(); ++j) {
      const unsigned char c = static_cast<unsigned char>(text[j]);
      if (std::isdigit(c)) {
        ++digits;
        if (digits >= minimum_digits) return true;
        continue;
      }
      if (c == ' ' || c == '-') continue;
      break;
    }
  }
  return false;
}

// These checks are necessary conditions for the built-in patterns used by
// the control plane. They only skip a regex pass when a match is impossible;
// the original regex still decides every candidate that survives the check.
bool passes_builtin_prefilter(const Rule& rule, const std::string& text) {
  if (!rule.regex) return true;
  if (!rule.prefilter.empty() && text.find(rule.prefilter) == std::string::npos) return false;
  const std::string& pattern = rule.needle;
  if (pattern == R"((ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{20,})") return text.find("gh") != std::string::npos;
  if (pattern == R"(github_pat_[A-Za-z0-9_]{50,})") return text.find("github_pat_") != std::string::npos;
  if (pattern == R"(AIza[0-9A-Za-z_-]{35,38})") return text.find("AIza") != std::string::npos;
  if (pattern == R"(xox[baprs]-[0-9A-Za-z-]{10,})") return text.find("xox") != std::string::npos;
  if (pattern == R"([sr]k_(live|test)_[0-9A-Za-z]{20,})") return text.find("k_") != std::string::npos;
  if (pattern == R"(cli_[a-z0-9]{16,})") return text.find("cli_") != std::string::npos;
  if (pattern == R"(ding[a-z0-9]{6,})") return text.find("ding") != std::string::npos;
  if (pattern == R"([3-6][0-9]{12,18})") return has_digit_sequence(text, 13, "3456");
  if (pattern == R"([a-z][a-z0-9+.-]{0,63}://[^\s:@/]+:[^\s@/]{4,}@)") return text.find("://") != std::string::npos;
  if (pattern == R"(((1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{13}|(1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{15}[0-9Xx]))") {
    return has_digit_sequence(text, 14, "12345678");
  }
  if (pattern == R"(0(10|2[0-9]|[3-9][0-9]{2})[- ]?[2-9][0-9]{6,7})") return has_digit_sequence(text, 10, "0");
  if (pattern == R"(1[3-9][0-9]{9})") return has_digit_sequence(text, 11, "1");
  if (pattern == R"(\+861[3-9][0-9]{9})") return text.find("+861") != std::string::npos;
  if (pattern == R"(00861[3-9][0-9]{9})") return text.find("00861") != std::string::npos;
  if (pattern == R"(1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4})") return has_digit_sequence(text, 11, "1");
  if (pattern == R"((\+86|0086)[ -]?1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4})") {
    return text.find('+') != std::string::npos || text.find("0086") != std::string::npos;
  }
  return true;
}

void add_default_rules(EngineImpl& e) {
  Rule email{R"(([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}))", "EMAIL", "", "@", true, {}};
  Rule bearer{R"(Bearer\s+[A-Za-z0-9._~+/-]{12,}=*)", "BEARER", "", "Bearer", true, {}};
  email.compiled = compile_rule_pattern(email.needle); bearer.compiled = compile_rule_pattern(bearer.needle);
  e.rules.push_back(std::move(email)); e.rules.push_back(std::move(bearer));
}

bool parse_config(EngineImpl& e, const char* json, size_t len) {
  const std::string source = (json && len) ? std::string(json, len) : std::string();
  if (source.empty()) {
    add_default_rules(e);
    return true;
  }
  JsonValue root;
  JsonParser parser(source);
  if (!parser.parse(root) || root.type != JsonValue::Object) return false;
  const JsonValue* rules = nullptr;
  const JsonValue* paths = nullptr;
  for (const auto& kv : root.object) {
    if (kv.first == "rules") rules = &kv.second;
    else if (kv.first == "field_paths" || kv.first == "json_fields") paths = &kv.second;
  }
  if (rules == nullptr && paths == nullptr) {
    // The control plane stores built-in toggles as {"rule_name": true}.
    // Treat enabled names as literal rules so a toggle update cannot make the
    // native engine fail to reload.
    for (const auto& kv : root.object) {
      if (kv.second.type == JsonValue::Boolean && kv.second.scalar == "true") {
        e.rules.push_back(Rule{kv.first, "TERM", "", "", false, {}});
      }
    }
    if (e.rules.empty()) add_default_rules(e);
    return true;
  }
  if (rules != nullptr) {
    if (rules->type != JsonValue::Array) return false;
    for (const auto& item : rules->array) {
      if (item.type == JsonValue::String) {
        if (item.scalar.empty()) return false;
        e.rules.push_back(Rule{item.scalar, "TERM", "", "", false, {}});
        continue;
      }
      if (item.type != JsonValue::Object) return false;
      const JsonValue* needle = nullptr;
      const JsonValue* pattern = nullptr;
      const JsonValue* label = nullptr;
      const JsonValue* capture_group = nullptr;
      const JsonValue* semantic = nullptr;
      for (const auto& kv : item.object) {
        if (kv.first == "needle") needle = &kv.second;
        else if (kv.first == "pattern") pattern = &kv.second;
        else if (kv.first == "label") label = &kv.second;
        else if (kv.first == "capture_group") capture_group = &kv.second;
        else if (kv.first == "semantic") semantic = &kv.second;
      }
      const bool is_regex = pattern != nullptr;
      const JsonValue* value = is_regex ? pattern : needle;
      if (value == nullptr || value->type != JsonValue::String || value->scalar.empty()) return false;
      std::string rule_label = "TERM";
      if (label != nullptr) {
        if (label->type != JsonValue::String || label->scalar.empty()) return false;
        rule_label = label->scalar;
      }
      Rule rule{value->scalar, rule_label, "", "", is_regex, {}};
      if (capture_group != nullptr) {
        if (capture_group->type != JsonValue::Number) return false;
        try { rule.capture_group = std::stoi(capture_group->scalar); }
        catch (...) { return false; }
        if (rule.capture_group < 0 || rule.capture_group > 16) return false;
      }
      if (semantic != nullptr) {
        if (semantic->type != JsonValue::Boolean) return false;
        rule.semantic = semantic->scalar == "true";
      }
      if (is_regex) {
        if (!safe_custom_regex_pattern(rule.needle)) return false;
        try { rule.compiled = compile_rule_pattern(rule.needle); }
        catch (...) { return false; }
      }
      e.rules.push_back(std::move(rule));
    }
  }
  if (paths != nullptr) {
    if (paths->type != JsonValue::Array) return false;
    for (const auto& item : paths->array) {
      if (item.type != JsonValue::String || item.scalar.empty()) return false;
      e.field_paths.push_back(item.scalar);
    }
    // Field-path redaction is additive: a JSON-aware configuration still
    // keeps the built-in rules unless the caller supplied an explicit rules
    // array (which may intentionally be empty).
    if (rules == nullptr) add_default_rules(e);
  }
  return true;
}

std::vector<std::string> split_path(const std::string& path) {
  const bool pointer = !path.empty() && path.front() == '/';
  std::string normalized = pointer ? path.substr(1) : path;
  if (!pointer && !normalized.empty() && normalized.front() == '$') {
    normalized.erase(0, 1);
    if (!normalized.empty() && normalized.front() == '.') normalized.erase(0, 1);
  }
  std::vector<std::string> parts;
  size_t begin = 0;
  while (begin <= normalized.size()) {
    const size_t end = normalized.find(pointer ? '/' : '.', begin);
    std::string part = normalized.substr(begin, end == std::string::npos ? std::string::npos : end - begin);
    if (!pointer && part.empty()) return {};
    if (pointer) {
      std::string decoded;
      for (size_t i = 0; i < part.size(); ++i) {
        if (part[i] == '~') {
          if (++i == part.size() || (part[i] != '0' && part[i] != '1')) return {};
          decoded += part[i] == '0' ? '~' : '/';
        } else decoded += part[i];
      }
      part = std::move(decoded);
    }
    parts.push_back(std::move(part));
    if (end == std::string::npos) break;
    begin = end + 1;
  }
  return parts;
}

std::string normalize_label(const std::string& label) {
  std::string safe;
  for (char c : label) if (std::isalnum(static_cast<unsigned char>(c)) || c == '_') safe += static_cast<char>(std::toupper(static_cast<unsigned char>(c)));
  if (safe.empty()) safe = "TERM";
  return safe;
}

std::string hex64(uint64_t value) {
  static const char digits[] = "0123456789abcdef";
  std::string result(16, '0');
  for (int i = 15; i >= 0; --i) { result[static_cast<size_t>(i)] = digits[value & 0xf]; value >>= 4; }
  return result;
}

// Request-scoped placeholders use a stable per-label occurrence slot instead
// of hashing the sensitive value. Every match receives the next slot, even if
// its value repeats, so changing duplicate relationships cannot change prompt
// bytes. The original value lives only in this request's map.
std::string make_token(RequestTokens& tokens, const std::string& original, const std::string& label) {
  const std::string safe = normalize_label(label);
  const std::string value_key = std::to_string(original.size()) + ":" + original + ":" + safe;
  if (!tokens.structural_slots || tokens.reusable) {
    auto found = tokens.values_to_tokens.find(value_key);
    if (found != tokens.values_to_tokens.end()) {
      ++tokens.occurrences[found->second];
      return found->second;
    }
  }
  if (tokens.tokens_to_originals.size() >= 100000) throw std::runtime_error("token mapping limit exceeded");
  std::string token;
  if (tokens.reusable) {
    if (tokens.resolver) {
      std::string persisted;
      durable_mapping(tokens, true, original, safe.substr(0, 12), token, persisted);
      if (persisted != original) throw std::runtime_error("durable mapping identity conflict");
    } else token = reusable_tokens.issue(tokens.reuse_scope, original, safe.substr(0, 12), tokens.input_suffixes);
  } else {
  std::string key = std::to_string(original.size()) + ":" + original + safe;
  uint64_t hash;
  do {
    if (tokens.structural_slots) {
      const size_t ordinal = tokens.next_slot[safe]++;
      key = safe + ":" + std::to_string(ordinal);
    }
    hash = 1469598103934665603ULL;
    for (unsigned char c : key) { hash ^= c; hash *= 1099511628211ULL; }
    if (tokens.structural_slots) {
      // Avalanche the slot hash: adjacent FNV ordinals otherwise look like a
      // sequence that a model may try to continue instead of copying exactly.
      // Identity still depends only on label and slot, preserving prompt bytes.
      hash ^= hash >> 30; hash *= 0xbf58476d1ce4e5b9ULL;
      hash ^= hash >> 27; hash *= 0x94d049bb133111ebULL;
      hash ^= hash >> 31;
    }
  } while (tokens.structural_slots && tokens.input_suffixes.count(hex64(hash)));
  token = "{{" + safe.substr(0, 12) + "_" + hex64(hash) + "}}";
  }
  auto taken = tokens.tokens_to_originals.find(token);
  if (taken != tokens.tokens_to_originals.end() && taken->second != original) {
    throw std::runtime_error("request token slot collision");
  }
  if (!tokens.structural_slots || tokens.reusable) tokens.values_to_tokens[value_key] = token;
  ++tokens.occurrences[token];
  tokens.tokens_to_originals[token] = original;
  tokens.labels.insert(safe.substr(0, 12));
  const size_t suffix_start = token.rfind('_');
  if (suffix_start != std::string::npos) {
    const std::string suffix = token.substr(suffix_start + 1, token.size() - suffix_start - 3);
    auto [suffix_it, inserted] = tokens.unique_suffix_to_token.emplace(suffix, token);
    if (!inserted && suffix_it->second != token) suffix_it->second.clear();
  }
  return token;
}

bool email_atom(unsigned char c) {
  return std::isalnum(c) || c >= 0x80 || c == '.' || c == '_' || c == '%' || c == '+' || c == '-';
}
bool domain_atom(unsigned char c) {
  return std::isalnum(c) || c >= 0x80 || c == '.' || c == '-';
}
bool tld_atom(unsigned char c) { return std::isalpha(c) != 0 || c >= 0x80; }
bool email_candidate_ok(const std::string& text, size_t begin, size_t end, size_t marker) {
  if (begin >= marker || marker + 1 >= end || (begin > 0 && text[begin - 1] == ':')) return false;
  const std::string local = text.substr(begin, marker - begin);
  const std::string domain = text.substr(marker + 1, end - marker - 1);
  if (local.empty() || domain.size() < 3 || local.front() == '.' || local.back() == '.' ||
      local.find("..") != std::string::npos || domain.find("..") != std::string::npos) return false;
  const size_t dot = domain.rfind('.');
  if (dot == std::string::npos || domain.size() - dot - 1 < 2) return false;
  size_t label_begin = 0;
  while (label_begin < domain.size()) {
    const size_t label_end = domain.find('.', label_begin);
    const size_t stop = label_end == std::string::npos ? domain.size() : label_end;
    if (stop == label_begin || domain[label_begin] == '-' || domain[stop - 1] == '-') return false;
    label_begin = stop + 1;
  }
  return true;
}
bool regex_space(unsigned char c) {
  return c == ' ' || c == '\f' || c == '\n' || c == '\r' || c == '\t' || c == '\v';
}
bool bearer_word_at(const std::string& text, size_t offset) {
  static const char word[] = "bearer";
  if (offset + sizeof(word) - 1 > text.size()) return false;
  for (size_t i = 0; i < sizeof(word) - 1; ++i) {
    if (std::tolower(static_cast<unsigned char>(text[offset + i])) != word[i]) return false;
  }
  return true;
}

int builtin_email_kind(const Rule& rule) {
  if (!rule.regex) return 0;
  if (rule.needle == R"(([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}))") return 1;
  if (rule.needle == R"(([A-Za-z0-9_\x80-\xFF][-A-Za-z0-9._%+\x80-\xFF]{0,63}@[-A-Za-z0-9.\x80-\xFF]+\.[A-Za-z\x80-\xFF]{2,}))") return 2;
  if (rule.needle == R"(([^\s@]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}))" && rule.semantic) return 3;
  return 0;
}

std::vector<std::pair<size_t, size_t>> builtin_matches(const Rule& rule, const std::string& text) {
  std::vector<std::pair<size_t, size_t>> matches;
  // Match the complete built-in pattern rather than the label. A user may
  // define a custom rule with the same label and it must still use its regex.
  const int email = builtin_email_kind(rule);
  const bool legacy_bearer = rule.regex && rule.needle == R"(Bearer\s+[A-Za-z0-9._~+/-]{12,}=*)";
  const bool bearer = legacy_bearer || (rule.regex && rule.needle == R"([Bb][Ee][Aa][Rr][Ee][Rr]\s+[A-Za-z0-9._~+/-]{20,}=*)");
  if (!email && !bearer) return matches;
  if (!rule.prefilter.empty() && text.find(rule.prefilter) == std::string::npos) return matches;
  if (email) {
    size_t marker = 0;
    size_t search_begin = 0;
    while ((marker = text.find('@', marker)) != std::string::npos) {
      size_t begin = marker;
      while (begin > search_begin) {
        const unsigned char c = static_cast<unsigned char>(text[begin - 1]);
        const bool atom = email == 3 ? !regex_space(c) && c != '@' :
                          email_atom(c) && (email == 2 || c < 0x80);
        if (!atom) break;
        --begin;
      }
      if (email == 2) {
        // This expression permits at most 64 local bytes, with a narrower
        // first character. Preserve its exact spans when using the fast path.
        if (marker - begin > 64) begin = marker - 64;
        while (begin < marker) {
          const unsigned char c = static_cast<unsigned char>(text[begin]);
          if (std::isalnum(c) || c == '_' || c >= 0x80) break;
          ++begin;
        }
      }
      size_t domain_end = marker + 1;
      while (domain_end < text.size() && domain_atom(static_cast<unsigned char>(text[domain_end])) && (email == 2 || static_cast<unsigned char>(text[domain_end]) < 0x80)) ++domain_end;
      if (begin < marker) {
        // The domain atom is greedy, then the final dot + alpha suffix is
        // backtracked to the rightmost valid dot, matching the ECMAScript
        // pattern even for multi-label domains or trailing punctuation.
        bool found = false;
        size_t dot = domain_end;
        while (dot > marker + 1) {
          dot = text.rfind('.', dot - 1);
          if (dot == std::string::npos || dot <= marker + 1) break;
          size_t tld_end = dot + 1;
          while (tld_end < domain_end && tld_atom(static_cast<unsigned char>(text[tld_end]))) ++tld_end;
          if (tld_end - dot - 1 >= 2 && (!rule.semantic || email_candidate_ok(text, begin, tld_end, marker))) {
            matches.emplace_back(begin, tld_end);
            marker = tld_end;
            search_begin = tld_end;
            found = true;
            break;
          }
        }
        if (found) continue;
      }
      ++marker;
    }
  } else {
    size_t cursor = 0;
    while (cursor + 6 <= text.size()) {
      if (!bearer_word_at(text, cursor)) {
        ++cursor;
        continue;
      }
      size_t begin = cursor;
      size_t token = cursor + 6;
      while (token < text.size() && regex_space(static_cast<unsigned char>(text[token]))) ++token;
      size_t end = token;
      while (end < text.size()) {
        unsigned char c = static_cast<unsigned char>(text[end]);
        if (!(std::isalnum(c) || c == '.' || c == '_' || c == '~' || c == '+' || c == '/' || c == '-')) break;
        ++end;
      }
      size_t value_len = end - token;
      if (value_len >= (legacy_bearer ? 12U : 20U)) {
        while (end < text.size() && text[end] == '=') ++end;
        matches.emplace_back(begin, end);
        cursor = end;
      } else {
        ++cursor;
      }
    }
  }
  return matches;
}

std::vector<MatchSpan> find_rule_matches(const Rule& rule, const std::string& text) {
  std::vector<MatchSpan> matches;
  std::unique_ptr<pcre2_match_data, decltype(&pcre2_match_data_free)> data(
      pcre2_match_data_create_from_pattern(rule.compiled.get(), nullptr), pcre2_match_data_free);
  std::unique_ptr<pcre2_match_context, decltype(&pcre2_match_context_free)> context(
      pcre2_match_context_create(nullptr), pcre2_match_context_free);
  if (!data || !context) throw std::runtime_error("regex allocation failed");
  pcre2_set_match_limit(context.get(), 10000000);
  pcre2_set_depth_limit(context.get(), 100000);
  pcre2_set_heap_limit(context.get(), 32768); // KiB; no JIT/native-stack matcher.
  PCRE2_SIZE offset = 0;
  uint32_t options = 0;
  while (offset <= text.size()) {
    const int rc = pcre2_match(rule.compiled.get(), reinterpret_cast<PCRE2_SPTR>(text.data()),
        text.size(), offset, options, data.get(), context.get());
    if (rc == PCRE2_ERROR_NOMATCH) {
      if (!options || offset == text.size()) break;
      ++offset;
      options = 0;
      continue;
    }
    // Resource exhaustion is an engine error, never an unmasked no-match.
    if (rc < 0) throw std::runtime_error("regex matching limit exceeded");
    const PCRE2_SIZE* groups = pcre2_get_ovector_pointer(data.get());
    const size_t begin = groups[0], finish = groups[1];
    const size_t group = static_cast<size_t>(rule.capture_group);
    if (group < static_cast<size_t>(rc) && groups[2 * group] != PCRE2_UNSET) {
      matches.push_back(MatchSpan{begin, finish, groups[2 * group], groups[2 * group + 1]});
    }
    offset = finish;
    // As with regex_iterator, try a nonempty match at the same position after
    // an empty match, then advance one byte only if that attempt cannot match.
    options = begin == finish ? PCRE2_NOTEMPTY_ATSTART | PCRE2_ANCHORED : 0;
  }
  return matches;
}

// Rules are applied in order so a request can contain several independent
// sensitive values.  A later regex must never inspect the placeholder emitted
// by an earlier rule; doing so creates nested tokens (for example PLATE inside
// IP_PUBLIC), which the restore map cannot reliably reconstruct.
bool overlaps_placeholder(const std::string& text, size_t begin, size_t end) {
  if (begin >= end || begin >= text.size()) return false;
  size_t pos = text.rfind("{{", begin);
  if (pos == std::string::npos) pos = text.find("{{", begin);
  while (pos != std::string::npos && pos < end) {
    PlaceholderMatch match;
    if (parse_restore_placeholder(text, pos, match) && match.braced && match.end > begin &&
        match.end >= 2 && text.compare(match.end - 2, 2, "}}") == 0) return true;
    pos = text.find("{{", pos + 2);
  }
  return false;
}

// A token already present in the client's history has no trustworthy original
// in this new request. Reserve its suffix so an occurrence slot cannot silently
// reinterpret it as a different host or credential. Never load another
// request's originals as a fallback: identical structural slots are deliberate.
void reserve_input_placeholders(RequestTokens& tokens, const std::string& text) {
  if (!tokens.structural_slots) return;
  for (size_t pos = 0; pos < text.size();) {
    PlaceholderMatch match;
    if (!parse_restore_placeholder(text, pos, match)) { ++pos; continue; }
    std::string suffix = match.suffix;
    for (char& c : suffix) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
    if (!tokens.reusable && tokens.unique_suffix_to_token.count(suffix)) throw std::runtime_error("input placeholder conflicts with request mapping");
    if (tokens.input_suffixes.size() >= 100000 && !tokens.input_suffixes.count(suffix)) throw std::runtime_error("input placeholder limit exceeded");
    tokens.input_suffixes.insert(std::move(suffix));
    pos = match.end;
  }
  // Decode complete JSON string spans independently of the outer document.
  // A streamed prefix can end mid-object while earlier strings are complete;
  // escaped braces/labels in those strings must reserve the same suffixes.
  if (text.find("\\u") != std::string::npos) {
    size_t pos = 0;
    while ((pos = text.find('"', pos)) != std::string::npos) {
      size_t end = pos + 1;
      bool escaped = false;
      for (; end < text.size(); ++end) {
        if (escaped) { escaped = false; continue; }
        if (text[end] == '\\') { escaped = true; continue; }
        if (text[end] == '"') break;
      }
      if (end == text.size()) break;
      JsonValue decoded;
      if (JsonParser(text.substr(pos, end + 1 - pos)).parse(decoded) && decoded.type == JsonValue::String && decoded.scalar != text.substr(pos + 1, end - pos - 1)) {
        reserve_input_placeholders(tokens, decoded.scalar);
      }
      pos = end + 1;
    }
  }
}

void reserve_json_value_placeholders(RequestTokens& tokens, const JsonValue& value) {
  if (value.type == JsonValue::String) reserve_input_placeholders(tokens, value.scalar);
  for (const auto& child : value.array) reserve_json_value_placeholders(tokens, child);
  for (const auto& child : value.object) reserve_json_value_placeholders(tokens, child.second);
}

std::string mask_text_internal(EngineImpl& e, RequestTokens& tokens,
                               std::string text, std::vector<size_t>* origins,
                               bool reserve_input = true, const std::string& json_context = "") {
  if (reserve_input) {
    reserve_input_placeholders(tokens, text);
    if (tokens.structural_slots && text.find("\\u") != std::string::npos) {
      JsonValue decoded;
      if (JsonParser(text).parse(decoded)) reserve_json_value_placeholders(tokens, decoded);
    }
  }
  if (origins) {
    origins->resize(text.size());
    for (size_t i = 0; i < text.size(); ++i) (*origins)[i] = i;
  }
  for (Rule& rule : e.rules) {
    if (rule.needle.empty()) continue;
    if (rule.regex) {
      const size_t prefix = rule.capture_group > 0 ? json_context.size() : 0;
      const std::string candidate = prefix ? json_context + text + "\"" : text;
      if (!passes_builtin_prefilter(rule, candidate)) continue;
      std::vector<std::pair<size_t, size_t>> builtin_ranges = builtin_matches(rule, candidate);
      const bool builtin = builtin_email_kind(rule) != 0 ||
                           rule.needle == R"(Bearer\s+[A-Za-z0-9._~+/-]{12,}=*)" ||
                           rule.needle == R"([Bb][Ee][Aa][Rr][Ee][Rr]\s+[A-Za-z0-9._~+/-]{20,}=*)";
      std::vector<MatchSpan> matches;
      if (!builtin_ranges.empty() && rule.capture_group == 0) {
        for (const auto& range : builtin_ranges) matches.push_back(MatchSpan{range.first, range.second, range.first, range.second});
      } else if (!builtin || rule.capture_group > 0) matches = find_rule_matches(rule, candidate);
      matches.erase(std::remove_if(matches.begin(), matches.end(), [&](const MatchSpan& match) {
        if (match.replace_begin < prefix || match.replace_end > prefix + text.size()) return true;
        if (overlaps_placeholder(candidate, match.replace_begin, match.replace_end)) return true;
        const std::string value = candidate.substr(match.replace_begin, match.replace_end - match.replace_begin);
        return rule.semantic && !semantic_match_valid(candidate, match.begin, match.end, match.replace_begin, match.replace_end, value, rule.label);
      }), matches.end());
      for (auto& match : matches) { match.replace_begin -= prefix; match.replace_end -= prefix; }
      if (matches.empty()) continue;
      std::string result;
      std::vector<size_t> result_origins;
      if (origins) result_origins.reserve(text.size());
      size_t cursor = 0;
      for (const auto& match : matches) {
        const size_t pos = match.replace_begin;
        const size_t length = match.replace_end - match.replace_begin;
        if (pos < cursor) continue;
        result.append(text, cursor, pos - cursor);
        if (origins) result_origins.insert(result_origins.end(), origins->begin() + cursor, origins->begin() + pos);
        const std::string original = text.substr(pos, length);
        const std::string token = make_token(tokens, original, rule.label);
        result += token;
        if (origins) {
          const size_t source = pos < origins->size() ? (*origins)[pos] : 0;
          result_origins.insert(result_origins.end(), token.size(), source);
        }
        cursor = pos + length;
      }
      result.append(text, cursor, std::string::npos);
      if (origins) result_origins.insert(result_origins.end(), origins->begin() + cursor, origins->end());
      text = std::move(result);
      if (origins) *origins = std::move(result_origins);
      continue;
    }
    size_t pos = 0;
    size_t cursor = 0;
    std::string result;
    std::vector<size_t> result_origins;
    if (origins) result_origins.reserve(text.size());
    bool replaced = false;
    while ((pos = text.find(rule.needle, pos)) != std::string::npos) {
      // Do not recursively mask our own placeholders.
      if (overlaps_placeholder(text, pos, pos + rule.needle.size())) { pos += rule.needle.size(); continue; }
      result.append(text, cursor, pos - cursor);
      if (origins) result_origins.insert(result_origins.end(), origins->begin() + cursor, origins->begin() + pos);
      const std::string token = make_token(tokens, rule.needle, rule.label);
      result += token;
      if (origins) {
        const size_t source = pos < origins->size() ? (*origins)[pos] : 0;
        result_origins.insert(result_origins.end(), token.size(), source);
      }
      cursor = pos + rule.needle.size();
      pos = cursor;
      replaced = true;
    }
    if (replaced) {
      result.append(text, cursor, std::string::npos);
      if (origins) result_origins.insert(result_origins.end(), origins->begin() + cursor, origins->end());
      text = std::move(result);
      if (origins) *origins = std::move(result_origins);
    }
  }
  return text;
}

std::string mask_text(EngineImpl& e, RequestTokens& tokens, std::string text) {
  return mask_text_internal(e, tokens, std::move(text), nullptr);
}

bool is_builtin_stream_rule(const Rule& rule) {
  return rule.regex && (rule.needle == R"(([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}))" ||
                        rule.needle == R"(Bearer\s+[A-Za-z0-9._~+/-]{12,}=*)" ||
                        rule.needle == R"([Bb][Ee][Aa][Rr][Ee][Rr]\s+[A-Za-z0-9._~+/-]{20,}=*)");
}

bool mask_stream_requires_buffer(const EngineImpl& e) {
  for (const Rule& rule : e.rules) {
    if (rule.regex && !is_builtin_stream_rule(rule)) return true;
  }
  return false;
}

bool stream_word_char(unsigned char c) {
  return email_atom(c) || c == '@' || c == '~' || c == '/';
}

bool suffix_is_bearer_prefix(const std::string& text, size_t* start) {
  static const std::string prefix = "Bearer";
  const size_t limit = std::min(prefix.size() - 1, text.size());
  for (size_t length = limit; length > 0; --length) {
    const size_t candidate_start = text.size() - length;
    // A one-byte (or partial) suffix only represents a Bearer prefix when it
    // starts at a token boundary. Without this check, the final `b` in an
    // already-started value such as `Bearer ab` is mistaken for a new prefix,
    // allowing the real Bearer candidate to be emitted before it is complete.
    if (candidate_start > 0 && stream_word_char(static_cast<unsigned char>(text[candidate_start - 1]))) {
      continue;
    }
    bool matches = true;
    for (size_t i = 0; i < length; ++i) {
      if (std::tolower(static_cast<unsigned char>(text[text.size() - length + i])) !=
          std::tolower(static_cast<unsigned char>(prefix[i]))) {
        matches = false;
        break;
      }
    }
    if (matches) {
      *start = candidate_start;
      return true;
    }
  }
  return false;
}

size_t mask_stream_safe_cut(const EngineImpl& e, const std::string& text) {
  if (text.empty()) return 0;
  size_t keep = 0;
  for (const Rule& rule : e.rules) {
    if (!rule.regex) {
      keep = std::max(keep, rule.needle.empty() ? size_t(0) : rule.needle.size() - 1);
    }
  }
  bool has_builtin = false;
  for (const Rule& rule : e.rules) {
    if (is_builtin_stream_rule(rule)) {
      has_builtin = true;
      break;
    }
  }
  if (has_builtin) {
    size_t segment_start = text.size();
    while (segment_start > 0 && stream_word_char(static_cast<unsigned char>(text[segment_start - 1]))) {
      --segment_start;
    }
    if (segment_start < text.size()) keep = std::max(keep, text.size() - segment_start);
    size_t bearer_start = 0;
    if (suffix_is_bearer_prefix(text, &bearer_start)) {
      keep = std::max(keep, text.size() - bearer_start);
    } else {
      size_t found = std::string::npos;
      for (size_t i = 0; i + 6 <= text.size(); ++i) {
        if (bearer_word_at(text, i)) found = i;
      }
      if (found != std::string::npos) {
        bool bearer_candidate = true;
        for (size_t i = found + 6; i < text.size(); ++i) {
          const unsigned char c = static_cast<unsigned char>(text[i]);
          if (!regex_space(c) && !stream_word_char(c) && c != '=') {
            bearer_candidate = false;
            break;
          }
        }
        if (bearer_candidate) keep = std::max(keep, text.size() - found);
      }
    }
  }
  if (keep >= text.size()) return 0;
  size_t cut = text.size() - keep;
  bool moved = true;
  while (moved && cut > 0) {
    moved = false;
    for (const Rule& rule : e.rules) {
      if (rule.regex || rule.needle.empty()) continue;
      const size_t max_overlap = std::min(cut, rule.needle.size() - 1);
      for (size_t overlap = max_overlap; overlap > 0; --overlap) {
        if (text.compare(cut - overlap, overlap, rule.needle, 0, overlap) == 0) {
          cut -= overlap;
          moved = true;
          break;
        }
      }
      if (moved) break;
    }
  }
  // Do not split an input placeholder before it can reserve its occurrence
  // suffix. This also prevents custom literals matching a placeholder label.
  const size_t open = text.rfind("{{", cut);
  if (open != std::string::npos) {
    const size_t close = text.find("}}", open + 2);
    if ((close == std::string::npos && text.size() - open <= 128) ||
        (close != std::string::npos && close + 2 > cut)) cut = open;
  }
  if (cut > 0 && text[cut - 1] == '{') --cut;
  return cut;
}

bool mask_stream_candidate_overflow(const EngineImpl& e, const std::string& text) {
  if (text.size() <= kMaxBuiltinStreamCandidateBytes) return false;
  bool has_builtin = false;
  for (const Rule& rule : e.rules) {
    if (is_builtin_stream_rule(rule)) {
      has_builtin = true;
      break;
    }
  }
  if (!has_builtin) return false;
  return mask_stream_safe_cut(e, text) == 0;
}

bool first_non_whitespace_is_json(const std::string& text, char* value) {
  for (unsigned char c : text) {
    if (c == ' ' || c == '\t' || c == '\r' || c == '\n') continue;
    *value = static_cast<char>(c);
    return true;
  }
  return false;
}

bool looks_like_json(const std::string& text) {
  for (unsigned char c : text) {
    if (c == ' ' || c == '\t' || c == '\r' || c == '\n') continue;
    return c == '{' || c == '[' || c == '"';
  }
  return false;
}

void collect_json_rule_replacements(const JsonValue& value, const std::string& key,
                                    EngineImpl& engine, RequestTokens& tokens,
                                    std::vector<JsonReplacement>& replacements) {
  for (const auto& replaced : replacements) {
    if (value.source_begin >= replaced.begin && value.source_end <= replaced.end) return;
  }
  if (value.type == JsonValue::String || value.type == JsonValue::Number) {
    std::string context;
    if (!key.empty()) { quote_json(key, context); context += ":\""; }
    std::string masked = mask_text_internal(engine, tokens, value.scalar, nullptr, false, context);
    if (masked != value.scalar) {
      std::string encoded; quote_json(masked, encoded);
      add_json_replacement(replacements, JsonReplacement{value.source_begin, value.source_end, std::move(encoded)});
    }
  }
  for (const auto& child : value.array) collect_json_rule_replacements(child, "", engine, tokens, replacements);
  for (const auto& child : value.object) collect_json_rule_replacements(child.second, child.first, engine, tokens, replacements);
}

bool mask_json_internal(EngineImpl& e, RequestTokens& tokens, const std::string& input,
                        std::string& output, std::vector<size_t>* origins = nullptr) {
  reserve_input_placeholders(tokens, input);
  JsonValue root;
  JsonParser parser(input);
  if (!parser.parse(root)) return false;
  reserve_json_value_placeholders(tokens, root);
  std::vector<JsonReplacement> replacements;
  for (const auto& path : e.field_paths) {
    auto parts = split_path(path);
    if (parts.empty()) return false;
    std::vector<JsonReplacement> candidates;
    collect_json_replacements(root, parts, 0, e, tokens, candidates);
    for (auto& candidate : candidates) add_json_replacement(replacements, std::move(candidate));
  }
  collect_json_rule_replacements(root, "", e, tokens, replacements);
  apply_json_replacements(input, replacements, output, origins);
  return true;
}

bool placeholder_suffix(const std::string& text, size_t at, size_t length) {
  if (at + length > text.size()) return false;
  bool all_hex = true;
  bool all_legacy_consonants = length == 6;
  static const std::string consonants = "bcdfghjkmnpqrstvwxz";
  for (size_t i = 0; i < length; ++i) {
    const unsigned char c = static_cast<unsigned char>(text[at + i]);
    const char lower = static_cast<char>(std::tolower(c));
    if (!std::isxdigit(c)) all_hex = false;
    if (consonants.find(lower) == std::string::npos) all_legacy_consonants = false;
  }
  return (length == 16 && all_hex) || (length == 6 && (all_hex || all_legacy_consonants));
}

bool placeholder_word(unsigned char c) {
  return std::isalnum(c) || c == '_';
}

bool parse_restore_placeholder(const std::string& text, size_t begin, PlaceholderMatch& match) {
  size_t pos = begin;
  size_t slashes = 0;
  while (pos < text.size() && text[pos] == '\\' && slashes < 3) { ++pos; ++slashes; }
  size_t opens = 0;
  while (pos < text.size() && text[pos] == '{' && opens < 3) {
    ++pos;
    ++opens;
    size_t between = 0;
    while (pos < text.size() && text[pos] == '\\' && between < 3) { ++pos; ++between; }
  }
  if (opens == 0 && slashes != 0) return false;
  if (opens == 0 && begin > 0 && placeholder_word(static_cast<unsigned char>(text[begin - 1]))) return false;
  size_t whitespace = 0;
  while (pos < text.size() && std::isspace(static_cast<unsigned char>(text[pos])) && whitespace < 64) { ++pos; ++whitespace; }
  const size_t label_begin = pos;
  while (pos < text.size() && placeholder_word(static_cast<unsigned char>(text[pos])) && pos - label_begin < 13) ++pos;
  const size_t label_end = pos;
  if (label_end == label_begin) return false;

  for (size_t delimiter = label_end; delimiter > label_begin; --delimiter) {
    const size_t underscore = delimiter - 1;
    if (text[underscore] != '_' || underscore == label_begin) continue;
    if (underscore - label_begin > 12) continue;
    for (size_t suffix_size : {size_t(16), size_t(6)}) {
      const size_t suffix_begin = underscore + 1;
      if (!placeholder_suffix(text, suffix_begin, suffix_size)) continue;
      size_t after = suffix_begin + suffix_size;
      size_t trailing_space = 0;
      while (after < text.size() && std::isspace(static_cast<unsigned char>(text[after])) && trailing_space < 64) { ++after; ++trailing_space; }
      size_t closes = 0;
      size_t close_end = after;
      while (close_end < text.size() && closes < 3) {
        size_t escaped_brace = close_end;
        size_t close_slashes = 0;
        while (escaped_brace < text.size() && text[escaped_brace] == '\\' && close_slashes < 3) { ++escaped_brace; ++close_slashes; }
        if (escaped_brace >= text.size() || text[escaped_brace] != '}') break;
        close_end = escaped_brace + 1;
        ++closes;
      }
      const bool braced = opens != 0 || closes != 0;
      const size_t suffix_end = suffix_begin + suffix_size;
      if (suffix_end < text.size() && placeholder_word(static_cast<unsigned char>(text[suffix_end]))) continue;
      match.end = braced ? close_end : suffix_begin + suffix_size;
      match.suffix_begin = suffix_begin;
      match.suffix_size = suffix_size;
      match.label = text.substr(label_begin, underscore - label_begin);
      match.suffix = text.substr(suffix_begin, suffix_size);
      match.braced = braced;
      return true;
    }
  }
  return false;
}

std::string json_escape_string_contents(const std::string& value) {
  static const char hex[] = "0123456789abcdef";
  std::string escaped;
  escaped.reserve(value.size());
  for (unsigned char c : value) {
    switch (c) {
      case '"': escaped += "\\\""; break;
      case '\\': escaped += "\\\\"; break;
      case '\b': escaped += "\\b"; break;
      case '\f': escaped += "\\f"; break;
      case '\n': escaped += "\\n"; break;
      case '\r': escaped += "\\r"; break;
      case '\t': escaped += "\\t"; break;
      default:
        if (c < 0x20) {
          escaped += "\\u00";
          escaped += hex[c >> 4];
          escaped += hex[c & 0x0f];
        } else {
          escaped += static_cast<char>(c);
        }
    }
  }
  return escaped;
}

size_t incomplete_restore_suffix(const RequestTokens& tokens, const std::string& text) {
  size_t pending = std::string::npos;
  // Retain the whole braced candidate, including partial escaped delimiters.
  // Restoring after only the first closing brace would leak the second brace
  // into the next delta. The bound covers both 64-byte whitespace allowances
  // and every delimiter accepted by parse_restore_placeholder.
  const size_t begin = text.size() > 256 ? text.size() - 256 : 0;
  for (size_t start = begin; start < text.size(); ++start) {
    if (text[start] != '{' && text[start] != '\\') continue;
    size_t pos = start;
    size_t slashes = 0;
    while (pos < text.size() && text[pos] == '\\' && slashes < 3) { ++pos; ++slashes; }
    if (pos == text.size()) { pending = start; break; }
    size_t opens = 0;
    while (pos < text.size() && text[pos] == '{' && opens < 3) {
      ++pos; ++opens;
      size_t between = 0;
      while (pos < text.size() && text[pos] == '\\' && between < 3) { ++pos; ++between; }
    }
    if (!opens) continue;
    size_t whitespace = 0;
    while (pos < text.size() && std::isspace(static_cast<unsigned char>(text[pos])) && whitespace < 64) { ++pos; ++whitespace; }
    const size_t word = pos;
    while (pos < text.size() && placeholder_word(static_cast<unsigned char>(text[pos])) && pos - word < 29) ++pos;
    if (pos < text.size() && placeholder_word(static_cast<unsigned char>(text[pos]))) continue;
    whitespace = 0;
    while (pos < text.size() && std::isspace(static_cast<unsigned char>(text[pos])) && whitespace < 64) { ++pos; ++whitespace; }
    size_t closes = 0;
    while (pos < text.size() && closes < 3) {
      size_t escaped = pos;
      size_t close_slashes = 0;
      while (escaped < text.size() && text[escaped] == '\\' && close_slashes < 3) { ++escaped; ++close_slashes; }
      if (escaped == text.size()) { pos = escaped; break; }
      if (text[escaped] != '}') break;
      pos = escaped + 1;
      ++closes;
    }
    if (pos == text.size() && closes < 3) { pending = start; break; }
  }
  // Bare known labels also need lookahead before accepting a complete suffix:
  // the next chunk may extend the word and make it ordinary literal text.
  size_t word_begin = text.size();
  while (word_begin > 0 && placeholder_word(static_cast<unsigned char>(text[word_begin - 1]))) --word_begin;
  if (word_begin < text.size() && text.size() - word_begin <= 29) {
    if (tokens.resolver) return pending == std::string::npos ? word_begin : std::min(pending, word_begin);
    std::string candidate = text.substr(word_begin);
    for (char& c : candidate) c = static_cast<char>(std::toupper(static_cast<unsigned char>(c)));
    for (const auto& label : tokens.labels) {
      const std::string prefix = label + "_";
      bool partial = candidate.size() < prefix.size() && prefix.compare(0, candidate.size(), candidate) == 0;
      if (candidate.compare(0, prefix.size(), prefix) == 0 && candidate.size() - prefix.size() <= 16) {
        const std::string suffix = candidate.substr(prefix.size());
        partial = std::all_of(suffix.begin(), suffix.end(), [](unsigned char c) { return std::isxdigit(c) != 0; }) ||
                  (suffix.size() <= 6 && suffix.find_first_not_of("BCDFGHJKMNPQRSTVWXZ") == std::string::npos);
      }
      if (partial) { pending = pending == std::string::npos ? word_begin : std::min(pending, word_begin); break; }
    }
  }
  if (pending != std::string::npos && text.size() - pending > 256) return std::string::npos;
  return pending;
}

void advance_json_state(const std::string& text, size_t begin, size_t end,
                        bool& in_string, bool& escaped) {
  for (size_t i = begin; i < end; ++i) {
    const char c = text[i];
    if (!in_string) {
      if (c == '"') in_string = true;
      continue;
    }
    if (escaped) {
      escaped = false;
    } else if (c == '\\') {
      escaped = true;
    } else if (c == '"') {
      in_string = false;
    }
  }
}

std::string restore_text(RequestTokens& tokens, std::string text, bool final,
                         std::string* remainder = nullptr, bool json_escape = false,
                         bool* json_in_string = nullptr, bool* json_escape_pending = nullptr,
                         bool* previous_input_word = nullptr) {
  bool local_in_string = json_in_string ? *json_in_string : false;
  bool local_escape_pending = json_escape_pending ? *json_escape_pending : false;
  size_t scan_end = text.size();
  if (!final) {
    const size_t pending = incomplete_restore_suffix(tokens, text);
    if (pending != std::string::npos) scan_end = pending;
  }

  // Parse both canonical and model-normalized spellings in one pass. Suffix
  // fallback is allowed only for visibly braced forms and only when that
  // suffix uniquely identifies a token in this request.
  std::string result;
  result.reserve(scan_end);
  size_t cursor = 0;
  while (cursor < scan_end) {
    PlaceholderMatch match;
    const bool continuing_word = cursor == 0 && previous_input_word && *previous_input_word &&
                                placeholder_word(static_cast<unsigned char>(text[cursor]));
    if (continuing_word || !parse_restore_placeholder(text, cursor, match) || match.end > scan_end) {
      if (json_escape) advance_json_state(text, cursor, cursor + 1, local_in_string, local_escape_pending);
      result += text[cursor++];
      continue;
    }
    const bool replacement_is_json_string_content = json_escape && local_in_string;
    std::string canonical = "{{" + match.label + "_" + match.suffix + "}}";
    for (char& c : canonical) c = static_cast<char>(std::toupper(static_cast<unsigned char>(c)));
    const size_t canonical_sep = canonical.rfind('_');
    if (canonical_sep != std::string::npos) {
      for (size_t i = canonical_sep + 1; i < canonical.size(); ++i) canonical[i] = static_cast<char>(std::tolower(static_cast<unsigned char>(canonical[i])));
    }
    auto found = tokens.tokens_to_originals.find(canonical);
    if (found == tokens.tokens_to_originals.end() && match.braced) {
      std::string normalized_suffix = match.suffix;
      for (char& c : normalized_suffix) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
      auto suffix = tokens.unique_suffix_to_token.find(normalized_suffix);
      if (suffix != tokens.unique_suffix_to_token.end() && !suffix->second.empty()) {
        std::string expected = suffix->second.substr(2, suffix->second.rfind('_') - 2);
        std::string requested = match.label;
        for (char& c : requested) c = static_cast<char>(std::toupper(static_cast<unsigned char>(c)));
        expected.erase(std::remove(expected.begin(), expected.end(), '_'), expected.end());
        requested.erase(std::remove(requested.begin(), requested.end(), '_'), requested.end());
        if (!tokens.resolver || expected == requested) found = tokens.tokens_to_originals.find(suffix->second);
      }
    }
    std::string historical, issued;
    const bool local_found = found != tokens.tokens_to_originals.end();
    bool reused = false;
    if (!local_found && tokens.reusable) {
      reused = tokens.resolver ? durable_mapping(tokens, false, canonical, "", issued, historical) :
          reusable_tokens.find(tokens.reuse_scope, canonical, match.braced, historical, &issued);
    }
    if (reused) {
      tokens.tokens_to_originals[issued] = historical;
      tokens.labels.insert(issued.substr(2, issued.rfind('_') - 2));
      // This is a historical reference, not a new masking occurrence.
      tokens.occurrences.try_emplace(issued, 0);
    }
    if (!local_found && !reused) {
      result.append(text, cursor, match.end - cursor);
    } else {
      const std::string& original = reused ? historical : found->second;
      result += replacement_is_json_string_content ? json_escape_string_contents(original) : original;
    }
    if (json_escape) advance_json_state(text, cursor, match.end, local_in_string, local_escape_pending);
    cursor = match.end;
  }
  if (json_escape && json_in_string && json_escape_pending) {
    advance_json_state(text, cursor, scan_end, local_in_string, local_escape_pending);
    *json_in_string = local_in_string;
    *json_escape_pending = local_escape_pending;
  }
  if (previous_input_word && scan_end > 0) {
    *previous_input_word = placeholder_word(static_cast<unsigned char>(text[scan_end - 1]));
  }
  if (scan_end < text.size()) {
    if (remainder) remainder->assign(text, scan_end, std::string::npos);
  } else if (remainder) {
    remainder->clear();
  }
  return result;
}

int alloc_output(const std::string& s, char** out, size_t* out_len) {
  if (!out || !out_len) return fail("output pointers must not be null");
  *out = nullptr; *out_len = 0;
  if (s.empty()) return 0;
  char* p = static_cast<char*>(std::malloc(s.size()));
  if (!p) return fail("allocation failed");
  std::memcpy(p, s.data(), s.size()); *out = p; *out_len = s.size(); return 0;
}

} // namespace

struct engine { EngineImpl impl; };
struct offset_map { std::vector<std::pair<size_t, size_t>> points; };

extern "C" uint32_t engine_abi_version(void) {
  return SILENCER_ENGINE_ABI_VERSION;
}

extern "C" uint64_t engine_capabilities(void) {
  return SILENCER_ENGINE_CAP_MASK | SILENCER_ENGINE_CAP_RESTORE_STREAM |
         SILENCER_ENGINE_CAP_MASK_STREAM | SILENCER_ENGINE_CAP_REQUEST_SCOPED |
         SILENCER_ENGINE_CAP_RESTORE_OPTIONS | SILENCER_ENGINE_CAP_MASK_ITEMS |
         SILENCER_ENGINE_CAP_REUSABLE_TOKENS | SILENCER_ENGINE_CAP_DURABLE_MAPPINGS;
}

extern "C" engine_t* engine_create(const char* config_json, size_t config_len) {
  clear_error();
  engine_t* e = nullptr;
  try {
    e = new engine_t();
    if (!parse_config(e->impl, config_json, config_len)) {
      delete e;
      fail("invalid engine configuration");
      return nullptr;
    }
    return e;
  }
  catch (...) { delete e; fail("engine_create failed"); return nullptr; }
}

extern "C" void engine_destroy(engine_t* engine) {
  if (!engine) return;
  try {
    std::unique_lock<std::mutex> lock(engine->impl.lifecycle_mu);
    engine->impl.closing = true;
    engine->impl.lifecycle_cv.wait(lock, [&] { return engine->impl.active_calls == 0; });
    lock.unlock();
    delete engine;
  } catch (...) {
    // Destruction is best effort across the C ABI. All normal paths above are
    // noexcept in practice; never let an implementation exception escape.
  }
}

extern "C" int mask_request(engine_t* engine, const char* input, size_t input_len, char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || (!input && input_len)) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::unique_lock<std::shared_mutex> lock(engine->impl.token_mu);
    std::lock_guard<std::mutex> token_lock(engine->impl.legacy_tokens.mu);
    std::string source(input ? input : "", input_len);
    std::string masked;
    if (looks_like_json(source)) {
      if (!mask_json_internal(engine->impl, engine->impl.legacy_tokens, source, masked)) {
        if (!engine->impl.field_paths.empty()) return fail("invalid JSON document");
        masked = mask_text(engine->impl, engine->impl.legacy_tokens, std::move(source));
      }
    } else {
      masked = mask_text(engine->impl, engine->impl.legacy_tokens, std::move(source));
    }
    return alloc_output(masked, out, out_len);
  }
  catch (...) { return fail("mask_request failed"); }
}

extern "C" int mask_request_with_map(engine_t* engine, const char* input, size_t input_len, char** out, size_t* out_len, offset_map_t** map) {
  clear_error();
  if (!out || !out_len || !map) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  *map = nullptr;
  if (!engine || (!input && input_len)) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::unique_lock<std::shared_mutex> lock(engine->impl.token_mu);
    std::lock_guard<std::mutex> token_lock(engine->impl.legacy_tokens.mu);
    std::string source(input ? input : "", input_len);
    std::vector<size_t> origins;
    std::string masked;
    if (looks_like_json(source)) {
      if (!mask_json_internal(engine->impl, engine->impl.legacy_tokens, source, masked, &origins)) {
        if (!engine->impl.field_paths.empty()) return fail("invalid JSON document");
        masked = mask_text_internal(engine->impl, engine->impl.legacy_tokens, std::move(source), &origins);
      }
    } else {
      masked = mask_text_internal(engine->impl, engine->impl.legacy_tokens, std::move(source), &origins);
    }
    auto created = std::make_unique<offset_map_t>();
    created->points.reserve(masked.size() + 1);
    for (size_t i = 0; i < masked.size(); ++i) created->points.emplace_back(i, i < origins.size() ? origins[i] : input_len);
    created->points.emplace_back(masked.size(), input_len);
    int rc = alloc_output(masked, out, out_len);
    if (rc != 0) return rc;
    *map = created.release();
    return 0;
  } catch (...) { *map = nullptr; return fail("mask_request_with_map failed"); }
}

extern "C" int offset_map_lookup(const offset_map_t* map, size_t masked_offset, size_t* original_offset) {
  clear_error();
  if (!map || !original_offset) return fail("invalid offset map");
  if (map->points.empty()) { *original_offset = 0; return 0; }
  // The terminal point is the only valid lookup at the masked output end.
  // Rejecting larger offsets prevents callers from silently treating an
  // out-of-range cursor as EOF and hiding an offset accounting bug.
  if (masked_offset > map->points.back().first) return fail("offset outside map");
  auto it = std::upper_bound(map->points.begin(), map->points.end(), std::make_pair(masked_offset, static_cast<size_t>(-1)));
  if (it == map->points.begin()) *original_offset = 0; else { --it; *original_offset = it->second; }
  return 0;
}

extern "C" void offset_map_free(offset_map_t* map) { delete map; }

extern "C" int json_redact(engine_t* engine, const char* input, size_t input_len, char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || (!input && input_len)) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    JsonValue root; JsonParser parser(std::string(input ? input : "", input_len));
    if (!parser.parse(root)) return fail("invalid JSON document");
    std::unique_lock<std::shared_mutex> lock(engine->impl.token_mu);
    std::lock_guard<std::mutex> token_lock(engine->impl.legacy_tokens.mu);
    std::string source(input ? input : "", input_len), encoded;
    if (!mask_json_internal(engine->impl, engine->impl.legacy_tokens, source, encoded)) return fail("invalid JSON field path");
    return alloc_output(encoded, out, out_len);
  } catch (...) { return fail("json_redact failed"); }
}

extern "C" uint64_t restore_stream_begin(engine_t* engine) {
  clear_error(); if (!engine) { fail("invalid engine"); return 0; }
  CallGuard guard(&engine->impl); if (!guard) { fail("engine is closing"); return 0; }
  try {
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    uint64_t id = engine->impl.next_stream++;
    if (!id) id = engine->impl.next_stream++;
    auto state = std::make_shared<Stream>();
    state->request_tokens = std::shared_ptr<RequestTokens>(&engine->impl.legacy_tokens, [](RequestTokens*) {});
    engine->impl.streams.emplace(id, std::move(state));
    return id;
  }
  catch (...) { fail("restore_stream_begin failed"); return 0; }
}

extern "C" uint64_t restore_stream_begin_for_mask(engine_t* engine, uint64_t mask_stream) {
  clear_error();
  if (!engine || !mask_stream) { fail("invalid argument"); return 0; }
  CallGuard guard(&engine->impl); if (!guard) { fail("engine is closing"); return 0; }
  try {
    std::shared_ptr<MaskStream> masking;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(mask_stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      masking = it->second;
    }
    std::shared_ptr<RequestTokens> request_tokens;
    {
      std::lock_guard<std::mutex> mask_lock(masking->mu);
      if (!masking->closed || !masking->request_tokens || !masking->request_tokens->structural_slots) {
        fail("mask stream is not a completed request session");
        return 0;
      }
      request_tokens = masking->request_tokens;
    }
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    uint64_t id = engine->impl.next_stream++;
    if (!id) id = engine->impl.next_stream++;
    auto state = std::make_shared<Stream>();
    state->request_tokens = std::move(request_tokens);
    engine->impl.streams.emplace(id, std::move(state));
    return id;
  } catch (...) { fail("restore_stream_begin_for_mask failed"); return 0; }
}

extern "C" int restore_stream_push(engine_t* engine, uint64_t stream, const char* input, size_t input_len, char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || !stream || (!input && input_len)) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  std::shared_ptr<Stream> state;
  try {
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.streams.find(stream);
      if (it == engine->impl.streams.end()) return fail("unknown stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown stream");
    state->started = true;
    state->pending.append(input ? input : "", input_len);
    std::string remainder;
    if (!state->request_tokens) return fail("restore stream has no token map");
    std::shared_lock<std::shared_mutex> engine_lock(engine->impl.token_mu, std::defer_lock);
    if (state->request_tokens.get() == &engine->impl.legacy_tokens) engine_lock.lock();
    std::lock_guard<std::mutex> token_lock(state->request_tokens->mu);
    std::string emitted = restore_text(*state->request_tokens, state->pending, false, &remainder,
                                       state->json_escape, &state->json_in_string, &state->json_escape_pending,
                                       &state->previous_input_word);
    state->pending = std::move(remainder);
    return alloc_output(emitted, out, out_len);
  } catch (...) {
    if (state) {
      std::lock_guard<std::mutex> state_lock(state->mu);
      state->closed = true;
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.streams.find(stream);
      if (it != engine->impl.streams.end() && it->second == state) engine->impl.streams.erase(it);
    }
    return fail("restore_stream_push failed");
  }
}

extern "C" int restore_stream_set_options(engine_t* engine, uint64_t stream, uint32_t options) {
  clear_error();
  if (!engine || !stream || (options & ~SILENCER_RESTORE_OPTION_JSON_ESCAPE) != 0) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  std::shared_ptr<Stream> state;
  try {
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.streams.find(stream);
      if (it == engine->impl.streams.end()) return fail("unknown stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown stream");
    if (state->started && ((options & SILENCER_RESTORE_OPTION_JSON_ESCAPE) != 0) != state->json_escape) {
      return fail("restore options cannot change after the first push");
    }
    state->json_escape = (options & SILENCER_RESTORE_OPTION_JSON_ESCAPE) != 0;
    return 0;
  } catch (...) { return fail("restore_stream_set_options failed"); }
}

extern "C" int restore_stream_finish(engine_t* engine, uint64_t stream, char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  std::shared_ptr<Stream> state;
  try {
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.streams.find(stream);
      if (it == engine->impl.streams.end()) return fail("unknown stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown stream");
    if (!state->request_tokens) return fail("restore stream has no token map");
    std::shared_lock<std::shared_mutex> engine_lock(engine->impl.token_mu, std::defer_lock);
    if (state->request_tokens.get() == &engine->impl.legacy_tokens) engine_lock.lock();
    std::lock_guard<std::mutex> token_lock(state->request_tokens->mu);
    std::string out_text = restore_text(*state->request_tokens, std::move(state->pending), true, nullptr,
                                        state->json_escape, &state->json_in_string, &state->json_escape_pending,
                                        &state->previous_input_word);
    int rc = alloc_output(out_text, out, out_len);
    state->closed = true;
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    auto it = engine->impl.streams.find(stream);
    if (it != engine->impl.streams.end() && it->second == state) engine->impl.streams.erase(it);
    return rc;
  } catch (...) {
    // A failed finish is terminal just like a failed push. Do not leave a
    // partially restored token buffer reachable by a later caller.
    if (state) {
      std::lock_guard<std::mutex> state_lock(state->mu);
      state->closed = true;
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.streams.find(stream);
      if (it != engine->impl.streams.end() && it->second == state) engine->impl.streams.erase(it);
    }
    return fail("restore_stream_finish failed");
  }
}

extern "C" int restore_stream_abort(engine_t* engine, uint64_t stream) {
  clear_error(); if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::shared_ptr<Stream> state;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.streams.find(stream);
      if (it == engine->impl.streams.end()) return fail("unknown stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown stream");
    state->closed = true;
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    auto it = engine->impl.streams.find(stream);
    if (it != engine->impl.streams.end() && it->second == state) engine->impl.streams.erase(it);
    return 0;
  } catch (...) { return fail("restore_stream_abort failed"); }
}

extern "C" uint64_t mask_stream_begin(engine_t* engine) {
  clear_error();
  if (!engine) { fail("invalid engine"); return 0; }
  CallGuard guard(&engine->impl); if (!guard) { fail("engine is closing"); return 0; }
  try {
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    uint64_t id = engine->impl.next_stream++;
    if (!id) id = engine->impl.next_stream++;
    auto state = std::make_shared<MaskStream>();
    state->buffered = mask_stream_requires_buffer(engine->impl);
    state->decided = state->buffered;
    engine->impl.mask_streams.emplace(id, std::move(state));
    return id;
  } catch (...) { fail("mask_stream_begin failed"); return 0; }
}

extern "C" uint64_t mask_stream_begin_scoped(engine_t* engine) {
  clear_error();
  if (!engine) { fail("invalid engine"); return 0; }
  CallGuard guard(&engine->impl); if (!guard) { fail("engine is closing"); return 0; }
  try {
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    uint64_t id = engine->impl.next_stream++;
    if (!id) id = engine->impl.next_stream++;
    auto state = std::make_shared<MaskStream>();
    state->buffered = mask_stream_requires_buffer(engine->impl);
    state->decided = state->buffered;
    state->request_tokens = std::make_shared<RequestTokens>();
    state->request_tokens->structural_slots = true;
    engine->impl.mask_streams.emplace(id, std::move(state));
    return id;
  } catch (...) { fail("mask_stream_begin_scoped failed"); return 0; }
}

extern "C" uint64_t mask_stream_begin_reusable(engine_t* engine, const char* scope, size_t scope_len) {
  clear_error();
  if (!engine) { fail("invalid engine"); return 0; }
  CallGuard guard(&engine->impl); if (!guard) { fail("engine is closing"); return 0; }
  if (!scope || !scope_len || scope_len > 256) { fail("invalid mapping scope"); return 0; }
  const uint64_t id = mask_stream_begin_scoped(engine);
  if (!id) return 0;
  try {
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    auto& tokens = engine->impl.mask_streams.at(id)->request_tokens;
    tokens->reusable = true;
    tokens->reuse_scope.assign(scope, scope_len);
    tokens->labels = reusable_tokens.labels(tokens->reuse_scope);
    return id;
  } catch (...) { fail("mask_stream_begin_reusable failed"); return 0; }
}

extern "C" int mask_stream_bind_mapping_resolver(engine_t* engine, uint64_t stream, uintptr_t handle,
                                                  mapping_resolver_fn resolve, mapping_release_fn release) {
  clear_error();
  if (!engine || !stream || !resolve || !release) return fail("invalid mapping resolver");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::shared_ptr<MaskStream> state;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> lock(state->mu);
    if (state->closed || !state->pending.empty() || !state->request_tokens ||
        !state->request_tokens->reusable || state->request_tokens->resolver) return fail("mask stream already started");
    state->request_tokens->resolver = std::make_unique<MappingResolver>(handle, resolve, release);
    return 0;
  } catch (...) { return fail("bind mapping resolver failed"); }
}

extern "C" int engine_import_reusable_tokens(engine_t* engine, const char* scope, size_t scope_len,
                                               const char* input, size_t input_len) {
  clear_error();
  if (!engine || !scope || !scope_len || scope_len > 256 || (!input && input_len)) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    JsonValue root;
    if (!JsonParser(std::string(input, input_len)).parse(root) || root.type != JsonValue::Array) return fail("invalid mapping JSON");
    for (const auto& row : root.array) {
      std::string token, original;
      std::time_t used = 0;
      bool has_original = false;
      for (const auto& field : row.object) {
        if (field.first == "preview" && field.second.type == JsonValue::String) token = field.second.scalar;
        if (field.first == "original" && field.second.type == JsonValue::String) { original = field.second.scalar; has_original = true; }
        if (field.first == "used_at" && field.second.type == JsonValue::Number) used = static_cast<std::time_t>(std::stoll(field.second.scalar));
      }
      PlaceholderMatch match;
      if (!has_original || !parse_restore_placeholder(token, 0, match) || match.end != token.size() || match.suffix_size != 6) continue;
      const std::string canonical = "{{" + normalize_label(match.label) + "_" + match.suffix + "}}";
      if (canonical != token) continue;
      reusable_tokens.seed(std::string(scope, scope_len), token, original, normalize_label(match.label), used);
    }
    return 0;
  } catch (...) { return fail("engine_import_reusable_tokens failed"); }
}

extern "C" int engine_clear_reusable_tokens(engine_t* engine) {
  clear_error();
  if (!engine) return fail("invalid engine");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  reusable_tokens.clear();
  return 0;
}

extern "C" int mask_stream_push(engine_t* engine, uint64_t stream,
                                  const char* input, size_t input_len,
                                  char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || !stream || (!input && input_len)) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  std::shared_ptr<MaskStream> state;
  try {
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown mask stream");
    state->pending.append(input ? input : "", input_len);
    if (!state->decided) {
      char first = 0;
      if (first_non_whitespace_is_json(state->pending, &first)) {
        state->buffered = first == '{' || first == '[' || first == '"';
        state->decided = true;
      }
    }
    if (state->buffered || !state->decided) return alloc_output(std::string(), out, out_len);
    if (mask_stream_candidate_overflow(engine->impl, state->pending)) {
      state->closed = true;
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
      return fail("built-in stream candidate exceeds safety limit");
    }
    const size_t cut = mask_stream_safe_cut(engine->impl, state->pending);
    if (cut == 0) return alloc_output(std::string(), out, out_len);
    std::unique_lock<std::shared_mutex> token_lock(engine->impl.token_mu, std::defer_lock);
    RequestTokens* request_tokens = state->request_tokens ? state->request_tokens.get() : &engine->impl.legacy_tokens;
    if (request_tokens == &engine->impl.legacy_tokens) token_lock.lock();
    std::lock_guard<std::mutex> request_lock(request_tokens->mu);
    std::string prefix = state->pending.substr(0, cut);
    state->pending.erase(0, cut);
    return alloc_output(mask_text(engine->impl, *request_tokens, std::move(prefix)), out, out_len);
  } catch (...) {
    if (state) {
      std::lock_guard<std::mutex> state_lock(state->mu);
      state->closed = true;
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
    }
    return fail("mask_stream_push failed");
  }
}

extern "C" int mask_stream_finish(engine_t* engine, uint64_t stream,
                                    char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  std::shared_ptr<MaskStream> state;
  try {
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown mask stream");
    std::unique_lock<std::shared_mutex> token_lock(engine->impl.token_mu, std::defer_lock);
    RequestTokens* request_tokens = state->request_tokens ? state->request_tokens.get() : &engine->impl.legacy_tokens;
    if (request_tokens == &engine->impl.legacy_tokens) token_lock.lock();
    std::lock_guard<std::mutex> request_lock(request_tokens->mu);
    std::string source = std::move(state->pending);
    std::string masked;
    if (state->buffered && looks_like_json(source)) {
      const bool valid = mask_json_internal(engine->impl, *request_tokens, source, masked);
      if (!valid) {
        if (engine->impl.field_paths.empty()) {
          masked = mask_text(engine->impl, *request_tokens, std::move(source));
        } else {
          state->closed = true;
          std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
          auto it = engine->impl.mask_streams.find(stream);
          if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
          return fail("invalid JSON document");
        }
      }
    } else {
      masked = mask_text(engine->impl, *request_tokens, std::move(source));
    }
    int rc = alloc_output(masked, out, out_len);
    if (rc != 0) {
      state->closed = true;
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
      return rc;
    }
    state->closed = true;
    if (!state->request_tokens || !state->request_tokens->structural_slots) {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
    }
    return rc;
  } catch (...) {
    if (state) {
      std::lock_guard<std::mutex> state_lock(state->mu);
      state->closed = true;
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
    }
    return fail("mask_stream_finish failed");
  }
}

extern "C" int mask_stream_items_json(engine_t* engine, uint64_t stream,
                                        char** out, size_t* out_len) {
  clear_error();
  if (!out || !out_len) return fail("invalid argument");
  *out = nullptr; *out_len = 0;
  if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::shared_ptr<MaskStream> state;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (!state->closed || !state->request_tokens || !state->request_tokens->structural_slots) {
      return fail("mask stream items are unavailable before completion");
    }
    std::lock_guard<std::mutex> request_lock(state->request_tokens->mu);
    std::vector<std::pair<std::string, std::string>> entries(
        state->request_tokens->tokens_to_originals.begin(),
        state->request_tokens->tokens_to_originals.end());
    std::sort(entries.begin(), entries.end(), [](const auto& a, const auto& b) {
      return a.first < b.first;
    });
    std::string json = "[";
    bool first = true;
    for (const auto& entry : entries) {
      const size_t sep = entry.first.rfind('_');
      if (sep == std::string::npos || entry.first.size() < sep + 3 || entry.first.substr(entry.first.size() - 2) != "}}") continue;
      if (!first) json += ',';
      first = false;
      json += "{\"label\":";
      quote_json(entry.first.substr(2, sep - 2), json);
      json += ",\"preview\":";
      quote_json(entry.first, json);
      json += ",\"original\":";
      quote_json(entry.second, json);
      if (state->request_tokens->reusable) {
        json += ",\"count\":" + std::to_string(state->request_tokens->occurrences[entry.first]);
      }
      json += ",\"display\":\"plaintext\"}";
    }
    json += ']';
    return alloc_output(json, out, out_len);
  } catch (...) {
    return fail("mask_stream_items_json failed");
  }
}

extern "C" int mask_stream_seal(engine_t* engine, uint64_t stream) {
  clear_error();
  if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::shared_ptr<MaskStream> state;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> lock(state->mu);
    state->pending.clear();
    state->closed = true;
    return 0;
  } catch (...) { return fail("mask_stream_seal failed"); }
}

extern "C" int mask_stream_abort(engine_t* engine, uint64_t stream) {
  clear_error();
  if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::shared_ptr<MaskStream> state;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    std::lock_guard<std::mutex> state_lock(state->mu);
    if (state->closed) return fail("unknown mask stream");
    state->closed = true;
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    auto it = engine->impl.mask_streams.find(stream);
    if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
    return 0;
  } catch (...) { return fail("mask_stream_abort failed"); }
}

extern "C" int mask_stream_release(engine_t* engine, uint64_t stream) {
  clear_error();
  if (!engine || !stream) return fail("invalid argument");
  CallGuard guard(&engine->impl); if (!guard) return fail("engine is closing");
  try {
    std::shared_ptr<MaskStream> state;
    {
      std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
      auto it = engine->impl.mask_streams.find(stream);
      if (it == engine->impl.mask_streams.end()) return fail("unknown mask stream");
      state = it->second;
    }
    {
      std::lock_guard<std::mutex> state_lock(state->mu);
      if (!state->closed) return fail("mask stream is not finished");
    }
    std::lock_guard<std::mutex> lock(engine->impl.streams_mu);
    auto it = engine->impl.mask_streams.find(stream);
    if (it != engine->impl.mask_streams.end() && it->second == state) engine->impl.mask_streams.erase(it);
    return 0;
  } catch (...) { return fail("mask_stream_release failed"); }
}

extern "C" void buffer_free(void* buffer) { std::free(buffer); }
extern "C" const char* last_error(void) { return g_last_error.c_str(); }
