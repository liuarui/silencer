# Changelog

## 1.1.2 - 2026-10-22

- Release the browser web-chat extension for ChatGPT, Gemini and DeepSeek text conversations.
- Make the Docker gateway extension API opt-in by default instead of hard-disabled, with an environment-level hard-disable override.
- Add protocol v3 durable, browser-install and page-origin-scoped mappings so response restoration survives separate requests without leaking between sites.
- Flush completed logical text values independently, preventing fragments from unrelated response fields from being joined as a placeholder.
- Publish reviewed extension source and tests in Git, and build a reproducible ZIP plus checksum automatically for the GitHub Release.
- Update the website, download links, release documentation and version metadata.

See [1.1.2 release notes](docs/releases/1.1.2.md).

## 1.1.1 - 2026-10-21

- Fix ordinary identifiers being mistaken for unresolved placeholders in tool arguments, which could interrupt Hermes and other streamed tool calls.
- Preserve identifier boundaries across response deltas and check pending candidates at completion or EOF.
- Retain fail-closed protection for genuine unresolved tool placeholders, including escaped input and snapshots.

See [1.1.1 release notes](docs/releases/1.1.1.md).

## 1.1.0 - 2026-10-20

- Native capture for Windows x64, macOS Intel/Apple Silicon and Linux x64.
- Visual installation, real permission/TLS/processing checks, repair, update and removal.
- Local processing by default; authenticated HTTPS remote processing and scoped rules/logs.
- Shared reverse-proxy/capture masking, streaming restoration and tool protection.
- Hardened installer rollback, Windows ACL/process cleanup and runtime helper verification.
- Native package CI and verified amd64/arm64 Docker publishing.
- Windows/Linux system-permission and proxy coexistence field validation remains required.

### 使用与验证

- 网页引导安装、授权、证书检查与卸载；捕获默认本地处理，可验证后切换 HTTPS 远程处理端。
- Windows 原生全量测试、PowerShell 5.1/7、ACL、进程清理与完整 ZIP 启动通过；macOS Intel/Apple Silicon、Linux 原生包及 Docker 双架构均通过 CI。
- 修复 Windows 日志句柄删除、SQLite 盘符与特殊字符备份路径，以及私有配置、事务恢复与备份权限。
- 官网提供三端下载、捕获保护边界、发布说明与 Changelog 入口。

See [1.1.0 release notes](docs/releases/1.1.0.md).

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
