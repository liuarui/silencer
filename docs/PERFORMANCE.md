# Performance testing

The repository provides synthetic fixtures and measurement scripts. Measure the same build, hardware, database, enabled rules and upstream behavior when comparing results; no universal throughput guarantee is implied.

## Engine

```bash
cmake -S engine -B build -DCMAKE_BUILD_TYPE=Release -DSILENCER_ENGINE_BUILD_BENCH=ON
cmake --build build
ctest --test-dir build --output-on-failure
build/cpp/engine_benchmark --size 2097152 --rounds 101
```

## Gateway and streams

Start `node scripts/benchmark-fixture.mjs` and configure an isolated gateway with `SILENCER_UPSTREAM_URL=http://127.0.0.1:19090`. Use a disposable database and synthetic text.

```bash
SILENCER_BENCH_URL=http://127.0.0.1:8080 \
SILENCER_BENCH_PATH=/api/requests/v1/chat \
SILENCER_BENCH_REQUESTS=10000 \
SILENCER_BENCH_CONCURRENCY=32 \
scripts/benchmark-gateway.sh

SILENCER_STREAM_URL=http://127.0.0.1:8080 \
SILENCER_STREAM_PATH=/api/requests/stream \
SILENCER_STREAM_REQUESTS=200 \
SILENCER_STREAM_CONCURRENCY=16 \
scripts/benchmark-stream.sh
```

The gateway benchmark needs ApacheBench (`ab`); the stream benchmark uses curl. Supply `SILENCER_BENCH_TOKEN` or `SILENCER_STREAM_TOKEN` if management authentication is enabled. Use one benchmark profile at a time against each gateway.

The fixture supports `SILENCER_FIXTURE_SSE_FRAMES`, `SILENCER_FIXTURE_NDJSON_FRAMES`, `SILENCER_FIXTURE_FRAME_INTERVAL_MS` and `SILENCER_FIXTURE_FIRST_BYTE_DELAY_MS` for stream duration and timing.

Each run records latency samples, metadata and before/after metrics beside its output file. `SILENCER_BENCH_OUTPUT` and `SILENCER_STREAM_OUTPUT` choose the destinations. Metadata records the build revision, dirty state, hardware, concurrency and request-body hash. Do not publish request bodies, credentials, database copies or internal deployment records with benchmark results.

Compare request failures and proxy/mask/storage/rejection counters alongside throughput. Test long conversations, cancellation and failure behavior separately from short-request latency. Report whether MySQL or SQLite, durable storage or tmpfs, and actual provider latency were included. Provider-reported cache usage remains the source of truth for cache hits; stable placeholders alone do not prove a cache hit.
