# Go gateway + native C++ masking engine. The legacy runtime is intentionally
# absent: this image is the full rewrite and has one process/data plane.
FROM node:22-alpine AS frontend-builder
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci --prefer-offline
COPY frontend/ ./
RUN npm run build && rm -rf dist/downloads

FROM golang:1.25-bookworm AS gateway-builder
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends g++ libpcre2-dev \
    && rm -rf /var/lib/apt/lists/*
COPY gateway/ ./gateway/
COPY engine/ ./engine/
RUN mkdir -p /out && g++ -std=c++17 -O2 -fPIC -shared /src/engine/cpp/engine.cpp \
    -I/src/engine/cpp -lpcre2-8 -o /out/libsilencer_engine.so
WORKDIR /src/gateway
RUN CGO_ENABLED=1 CGO_LDFLAGS='-L/out -Wl,-rpath,/usr/local/lib' \
    go build -tags silencer_engine -trimpath -ldflags='-s -w' -o /out/silencer-gateway ./cmd/gateway \
    && CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/silencer-migrate ./cmd/migrate \
    && CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/silencer-host-agent ./cmd/host-agent

FROM debian:bookworm-slim AS runtime
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Silencer" \
    org.opencontainers.image.source="https://github.com/liuarui/silencer" \
    org.opencontainers.image.version=$VERSION \
    org.opencontainers.image.revision=$REVISION
ENV SILENCER_GATEWAY_ADDR=0.0.0.0:8080 \
    SILENCER_CONTROL_CONFIG_FILE=/data/control.json \
    LD_LIBRARY_PATH=/usr/local/lib \
    SILENCER_WEB_DIST=/app/web_dist \
    SILENCER_ENGINE=cpp \
    SILENCER_MAX_BODY_BYTES=67108864
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl libstdc++6 libpcre2-8-0 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 silencer \
    && useradd --system --uid 10001 --gid silencer --create-home --home-dir /home/silencer silencer \
    && mkdir -p /data /run/silencer && chown -R silencer:silencer /data /run/silencer
COPY --from=gateway-builder /out/silencer-gateway /usr/local/bin/silencer-gateway
COPY --from=gateway-builder /out/silencer-migrate /usr/local/bin/silencer-migrate
COPY --from=gateway-builder /out/silencer-host-agent /usr/local/bin/silencer-host-agent
COPY --from=gateway-builder /out/libsilencer_engine.so /usr/local/lib/libsilencer_engine.so
COPY --from=frontend-builder /src/frontend/dist /app/web_dist
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/silencer/
COPY licenses/ /usr/share/licenses/silencer/third-party/
COPY frontend/src/assets/fonts/OFL-*.txt /usr/share/licenses/silencer/fonts/
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/readyz || exit 1
USER silencer
ENTRYPOINT ["/usr/local/bin/silencer-gateway"]
