#!/usr/bin/env bash
set -euo pipefail

# Verification gate for the Go/C++ gateway and React console.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 "$ROOT/scripts/check-public-release.py"
python3 "$ROOT/scripts/test-release-pipeline.py" -v

echo "[go] unit tests"
(cd "$ROOT/gateway" && go test ./...)

if command -v g++ >/dev/null 2>&1; then
  command -v pkg-config >/dev/null 2>&1 && pkg-config --exists libpcre2-8 || {
    echo "PCRE2 development headers and pkg-config are required" >&2; exit 1;
  }
  native_dir="${TMPDIR:-/tmp}/silencer-verify-native"
  mkdir -p "$native_dir"
  echo "[cpp] strict build and test"
  g++ -std=c++17 -Wall -Wextra -Werror -fPIC -shared \
    "$ROOT/engine/cpp/engine.cpp" -I"$ROOT/engine/cpp" \
    $(pkg-config --cflags --libs libpcre2-8) -o "$native_dir/libsilencer_engine.so"
  g++ -std=c++17 -pthread -Wall -Wextra -Werror \
    "$ROOT/engine/cpp/engine_test.cpp" -I"$ROOT/engine/cpp" \
    -L"$native_dir" -lsilencer_engine -Wl,-rpath,"$native_dir" \
    -o "$native_dir/engine_test"
  "$native_dir/engine_test"
  echo "[go] cgo engine tests"
  (cd "$ROOT/gateway" && CGO_ENABLED=1 CGO_LDFLAGS="-L$native_dir -Wl,-rpath,$native_dir" go test -tags silencer_engine ./...)
  echo "[go] production gateway build"
  (cd "$ROOT/gateway" && CGO_ENABLED=1 CGO_LDFLAGS="-L$native_dir -Wl,-rpath,$native_dir" go build -tags silencer_engine -trimpath -o "$native_dir/silencer-gateway" ./cmd/gateway)
  if [[ -f "$ROOT/extension/manifest.json" ]]; then
    echo "[extension] native gateway integration"
    python3 "$ROOT/extension/tests/native.py" "$native_dir/silencer-gateway"
  fi
  if command -v clang++ >/dev/null 2>&1; then
    fuzz_bin="$native_dir/engine_fuzz"
    if clang++ -std=c++17 -Wall -Wextra -Werror -fsanitize=fuzzer,address,undefined \
      "$ROOT/engine/cpp/engine.cpp" "$ROOT/engine/cpp/engine_fuzz.cpp" \
      -I"$ROOT/engine/cpp" $(pkg-config --cflags --libs libpcre2-8) -o "$fuzz_bin" >/dev/null 2>&1; then
      echo "[cpp] fuzz smoke"
      fuzz_corpus="$(mktemp -d "$native_dir/fuzz-corpus.XXXXXX")"
      trap 'rm -rf "$fuzz_corpus"' EXIT
      cp "$ROOT/engine/cpp/corpus/"* "$fuzz_corpus/"
      "$fuzz_bin" -runs=1000 -max_len=4096 "$fuzz_corpus"
      rm -rf "$fuzz_corpus"
      trap - EXIT
    else
      echo "[cpp] fuzz skipped: clang libFuzzer runtime unavailable"
    fi
  fi
else
  echo "[cpp] g++ is required to verify the production engine" >&2
  exit 1
fi

echo "[frontend] build"
(cd "$ROOT/frontend" && npm run build)
(cd "$ROOT/frontend" && npm run lint && npm run test:logs && npm run test:capture)

echo "[node] static checks"
python3 "$ROOT/scripts/test-installer-lifecycle.py" -v
bash -n "$ROOT/scripts/silencer-blue-green.sh"
bash -n "$ROOT/scripts/benchmark-gateway.sh"
bash -n "$ROOT/scripts/benchmark-stream.sh"
bash -n "$ROOT/scripts/test-migrations.sh"
bash "$ROOT/scripts/test-blue-green.sh"
bash "$ROOT/scripts/test-migrations.sh"
bash "$ROOT/scripts/test-mysql.sh"
node "$ROOT/scripts/check-i18n.mjs"
node --experimental-strip-types "$ROOT/scripts/check-env-import.mjs"
if [[ -f "$ROOT/extension/manifest.json" ]]; then
  node "$ROOT/scripts/check-extension.mjs"
  node "$ROOT/extension/tests/protocols.mjs"
  node "$ROOT/scripts/test-extension-ledger.mjs"
  node "$ROOT/scripts/test-extension-connection.mjs"
  node "$ROOT/scripts/test-extension-registration.mjs"
  extension_tmp="$(mktemp -d "${TMPDIR:-/tmp}/silencer-extension-release.XXXXXX")"
  trap 'rm -rf "$extension_tmp"' EXIT
  version="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["version"])' "$ROOT/frontend/package.json")"
  python3 "$ROOT/scripts/assemble-extension-release.py" --version "$version" --output "$extension_tmp/silencer-webchat-$version.zip"
  rm -rf "$extension_tmp"
  trap - EXIT
fi

echo "verify-all: OK"
