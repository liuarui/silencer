# Silencer 1.1.1

[English](README_EN.md) · [官网](https://silencer.5970000.xyz/) · [在线演示](https://silencer.5970000.xyz/demo/) · [Docker Hub](https://hub.docker.com/r/liuarui/silencer) · [部署教程](docs/TUTORIAL.md) · [架构](docs/ARCHITECTURE.md) · [性能测试](docs/PERFORMANCE.md)

Silencer 是面向 AI 客户端的隐私脱敏网关。请求转发前，将敏感文本替换为占位符；响应返回时，还原普通回复和工具调用参数。运行时采用 Go 网关、C++ 脱敏引擎和 React 控制台。

## 功能

- 支持 JSON、SSE、NDJSON，按逻辑通道处理流式占位符还原。
- 内置规则、自定义敏感词和正则规则，可在控制台开关并热更新。
- 持久化映射保持多轮对话的占位符稳定，支持历史上下文、重启和日志清理后的还原。
- 命名上游路由、附加请求头和共享连接池。
- 请求账本、异常筛选、404 噪声白名单，原文默认以掩码展示。
- 危险工具调用检测：仅记录、覆写为确认提示、阻断。默认仅记录。
- SQLite 或 MySQL 存储、日志保留设置、Prometheus 指标和可选 OpenTelemetry。

**浏览器扩展暂不发布。** 插件源码、扩展专用页面和安装文档不包含在本仓库；正式镜像关闭扩展接口及下载。

官网静态源码及独立演示实例的部署方式见 [website/README.md](website/README.md)。公共演示账号用于体验，请勿提交真实凭据或个人数据；演示站点在 Nginx 层禁用修改密码接口。

## 快速启动

发布镜像支持 `linux/amd64` 和 `linux/arm64`，`1.1.1` 与 `latest` 会自动选择本机架构：

```bash
docker pull liuarui/silencer:1.1.1
export SILENCER_GATEWAY_TOKEN="$(openssl rand -hex 24)"
docker run -d --name silencer --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v silencer_data:/data \
  -e SILENCER_GATEWAY_TOKEN \
  -e SILENCER_CONTROL_CONFIG_FILE=/data/control.json \
  liuarui/silencer:1.1.1
curl http://127.0.0.1:8080/readyz
```

打开 <http://127.0.0.1:8080/>，默认管理账号为 `admin`，密码是刚设置的 `SILENCER_GATEWAY_TOKEN`。`/data` 保存控制配置、运行设置、修改后的管理密码、SQLite 数据库和日志文件。远程访问时，通过可信的 HTTPS 反向代理暴露服务。

从源码构建并运行：

```bash
docker compose up -d --build
```

使用 MySQL：

```bash
cp .env.mysql.example .env
# 填写数据库密码；公网部署还需设置 SILENCER_GATEWAY_TOKEN
docker compose -f docker-compose.mysql.yml up -d --build
```

## 捕获模式

网页侧栏提供捕获模式安装向导，支持本地处理（默认）或通过 HTTPS 交给指定 Silencer 服务器处理。捕获始终需要本机后台组件；服务器可提供安装引导和远程处理，不会直接接管另一台电脑的网络。

1.1 提供 Windows 10/11 x64、macOS 14+（Intel / Apple Silicon）、Ubuntu 24.04 x64（内核 6.8+）安装包。网页按系统提供脚本，逐步检查组件、权限、证书与处理链路，并提供修复、更新和卸载引导。客户端沿用原地址和 Key。

从 [GitHub Release](https://github.com/liuarui/silencer/releases/latest) 下载组件。Docker 负责服务器部署；捕获组件安装在实际运行 AI 客户端的电脑上。macOS 已完成本地捕获与 Clash TUN 联调；Windows/Linux 的系统授权、代理共存仍需在各自实机核验。系统要求与保护边界见 [捕获模式说明](docs/CAPTURE.md)。

## 接入客户端

在控制台添加命名上游后，将客户端的 API 基地址设置为：

```text
http://127.0.0.1:8080/r/<上游名称>/v1
```

代理客户端继续使用供应商 API Key。管理密码用于控制台与控制 API；网关会移除管理令牌和控制台 Cookie，再转发请求。

也可配置全局 `SILENCER_UPSTREAM_URL`，通过 `/api/requests/<path>` 转发。例如 `/api/requests/v1/chat/completions`。

“网关状态”检测只检查本地网关；models/chat 检测才请求上游，只有 HTTP 2xx 判为成功。

## 数据与行为边界

详细日志默认保留请求、脱敏请求、上游响应和还原响应 3 天，可在设置中关闭或调整保留时间。日志包含原文，控制台默认遮罩仅影响显示。还原映射独立保存在数据库中，不随日志过期或清空删除，应随数据库保护和备份。

危险工具调用防护只检测工具参数，不检测普通回复或思考文本。覆写模式返回确认提示，不会替客户端审批或自动恢复执行；检测是基于规则的检查，不能代替执行环境的权限控制。

引擎不可用、规则处理或还原失败时，网关返回错误。二进制响应不执行文本还原。具体协议与资源边界见[架构说明](docs/ARCHITECTURE.md)。

## 开发与验证

需要 Go 1.25+、Node.js 22.12+、C++17 编译器、PCRE2 开发包及 pkg-config。

```bash
# Debian/Ubuntu
sudo apt-get install g++ libpcre2-dev pkg-config
npm --prefix frontend ci
bash scripts/verify-all.sh
```

公开验证覆盖 Go、严格 C++、原生引擎、前端、日志与隐私、迁移及部署脚本。MySQL 测试需要独立的测试数据库和 `SILENCER_TEST_MYSQL_DSN`；无 DSN 时明确跳过。完整 Docker 验证：

```bash
docker build -t silencer:test .
bash scripts/test-image.sh silencer:test
```

见[贡献说明](CONTRIBUTING.md)、[1.1.1 发布说明](docs/releases/1.1.1.md)和[第三方许可](THIRD_PARTY_NOTICES.md)。本项目按 [AGPL-3.0-only](LICENSE) 授权。

## Sponsors

Sponsored by [ForZTN](https://sponsorship.forztn.com/github.com/liuarui/silencer).
