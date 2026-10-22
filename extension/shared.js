/* Silencer Browser Bridge — 扩展内共享常量与纯函数（**唯一来源**）
 *
 * 为什么单独成文件：这三样东西原先在 background.js / options.js / popup.js 里各抄了一份：
 *   - `STATIC_SITES`（3 份）、`UNSUPPORTED_REASON`（2 份）、
 *   - `normalizeDomain` / `siteMatchPattern`（options 一套、background 一套）。
 * 抄袭的代价不是"多打几个字"，而是**漂移后无声地自相矛盾**：popup 说「已生效」、
 * options 标「路径未适配」；或者 options 用 `*://127.0.0.1/*` 申请权限（Chrome 直接拒绝），
 * 而 background 注册时用 `http://` —— 用户看到"添加成功"但实际不生效。
 * matchPattern 的 IP/localhost 分支就因为这种漂移出过一次缺陷（SPEC v2.7 记录）。
 *
 * 加载方式：
 *   - SW：`background.js` 顶部 `importScripts('shared.js')`（classic service worker）；
 *   - 扩展页面：`<script src="shared.js"></script>` 必须在各自的 logic 脚本**之前**。
 * 两处都把结果挂到 `self.SILENCER_SHARED`，不污染 `window` 命名空间。
 *
 * 本文件必须是**纯常量 + 纯函数**：不碰 chrome.*、不发请求、不读 storage。
 */
'use strict';

