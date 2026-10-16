# Changelog

## 1.0.0 - 2026-10-16

- First stable 1.0 release.
- Go gateway with the C++ masking engine and React console.
- `/api` control and data-plane contract.
- MySQL and SQLite storage, JSON/SSE/NDJSON restoration, and metrics.
- Durable scoped mappings preserve placeholders across long conversations, multiple turns, restarts and log expiry.
- Tool-call protection supports observe, rewrite and block, with documented rule examples.
- Fixed canceled terminal-frame cleanup, provider header forwarding, shared upstream connection reuse and probe status classification.
- Heap-backed bounded regex matching avoids native stack overflow and preserves empty-class and NUL-escape behavior.
- Browser extension release is deferred; its APIs and downloads remain disabled in production.

### 文档 / Documentation

- README 和部署文档统一产品定位、上游/客户端边界、浏览器扩展限制、存储范围和发布检查说明。

发布内容与验证说明见 [1.0.0 release notes](docs/releases/1.0.0.md)。
