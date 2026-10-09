# Silencer

项目地址：<https://github.com/liuarui/silencer> · 在线体验：<https://silencer.5970000.xyz/demo/>

公共演示账号：`demo` / `SilencerDemo-2026!`。只用于查看控制台和体验本地脱敏，不连接真实上游。

Silencer 是面向 AI 客户端的自托管隐私脱敏服务，可运行在你控制的服务器或其他主机上。支持 Base URL 的客户端把请求发送到 Silencer 的代理入口；请求在 Silencer 实例所在主机脱敏后转发给上游，响应返回时再流式还原。Web 控制台用于管理实例。默认不发送遥测，配置和数据由部署者控制。

命令行和桌面客户端通过通用 Base URL/环境变量接入。客户端接入页可检查当前进程可访问的受支持配置文件，并提供预览、备份和回滚；Docker 默认不挂载宿主机文件。网页扩展是另一条接入方式，只连接浏览器所在主机的 Silencer 服务，适合浏览器与服务同机运行。

## 核心优势

- **一条清晰链路**：客户端先请求 Silencer，Silencer 脱敏后再访问选定上游（包括 sub2api），响应返回时按会话映射流式还原。
- **多上游路由**：一个实例可以保存多个上游和规则集，客户端只需切换路由地址，不必重复改密钥和代理配置。
- **稳定的上下文映射**：同一会话中的每个敏感值使用稳定占位符；不同 IPv4、邮箱或凭据会分配不同标识，避免上下文串线。
- **可审计但少暴露**：请求账本按客户端原文、上游脱敏内容、上游响应、客户端还原内容分阶段保存；凭据只保留遮罩、类型、长度和摘要。
- **持久层可替换**：单实例可使用 SQLite，长期运行可切换 MySQL；后台队列与连接池避免数据库提交阻塞转发主链路。
- **可回滚的接入**：客户端配置助手先生成计划，再备份并原子写入；配置变更可查看历史、检测冲突并回滚。Docker 部署时，宿主机客户端需用宿主机脚本或手动应用计划。
- **由部署者掌控**：默认不发送遥测，配置、会话、规则和备份都由部署者控制。

### 上游和客户端是什么关系

客户端是请求的来源，例如 Claude Code、Codex、Hermes、Cursor；上游是 Silencer 要转发到的目标，例如 OpenAI、Anthropic、sub2api 或内部兼容服务。命令行客户端通过 Base URL 接入，链路如下：

```text
Claude / Codex / Hermes / Cursor
              │ Base URL
              ▼
        Silencer 5802
              │ 脱敏后转发
              ▼
       sub2api / 其他上游
```

Silencer 不替客户端管理 API Key，也不要求为每个客户端维护专用协议。客户端只要支持 `OPENAI_BASE_URL`、`ANTHROPIC_BASE_URL` 或同类 Base URL 配置，就可以接入同一个路由。浏览器扩展使用独立的本机桥接链路：扩展只访问浏览器所在主机的 `127.0.0.1` / `localhost:5801`，因此浏览器与提供桥接的 Silencer 实例需要在同一台机器上。

## 性能与边界

代理转发路径使用流式处理，事件和请求账本异步写入；规则扫描复用预过滤结果，并在不需要坐标映射时走轻量替换路径。下面是本机合成基准，不是生产 SLA：

- 100,000 条事件、其中 80% 为明确失败的 RESTORE 查询，p50 从 **345.51 ms** 降到 **91.44 ms**（约 **73.5%**）。
- 500,000 条同类事件，p50 从 **1,923.45 ms** 降到 **441.13 ms**（约 **77.1%**）。
- 2 MiB、91,180 次重复邮箱命中时，文本脱敏 p50 降低约 **20.2%**，JSON 脱敏 p50 降低约 **15.7%**。

基准只覆盖本地规则、解析和聚合，不包含网络、上游模型、TLS、数据库提交和 NER 推理。请在目标机器上使用 [性能文档](docs/PERFORMANCE.md) 中的脚本重新测量。

## 快速开始

### Docker / Web 控制台

```bash
docker build -t silencer:latest .

docker run -d --name silencer --restart unless-stopped \
  -p 127.0.0.1:5801:5801 -p 127.0.0.1:5802:5802 \
  -e SILENCER_REQUIRE_HTTPS=0 \
  -v silencer_data:/data \
  silencer:latest
```

首次启动后，运行 `docker logs -f silencer` 查看一次性初始化码。

