# 官网与演示环境

[官网](https://silencer.5970000.xyz/)和[在线演示](https://silencer.5970000.xyz/demo/)使用两个独立的服务。`public/` 是官网的 HTML、CSS 和 JavaScript；`/demo/` 转发到 Go + C++ 网关自带的控制台。官网支持中英文及明暗主题，并展示 1.1 捕获模式、三端安装包、兼容边界与更新日志。下载链接固定到对应 Release；升级官网时同步版本、下载链接、文案与 CSS/JS 缓存版本。

## 本地预览官网

在仓库根目录运行：

```bash
python3 -m http.server 8081 --bind 127.0.0.1 --directory website/public
```

打开 <http://127.0.0.1:8081/>。静态预览不包含 `/demo/` 或控制 API，完整演示需要下面的独立网关和 Nginx 配置。

## 启动独立演示实例

演示使用单独的数据卷和管理密码，不挂载生产数据、客户端配置或宿主机代理。以下命令从源码构建，适用于 AMD64、ARM64 原生构建环境。也可以直接使用 Docker Hub 的 `liuarui/silencer:1.1.0`，该标签会自动选择 AMD64 或 ARM64。

在仓库根目录运行：

```bash
docker build -t silencer:demo .
export SILENCER_DEMO_PASSWORD="$(openssl rand -hex 24)"
docker run -d --name silencer-demo --restart unless-stopped \
  -p 127.0.0.1:5803:8080 \
  -v silencer_demo_data:/data \
  -e SILENCER_GATEWAY_USERNAME=demo \
  -e SILENCER_GATEWAY_TOKEN="$SILENCER_DEMO_PASSWORD" \
  -e SILENCER_CONTROL_CONFIG_FILE=/data/control.json \
  -e SILENCER_REQUIRE_UPSTREAM=false \
  -e SILENCER_STORAGE_MODE=sqlite \
  -e SILENCER_STORE_RAW_BODY=false \
  -e SILENCER_EXTENSION_DISABLED=1 \
  silencer:demo
curl --fail --retry 20 --retry-connrefused --retry-delay 1 \
  http://127.0.0.1:5803/readyz
```

账号是 `demo`，密码是 `SILENCER_DEMO_PASSWORD`。官网 HTML 中的 `SET_YOUR_DEMO_PASSWORD` 是占位值：部署自己的官网时，改为自己的演示账号密码，或移除密码展示并另行分发。不要使用生产密码。

该配置没有模型上游，可以体验控制台、规则和脱敏测试。演示账号具有管理权限，仍可修改规则和配置，**不是只读账号**。不要向公共演示输入真实凭据或个人数据，也不要连接生产上游。详细日志的显示掩码不会删除数据库中的原文；需要时在设置中关闭详细数据记录并制定数据清理计划。

## 部署官网和反向代理

以下示例使用 Debian/Ubuntu 的 Nginx 目录约定。先安装 Nginx，再执行：

```bash
sudo install -d /var/www/silencer
sudo install -m 0644 website/public/index.html website/public/site.js \
  website/public/styles.css /var/www/silencer/
sudo install -m 0644 website/nginx.conf.example \
  /etc/nginx/sites-available/silencer-website
# 将配置中的 silencer.example.com 改为自己的域名，并配置公开入口的 HTTPS。
sudo ln -s /etc/nginx/sites-available/silencer-website \
  /etc/nginx/sites-enabled/silencer-website
sudo nginx -t
sudo systemctl reload nginx
```

先在 HTTPS 入口配置有效证书，再对外开放登录。示例的 HTTP 监听可放在已有的可信 HTTPS 代理之后；按实际代理拓扑设置协议头和真实来源地址，不要直接信任任意客户端提交的转发头。

路由说明：

| 路径 | 用途 |
| --- | --- |
| `/`、`/styles.css`、`/site.js` | Nginx 提供官网静态文件 |
| `/demo/` | 去掉前缀后转发到独立演示控制台 |
| `/assets/`、`/favicon.svg` | 演示控制台使用的根路径资源 |
| `/capture-assets/` | 捕获向导的安装脚本下载，转发到演示网关 |
| `/api/` | 演示控制 API 及默认模型代理入口 |
| `/r/` | 演示的命名上游代理入口 |
| `/api/track` | 兼容页面的访问上报请求，直接返回 204；示例不收集访问数据 |

网关路由允许最大 64 MiB 的请求，关闭请求和响应缓冲，并延长流式连接的读写超时。应用自身的并发、响应大小和上游超时限制仍然有效。

以下两个改密入口在 Nginx 层直接返回 `403` 和 `demo_password_change_disabled`，不会交给网关处理：

- `/api/auth/change-password`
- `/demo/api/auth/change-password`

限制只作用于使用此示例的演示站点，不改变网关产品的修改密码功能。演示容器端口绑定到 `127.0.0.1`，对外访问统一经过 Nginx；控制台仍可能显示修改密码按钮，但提交会被拒绝。

## 更新演示实例

官网更新时，只替换 `/var/www/silencer/` 中的三个静态文件。网关更新时，先在独立测试卷验证新镜像，再使用原有数据卷、账号、密码及环境设置替换演示容器。不要为现有实例重新生成密码，也不要删除其数据卷。

升级后检查 `http://127.0.0.1:5803/readyz`（或经反代访问 `/demo/readyz`）、`/demo/` 的页面资源、原账号登录和脱敏测试；确认两个改密入口继续返回 403。Nginx 配置变动必须先通过 `nginx -t` 再 reload。升级到 1.1 时补齐 `/capture-assets/` 转发，并验证 `/demo/#/capture` 的向导与 `/capture-assets/install.sh`、`/capture-assets/install.ps1` 下载；已有站点应合并这一条路由，保留原有采集器、限流与访问来源配置。