(function (root) {
  /** manifest 静态 content_scripts 已覆盖的站点。改动必须与 manifest.json 同步。 */
  // 当前版本支持且默认启用的文字对话站点（必须与 manifest.json 的 matches 一致）。
  // **必须与 manifest.json 的 content_scripts.matches 一致**：check-extension 会双向比对，
  // 改一边忘一边就会出现「界面显示已启用、实际脚本没注入」的静默失效。
  // 当前公开支持 ChatGPT、Gemini、DeepSeek 的文字对话；附件和多媒体不在覆盖范围。
  const STATIC_SITES = ['chatgpt.com', 'gemini.google.com', 'deepseek.com'];

  // Request-path fixtures are not live-site verification. UI must distinguish
  // enabled injection, a codec test, and a protected real browser request.
  const WEBCHAT_SITES = [
    { id: 'chatgpt', name: 'ChatGPT', domains: ['chatgpt.com'], request: 'JSON', response: 'SSE / JSON' },
    { id: 'gemini', name: 'Gemini', domains: ['gemini.google.com'], request: 'f.req / JSON', response: 'JSON / UTF-16 length frames' },
    { id: 'deepseek', name: 'DeepSeek', domains: ['deepseek.com'], request: 'JSON', response: 'SSE / JSON patch' },
  ];
  function webchatSupport(host) {
    const h = String(host || '').toLowerCase();
    return WEBCHAT_SITES.find((p) => p.domains.some((d) => h === d || h.endsWith('.' + d))) || null;
  }
  const PRESET_SITES = [
    // ── 当前版本支持（与 STATIC_SITES 一致，改这里必须同步 manifest.json） ──
    { domain: 'chatgpt.com', zh: 'ChatGPT', en: 'ChatGPT', verified: true },
    { domain: 'gemini.google.com', zh: 'Google Gemini', en: 'Google Gemini', verified: true },
    { domain: 'deepseek.com', zh: 'DeepSeek', en: 'DeepSeek', verified: true },
  ];

  /**
   * 不在推荐列表里的**历史/备用域名**：用户手输时也要按「未实测」标注，
   * 否则会与推荐列表里同一服务的另一个域名给出两套判据（SPEC 反复强调的同源问题）。
   */
  const LEGACY_UNVERIFIED = [];

  const UNVERIFIED_DOMAINS = PRESET_SITES
    .filter((p) => !p.verified)
    .map((p) => p.domain)
    .concat(LEGACY_UNVERIFIED);

  /**
   * 路径规则未实测的站点：允许添加，但必须标注 + 添加前提示。
   * popup 需要它来判断「本页是否生效」，options 需要它来打标签 —— 两处判据必须同源。
   * 由 PRESET_SITES 推导，不再手写，避免两份清单漂移。
   */
  const UNSUPPORTED_REASON = {};
  for (const d of UNVERIFIED_DOMAINS) UNSUPPORTED_REASON[d] = 'paths';

  /**
   * 域名 → Chrome match pattern。
   *
   * **必须是已授权 host 权限的子集**：manifest 对 `127.0.0.1` / `localhost` 只预授权
   * `http://`（SPEC §3.1），所以它们只能用 `http://…/*`；写成 `*://…/*` 会被 Chrome 拒绝
   * （`chrome.permissions.contains` 直接返回 false，动态注册静默失效）。
   * 真实站点走 options 页的手势授权 `*://*.<domain>/*`，申请与注册两边必须同款。
   */
  function siteMatchPattern(domain) {
    const d = String(domain || '').trim().toLowerCase();
    if (!d) return '';
    const local = /^\d{1,3}(\.\d{1,3}){3}$/.test(d) || d.includes(':') || d === 'localhost';
    return local ? `http://${d}/*` : `*://*.${d}/*`;
  }

  /** 域名校验：只允许 `[a-z0-9.*-]`（SPEC §3.5(5)），首尾不允许点。 */
  function normalizeDomain(raw) {
    const d = String(raw || '').trim().toLowerCase();
    if (!d) return '';
    if (!/^[a-z0-9.*-]+$/.test(d)) return '';
    if (d.startsWith('.') || d.endsWith('.')) return '';
    return d;
  }

  /** 本页域名是否被某个已启用站点覆盖（含子域）。 */
  function siteCovers(host, sites) {
    const h = String(host || '').toLowerCase();
    if (!h) return '';
    for (const s of sites || []) {
      const d = String(s || '').toLowerCase();
      if (d && (h === d || h.endsWith('.' + d))) return d;
    }
    return '';
  }

  /** 面板地址必须是没有路径、查询或凭据的 HTTP(S) URL。 */
  const PANEL_URL_RE = /^https?:\/\/[^\/?#\s]+$/i;

  /** panelUrl 是否指向本机，用于给远程地址显示风险提示。 */
  function isLocalPanelUrl(url) {
    try {
      const parsed = new URL(String(url || ''));
      return PANEL_URL_RE.test(String(url || '')) &&
        (parsed.hostname.toLowerCase() === 'localhost' || parsed.hostname === '127.0.0.1');
    } catch {
      return false;
    }
  }

  function isPanelUrl(url) {
    try {
      const parsed = new URL(String(url || ''));
      return PANEL_URL_RE.test(String(url || '')) &&
        (parsed.protocol === 'http:' || parsed.protocol === 'https:') &&
        Boolean(parsed.hostname) && !parsed.username && !parsed.password;
    } catch {
      return false;
    }
  }

  /**
   * 该域名（或它所属的已启用站点）是否有「路径未实测」标注；无则返回 ''。
   *
   * 除精确命中外还认**子域**：`UNSUPPORTED_REASON` 只列顶域（推荐站点也只写顶域），
   * 而用户可能手输 `chat.deepseek.com`、`www.doubao.com` 这类具体主机 —— 不认子域就会
   * 给出「已适配」的错判，popup 显示「已生效」而实际未脱敏（最危险的一种失败）。
   */
  function unsupportedReason(domain, coveredSite) {
    for (const raw of [domain, coveredSite]) {
      const d = String(raw || '').toLowerCase();
      if (!d) continue;
      if (UNSUPPORTED_REASON[d]) return UNSUPPORTED_REASON[d];
      for (const u of UNVERIFIED_DOMAINS) {
        if (d.endsWith('.' + u)) return 'paths';
      }
    }
    return '';
  }

  /**
   * 扩展实现的 `/api/ext/*` 协议版本。
   *
   * 必须与 Go 网关 `/api/ext/ping` 的 `ext_protocol` 保持一致。
   * 支持的版本，SW 比对不一致就在 popup 与图标角标上报警。
   *
   * 【为什么不比产品版本号】扩展 `manifest.version`（1.0.0）与客户端 `version`
   * 是两条独立的发布节奏，**从来就不同步**，拿它比必然误报。这个整数**只在接口契约**
   * （`/api/ext/*` 的请求/响应字段或语义）变化时 +1：产品发版、UI 调整、内部重构一律不动。
   *
   * 【为什么必须有】没有它，客户端改了契约而用户没重载扩展时，扩展会**静默失效**
   * —— 页面毫无异常，用户只看到「怎么不脱敏了」，且没有任何地方能归因。
   */
  const EXT_PROTOCOL_VERSION = 3;

  root.SILENCER_SHARED = {
    STATIC_SITES, PRESET_SITES, WEBCHAT_SITES, webchatSupport, UNSUPPORTED_REASON, EXT_PROTOCOL_VERSION,
    siteMatchPattern, normalizeDomain, siteCovers, unsupportedReason,
    PANEL_URL_RE, isLocalPanelUrl, isPanelUrl,
  };
})(typeof self !== 'undefined' ? self : this);
