# Silencer 前端

React 19 + TypeScript + Vite + Tailwind CSS + Radix UI primitives。这里是 Silencer 自托管服务的 Web 控制台，提供实例配置、上游路由、客户端接入、日志和安全审计界面；构建产物由 Go 网关在 Docker 或源码部署中托管。

```bash
npm ci
npm run dev        # http://localhost:5173，开发 API 默认指向运行中的 Silencer 8080（可按需配置代理）
npm run build      # tsc -b + vite build → dist/
npm run lint       # oxlint
node ../scripts/check-i18n.mjs   # zh / en 字典对齐
```

需要 Node.js 22.12 或更新版本。浏览器开发时可设置 `VITE_SILENCER_TOKEN` 自动登录，该变量仅在 Vite 开发构建（`import.meta.env.DEV`）读取。生产/Docker 通过登录页使用管理账号和部署时配置的密码登录。

约定：

- 所有请求走 `src/lib/silencer-fetch.ts`，不直接 `fetch`；
- 用户可见文案一律 `t()` / `tf()`，key 同时写入 `src/lib/i18n.tsx` 的 zh 与 en；
- 新 UI 用 Tailwind 类 + `src/components/ui` 语义组件，颜色走 CSS 变量以兼容深浅主题。
- 页面文案必须同时补齐 `zh` / `en`；`node ../scripts/check-i18n.mjs` 会检查字典键和静态引用。
- 生产构建不接受前端内嵌令牌；浏览器部署使用面板登录页和 HttpOnly 会话 Cookie。
- 视觉改动至少验证宽屏和窄视口的无横向溢出、加载/空态/错误态及键盘焦点。
