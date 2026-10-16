#ifndef SILENCER_REUSABLE_TOKENS_H
#define SILENCER_REUSABLE_TOKENS_H

#include <algorithm>
#include <ctime>
#include <list>
#include <iterator>
#include <mutex>
#include <random>
#include <string>
#include <stdexcept>
#include <unordered_map>
#include <unordered_set>

namespace silencer {

// Opaque entity identities survive individual request sessions and engine
// reloads. Each route/credential scope has its own forward and reverse keys.
class ReusableTokenRegistry {
  struct Entry {
    std::string token, original, label, forward_key, suffix_key;
    std::time_t touched;
    size_t bytes;
    std::list<std::string>::iterator position;
  };
  std::mutex mutex_;
  std::random_device entropy_;
  std::unordered_map<std::string, Entry> reverse_;
  std::unordered_map<std::string, std::string> forward_, suffix_;
  std::list<std::string> order_;
  size_t bytes_ = 0;
  std::time_t last_prune_ = 0;
  static constexpr size_t max_entries_ = 20000;
  static constexpr size_t max_bytes_ = 16 << 20;
  static constexpr std::time_t ttl_ = 24 * 60 * 60;

  static std::string key(const std::string& scope, const std::string& value) {
    return scope + '\0' + value;
  }
  static std::string value_key(const std::string& original, const std::string& label) {
    return std::to_string(original.size()) + ":" + original + ":" + label;
  }
  void erase(std::unordered_map<std::string, Entry>::iterator it) {
    auto canonical = forward_.find(it->second.forward_key);
    if (canonical != forward_.end() && canonical->second == it->first) forward_.erase(canonical);
    suffix_.erase(it->second.suffix_key);
    bytes_ -= it->second.bytes;
    order_.erase(it->second.position);
    reverse_.erase(it);
  }
  void prune(std::time_t now) {
    if (!last_prune_ || now - last_prune_ >= 60) {
      for (auto it = reverse_.begin(); it != reverse_.end();) {
        if (now - it->second.touched > ttl_) { auto expired = it++; erase(expired); }
        else ++it;
      }
      last_prune_ = now;
    }
    while (!order_.empty() && (reverse_.size() > max_entries_ || bytes_ > max_bytes_)) erase(reverse_.find(order_.front()));
  }
  void touch(Entry& entry, std::time_t now) {
    entry.touched = now;
    order_.splice(order_.end(), order_, entry.position);
  }
  bool insert(const std::string& scope, const std::string& token,
              const std::string& original, const std::string& label, std::time_t when) {
    const std::string rkey = key(scope, token);
    const std::string fkey = key(scope, value_key(original, label));
    const size_t split = token.rfind('_');
    const std::string skey = key(scope, token.substr(split + 1, token.size() - split - 3));
    auto previous = reverse_.find(rkey);
    if (previous != reverse_.end()) {
      if (previous->second.original != original) return false;
      touch(previous->second, std::max(when, previous->second.touched));
      return true;
    }
    auto occupied = suffix_.find(skey);
    if (occupied != suffix_.end() && occupied->second != rkey) return false;
    const size_t cost = rkey.size() + fkey.size() + skey.size() + original.size() + token.size() + 512;
    if (cost > max_bytes_) return false;
    order_.push_back(rkey);
    reverse_.emplace(rkey, Entry{token, original, label, fkey, skey, when, cost, std::prev(order_.end())});
    auto canonical = forward_.find(fkey);
    if (canonical == forward_.end() || reverse_.at(canonical->second).touched <= when) forward_[fkey] = rkey;
    suffix_[skey] = rkey;
    bytes_ += cost;
    prune(std::time(nullptr));
    return reverse_.count(rkey) != 0;
  }

 public:
  std::string issue(const std::string& scope, const std::string& original,
                    const std::string& label, const std::unordered_set<std::string>& reserved) {
    std::lock_guard<std::mutex> lock(mutex_);
    const auto now = std::time(nullptr);
    prune(now);
    auto existing = forward_.find(key(scope, value_key(original, label)));
    if (existing != forward_.end()) {
      auto entry = reverse_.find(existing->second);
      if (now - entry->second.touched <= ttl_) {
        touch(entry->second, now);
        return entry->second.token;
      }
      erase(entry);
    }
    static const std::string alphabet = "bcdfghjkmnpqrstvwxz";
    std::uniform_int_distribution<size_t> pick(0, alphabet.size() - 1);
    std::string suffix, token;
    do {
      suffix.clear();
      for (size_t i = 0; i < 6; ++i) suffix += alphabet[pick(entropy_)];
      token = "{{" + label + "_" + suffix + "}}";
    } while (reserved.count(suffix) || suffix_.count(key(scope, suffix)));
    if (!insert(scope, token, original, label, now)) throw std::runtime_error("reusable mapping capacity exceeded");
    return token;
  }

  bool find(const std::string& scope, const std::string& token, bool suffix_fallback,
            std::string& original, std::string* issued = nullptr) {
    std::lock_guard<std::mutex> lock(mutex_);
    const auto now = std::time(nullptr);
    prune(now);
    auto it = reverse_.find(key(scope, token));
    if (it == reverse_.end() && suffix_fallback) {
      const size_t split = token.rfind('_');
      auto suffix = suffix_.find(key(scope, token.substr(split + 1, token.size() - split - 3)));
      if (suffix != suffix_.end()) {
        auto candidate = reverse_.find(suffix->second);
        std::string requested = token.substr(2, split - 2);
        requested.erase(std::remove(requested.begin(), requested.end(), '_'), requested.end());
        std::string actual = candidate->second.label;
        actual.erase(std::remove(actual.begin(), actual.end(), '_'), actual.end());
        if (requested == actual) it = candidate;
      }
    }
    if (it == reverse_.end()) return false;
    if (now - it->second.touched > ttl_) { erase(it); return false; }
    original = it->second.original;
    if (issued) *issued = it->second.token;
    touch(it->second, now);
    return true;
  }

  bool seed(const std::string& scope, const std::string& token, const std::string& original,
            const std::string& label, std::time_t when) {
    std::lock_guard<std::mutex> lock(mutex_);
    const auto now = std::time(nullptr);
    prune(now);
    if (when <= 0 || when > now || now - when > ttl_) return false;
    return insert(scope, token, original, label, when);
  }

  std::unordered_set<std::string> labels(const std::string& scope) {
    std::lock_guard<std::mutex> lock(mutex_);
    prune(std::time(nullptr));
    std::unordered_set<std::string> result;
    const std::string prefix = key(scope, "");
    for (const auto& entry : reverse_) {
      if (entry.first.compare(0,prefix.size(),prefix)==0) result.insert(entry.second.label);
    }
    return result;
  }

  void clear() {
    std::lock_guard<std::mutex> lock(mutex_);
    reverse_.clear(); forward_.clear(); suffix_.clear(); order_.clear(); bytes_ = 0; last_prune_ = 0;
  }
};
}  // namespace silencer
#endif