也可由 Compose 自动构建并启动：`docker compose up -d --build`。随后用 `docker compose ps` 查看状态、用 `docker compose logs -f silencer` 查看初始化码；`http://127.0.0.1:5801/readyz` 返回 `{"ok":true}` 表示服务和请求存储已就绪。修改源码后重复构建命令即可更新镜像，命名卷 `silencer_data` 会保留配置和记录。`docker compose down` 会停止容器但保留数据卷。默认部署不依赖其他项目的 Docker 网络；如果 sub2api 容器已加入 `sub2api_sub2api-network`，并希望直接通过容器名访问它，也可以叠加配置：`docker compose -f docker-compose.yml -f docker-compose.sub2api.yml up -d --build`。

需要在同一台机器并行验收多个实例时，为每个实例使用不同的 Compose 项目名和宿主端口，例如 `docker compose -p silencer-next -f docker-compose.yml up -d --build`；Compose 会自动隔离容器和命名卷。

打开 `http://127.0.0.1:5801/`。首次打开时输入容器日志显示的一次性初始化码，创建管理员账号和密码；以后使用账号密码登录。生产环境请通过反向代理或防火墙限制面板访问，并持久化 `/data`。

Docker 示例只把端口绑定到部署主机的回环地址，并关闭管理面板的 HTTPS 强制，适合在该主机上体验或检查。应用默认关闭面板 HTTPS 强制，Compose 也显式设为关闭。供其他机器访问时，使用 HTTPS 反向代理保护管理面板 `5801`；代理入口 `5802` 也应单独提供 TLS，或仅允许通过可信内网访问。`SILENCER_REQUIRE_HTTPS=1` 只校验管理面板，不会为代理端口启用 TLS。需要从其他容器或主机访问时，可设置 `SILENCER_BIND_HOST=0.0.0.0`，并用防火墙限制来源；不要将管理端口直接暴露到公网。Nginx 终止面板 TLS 时设置 `SILENCER_TRUST_PROXY=1`，并透传单跳 `Host`、`X-Forwarded-Proto`、`X-Forwarded-Host`、`X-Real-IP`；可设置 `SILENCER_ALLOWED_HOSTS=panel.example.com` 锁定管理域名。登录、初始化、重置密码和改密接口带有失败退避，避免暴力撞库。

### 从源码部署服务

源码部署时，先构建控制台并安装 Python 服务：

```bash
npm --prefix frontend ci
npm --prefix frontend run build
python -m pip install -e './backend[ner]'
python -m silencer
```

服务会输出首次初始化码；数据默认保存在当前用户的数据目录，也可用 `SILENCER_DATA_DIR` 指定。要构建可分发的 wheel（含 Web 控制台静态文件），运行 `python scripts/build-wheel.py`，再在目标机器安装 `dist/packages/` 下生成的 `silencer-0.0.2-*.whl` 并运行 `silencer`。

### 选择数据存储

默认使用 SQLite 文件存储；Docker 部署时，事件、审计和请求账本都保存在持久化卷 `/data`。也可以通过环境变量切换：

```bash
# SQLite 文件数据库（保存在实例数据目录中，适合单实例部署）
SILENCER_STORAGE_MODE=file

# 显式 SQLite（可指定文件位置）
SILENCER_STORAGE_MODE=sqlite
SILENCER_SQLITE_PATH=/data/silencer.sqlite3

# MySQL（长期运行；实例共享记录时请先了解数据隔离边界）
SILENCER_STORAGE_MODE=mysql
SILENCER_MYSQL_DSN='mysql://silencer:密码@mysql.example.com:3306/silencer'
SILENCER_MYSQL_POOL_SIZE=8
```

DSN 中如果密码含 `@`、`:` 或 `/`，请先做 URL 编码。Compose MySQL 示例使用分离的 host/user/password/database 环境变量，因此密码可直接使用特殊字符；自建数据库也可以注入 `SILENCER_MYSQL_HOST`、`SILENCER_MYSQL_PORT`、`SILENCER_MYSQL_USER`、`SILENCER_MYSQL_PASSWORD`、`SILENCER_MYSQL_DATABASE`，或者继续使用 DSN。

MySQL 模式会复用连接，事件、审计和请求账本由后台队列写入；请求转发不会同步等待数据库提交。多个实例若连接同一个数据库，这些记录会进入相同的表，目前不提供实例或租户隔离；需要隔离时，请为每个实例使用独立数据库。每个实例的规则、上游和账号配置、备份仍由各自的 `/data` 持久化。可直接使用仓库中的 `docker-compose.mysql.yml`，首次启动会自动建表。复制 `.env.mysql.example` 为 `.env` 并设置强密码后再启动；Compose 会拒绝空缺密码，不要把真实凭据提交到仓库。

`file` 与 `sqlite` 都使用 SQLite 文件格式；`file` 使用 `silencer-data.sqlite3` 默认文件名，`sqlite` 使用 `silencer-events.sqlite3`，并支持通过 `SILENCER_SQLITE_PATH` 指定路径。面板的 `/api/status` 会显示当前模式和健康状态。

## 接入客户端

先在“系统设置”的“上游与客户端”区域添加目标服务，再到“客户端接入”页复制 Base URL。Docker 默认发布共享代理端口 `5802`，使用 `/r/<上游名称>` 路由：

- OpenAI 兼容：`http://<Silencer服务器地址>:5802/r/<上游名称>/v1`
- Anthropic：`http://<Silencer服务器地址>:5802/r/<上游名称>`

以上 HTTP 地址仅适用于本机或可信内网；跨公网接入时请替换为受 TLS 保护的代理地址。

控制台也会显示每个上游的独立端口（通常为 `18701` 起），适用于原生运行或额外发布这些端口的部署。Docker 默认只发布 `5801` 和 `5802`，因此通常使用共享路由格式；上游名称必须与控制台中的名称完全一致。

客户端与 Silencer 在同一台机器时可将服务器地址写成 `127.0.0.1`；跨服务器接入时，代理地址应使用受 TLS 保护的 HTTPS 入口，或可信内网地址。

Claude Code 可设置 `ANTHROPIC_BASE_URL`；OpenAI SDK、Codex、Cursor 等可设置 `OPENAI_BASE_URL`。API 密钥仍由客户端配置；请求由 Silencer 服务脱敏后转发，并由该服务处理会话映射。

如果客户端直接安装在 Docker 宿主机，可运行配套脚本自动检测并接入 Claude、Codex、Hermes：

```bash
./scripts/silencer-client.py apply --client all \
  --proxy-url http://127.0.0.1:5802/r/<上游名称>
```

脚本会先预览，输入 `yes` 后才备份并修改；完成后会输出回滚清单。完整参数见 [使用教程](docs/TUTORIAL.md)。其他客户端可手动设置 `OPENAI_BASE_URL` 或 `ANTHROPIC_BASE_URL`，再把地址指向同一个 Silencer 路由。

## Web 与安全边界

“网页扩展”适用于支持的浏览器 AI 网站，扩展只连接浏览器所在主机的 Silencer 实例；请在浏览器和 Silencer 同机时安装并配置令牌。请求账本可保存脱敏前后正文，凭据类请求头会遮罩；请按需要设置保留期限并限制控制台访问。规则在“敏感词库”页面管理，风险事件在“安全审计”查看；安全检测是辅助措施，不能替代权限控制、备份和代码审查。

客户端原始请求会先到达 Silencer 所在主机，再由该服务完成脱敏。跨主机连接代理端口时，要单独通过 TLS 反向代理或可信内网保护 `5802`；管理面板的 HTTPS 设置不会为代理端口加密。代理不可用时默认返回 503，不会自动绕过脱敏直连上游；只有在高级设置中明确选择透明直连才会放行明文。请自行确认目标站点、第三方中转服务及网络设备的隐私政策。不要把生产密钥交给不受信任的服务。

## doctor 与故障排查

Docker 部署可查看 `docker compose ps` 和 `docker compose logs -f silencer`；检查面板和代理端口连通性，并确认 `/data` 可写。宿主机诊断脚本可在源码目录运行 `./scripts/silencer-doctor.sh --json`。停用脱敏时，服务的代理端口会按配置阻断或明确报告未保护状态，不应默认当作安全直连。

## 开发与发布检查

修改后可以运行完整门禁，覆盖 Python、前端、扩展、版本和打包检查：

```bash
python scripts/verify-all.py
git diff --check
```

当前版本为 `0.0.2`。Docker 使用多阶段构建前端，不需要把 `frontend/dist` 提交到仓库；发布前请确认 `/data`、数据库密码和运行日志中没有真实凭据。

## 开源许可

Silencer 原创代码按 [GNU Affero 通用公共许可证第 3 版](LICENSE)（AGPL-3.0-only）授权。字体、图标和依赖的许可归属见 [第三方许可说明](THIRD_PARTY_NOTICES.md) 及 `licenses/`。
