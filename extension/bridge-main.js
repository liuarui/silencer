/* Silencer Browser Bridge — MAIN world hook 层
 *
 * 跑在**页面上下文**（world:"MAIN", run_at:"document_start"，实测先于页面脚本 hook 成功）。
 * 这里没有 chrome.* 特权，所有特权操作经 postMessage（带 nonce）转给 ISOLATED 层。
 *
 * ⚠️ MAIN world 的一切都视为**可被页面读取**：token 绝不经过这里，请求/响应原文也不外发，
 *    只把文本交给扩展侧打码/还原（打码只进不出）。
 */
(() => {
  'use strict';
  if (window.__SILENCER_BRIDGE__) return;
  window.__SILENCER_BRIDGE__ = true;

  // Non-authoritative page diagnostics: no tokens, bodies or extension settings.
  // This lets a developer distinguish missing injection from an unmatched path.
  const diagnostic = { build: 'webchat-1.1.2', bridge: 'starting', unmatched: [] };
  const publishDiagnostic = (patch) => {
    Object.assign(diagnostic, patch);
    try {
      if (typeof document === 'undefined' || !document.documentElement) return;
      let tag = document.querySelector('meta[name="silencer-bridge-diagnostic"]');
      if (!tag) { tag = document.createElement('meta'); tag.name = 'silencer-bridge-diagnostic'; document.documentElement.appendChild(tag); }
      tag.content = JSON.stringify(diagnostic);
    } catch (_error) { /* diagnostics never affect the network */ }
  };
  publishDiagnostic({});
  if (typeof document !== 'undefined') document.addEventListener('DOMContentLoaded', () => publishDiagnostic({}), { once: true });
  const noteResponseError = (error) => {
    const code = String(error?.message || '').replace(/^Silencer: /, '');
    publishDiagnostic({ response: /^[a-z_]{1,80}$/.test(code) ? code : 'stream-error', ...(error?.silencerDiagnostic ? { codec: error.silencerDiagnostic } : {}) });
  };
  const noteRequest = (url, matched) => {
    try {
      const target = new URL(url, location.href);
      const source = window.SILENCER_WEBCHAT?.providerFor(location.hostname);
      if (!source || window.SILENCER_WEBCHAT.providerFor(target.hostname)?.id !== source.id) return;
      if (source.id === 'gemini' && target.pathname.endsWith('/batchexecute')) {
        const methods = (target.searchParams.get('rpcids') || '').split(',').filter(Boolean).map((id) => ['hNvQHb', 'MaZiqc', 'EqPOKe'].includes(id) ? id : 'other');
        publishDiagnostic({ geminiRpc: [...new Set(methods)].slice(0, 6) });
      }
      const path = target.pathname.split('/').map((part) => part.length > 48 || /^[0-9a-f-]{16,}$/i.test(part) ? ':id' : part).join('/').slice(0, 200);
      if (matched) publishDiagnostic({ matched: path });
      else publishDiagnostic({ unmatched: [...new Set([...diagnostic.unmatched, path])].slice(-6) });
    } catch (_error) { /* invalid/foreign URL */ }
  };

  const BRIDGE_TIMEOUT_MS = 3000;
  // 文本 body 的**下限**：短于它连一次桥都不值得走（审计 M4）。
  //
  // 原值是 80，理由是「滤掉 GET 语义的查询串」。但那个理由早已不成立——
  // 上面的 fetch hook 现在**先按方法过滤**（只放行 POST/PUT/PATCH），GET 查询串根本到不了这里。
  // 于是 80 只剩下一个效果：≤80 字符的写请求**不打码、不记录、无提示**，
  // 与 multipart 分支（无长度阈值）自相矛盾，属「静默未脱敏」。
  //
  // 取 8 而不是 0：短于 8 字符的 body（`{}`、`{"a":1}`、abort 请求）不可能承载任何
  // 现实中的敏感值——最短的现实 PII 是 11 位手机号 / 18 位身份证 / 9 字符邮箱，
  // 全都在 8 之上。保留这个下限只为省掉这些必然无命中的桥调用，
  // 而**它不再能掩盖任何有意义的请求**。
  const MIN_MASKABLE_LEN = 8;

  // 单份 Office 附件的原始字节上限，**必须与引擎侧闸门对齐**（审计 M5）。
  //
  // `panel._EXT_MAX_BODY = 32MB` 是请求体（JSON 信封）上限，而整份文件要 base64 后
  // 塞进去，膨胀 4/3 —— 原值 25MB 恰好卡在边界之上：25MB 原始文件 ≈ 33.3MB 请求体
  // → 413 + `blocking:true` → 扩展按 (A) 类无条件阻断 → 页面**网络错误**
  // （不是降级直通）。用户传一个 24–25MB 的 docx 会看到整页请求失败。
  //
  // 反推：`4 * ceil(n/3) + 信封 ≈ 32MB` → n 上限约 24.0MB。这里取 23MB，
  // 留约 1.4MB 余量给文件名/headers 等开销。超限的文件会落到 `kind:'raw'` 分支
  // **原样透传并由 `attachment` note 诚实上报「未脱敏 N 个」**——
  // 页面能用，用户也知道它没被保护。这比「硬失败」和「静默放行」都好。

  // ─── postMessage 桥（nonce 校验） ───
  const pending = new Map();
  const bridge = {
    call(action, payload, timeoutMs = BRIDGE_TIMEOUT_MS) {
      return new Promise((resolve) => {
        const nonce = (crypto.randomUUID && crypto.randomUUID()) || String(Math.random()).slice(2);
        const timer = setTimeout(() => {
          pending.delete(nonce);
          resolve(null);                       // 超时按 (B) 直通处理
        }, timeoutMs);
        pending.set(nonce, { resolve, timer });
        window.postMessage({ type: 'SILENCER_BRIDGE_REQ', nonce, action, payload }, location.origin);
      }).then((result) => {
        // 【桥健康度记账】唯一能观测「扩展是否还在」的信号就是这里：重载/更新扩展会销毁
        // 本页的 content script 上下文，ISOLATED 侧与 SW 都换了新的 —— MAIN 侧再
        // postMessage 就没人应答，一路超时返回 null。连续多次拿不到结果就认定桥断了
        // （处置见 restoreNativeAndStop）。拿 callback 包在这里而不是两个 resolve 处，
        // 是为了让超时与正常响应两条分支天然共用一条记账路径。
        noteBridgeCall(result != null);
        return result;
      });
    },
    notify(action, payload) {
      window.postMessage({ type: 'SILENCER_BRIDGE_REQ', nonce: null, action, payload }, location.origin);
    },
  };

  // ─── mask 的专用调用：更长超时 + 一次重试 ───
  //
  // `mask` 是整条链路上**唯一**「桥超时 = 明文出网」的调用：桥一超时，MAIN 侧拿不到
  // `r.sid` 会走 (B) 分支把**原始请求体**发出去（未脱敏），而且**整条响应也不会被还原**
  // （`sid` 为空 → 不包 wrapResponse）。restore 超时只是少还原几个字，代价完全不同。
  //
  // 3s 对 mask 太紧：引擎侧 `/api/ext/*` 走一把全局锁串行（`panel._EXT_LOCK`），
  // 多标签页并发、或 NER 冷启动时，单次 mask 超过 3s 很正常。这里给 6s，并在失败时
  // 重试一次。重试成功的第一个 sid 会在引擎侧成为孤儿会话，由会话 TTL 自然回收。
  const MASK_TIMEOUT_MS = 6000;
  const maskOnce = (text, metadata) => {
    const context = { ...metadata };
    if (context.url) { try { context.url = new URL(context.url, location.href).href; } catch (_error) { /* keep unknown target */ } }
    return bridge.call('mask', { text, ...context }, MASK_TIMEOUT_MS);
  };
  const callMask = async (text, metadata = {}) => {
    // ⚠️ `blocking`（引擎 413 超限 / 503 管线失败 / 400 参数错）是**确定性拒绝**，
    // **绝不重试**：重试只会拿同一份负载再打一次，既慢又白烧。它必须原样上抛给
    // 调用点去 `throw new TypeError('Failed to fetch')`，走 fail-closed
    // —— 宁可页面报网络错误，也绝不放行未脱敏明文出网。
    const first = await maskOnce(text, metadata);
    if (first && first.blocking) return first;
    if (first) return first;
    // 桥超时/失败 → 重试一次（覆盖引擎全局锁排队与 NER 冷启动的偶发超时）。
    return await maskOnce(text, metadata);
  };
  const webchat = window.SILENCER_WEBCHAT;
  const maskWebText = async (text, metadata) => {
    const provider = webchat && webchat.match(metadata.url, location.href);
    if (!provider) return callMask(text, metadata);
    const config = await getWideMode();
    if (config.siteEnabled === false) return { ok: false, passthrough: true };
    let issuedSid = '';
    try {
      return await webchat.maskRequest(provider, text, async (value) => {
        const result = await callMask(value, metadata);
        issuedSid = result && result.sid;
        return result;
      });
    } catch (error) {
      if (issuedSid) void notifyFinal(issuedSid, 'mask_not_applied');
      bridge.notify('note', { kind: 'unsupported_protocol', provider: provider.id });
      return { ok: false, blocking: true, error: error.message };
    }
  };
  window.addEventListener('message', (e) => {
    if (e.origin !== location.origin) return;
    if (!e.data || e.data.type !== 'SILENCER_BRIDGE_RESP') return;
    const p = pending.get(e.data.nonce);
    if (p) {
      clearTimeout(p.timer);
      pending.delete(e.data.nonce);
      p.resolve(e.data.result);
    }
  });

  // ─── 拦截策略：广泛模式（默认，零适配）/ 窄模式（只认对话接口） ───
  //
  // 【为什么从白名单改成黑名单】原方案要求路径命中 LLM_PATH_HINTS 才打码，于是每加一个
  // 新站点都要先猜接口路径——猜错就**整站静默未脱敏**（页面毫无异常，内容裸着发出去）。
  // 2026-09-16 实测 18 站，有 4 个海外站就是这样漏掉的（Gemini/Grok/Perplexity/Poe 的
  // 路径一个关键词都不带）。逐个补路径是打地鼠：站点改版一次就失效一次。
  //
  // 广泛模式的判定是三层：
  //   ① 认证/埋点/噪声路径 → 永不打码（黑名单）
  //   ② 命中对话接口形态   → 一定打码（**跨域也算**，兜住 API 在独立域的站点）
  //   ③ 其余同站 POST      → 也送引擎（广泛模式独有；引擎没匹配到敏感信息就原样返回）
  //
  // 同站限定是关键：埋点几乎全是跨域（clarity.ms / sentry.io / volces.com / 百度统计…），
  // 一条「不同站就不碰」就能滤掉绝大部分噪声，而 AI 站点的对话请求基本都在本站或子域。
  const LLM_PATH_HINTS = [
    /\/api\/.*conversation/i,
    /\/backend-api\//i,
    /\/(?:v[0-9]+\/)?(?:chat|completions?|messages|responses)/i, // completions? 兼容单复数，覆盖 Claude/DeepSeek/Kimi 等
    /\/samantha\/chat\//i,                                        // 豆包（Doubao）
    /\/api\/(?:v[0-9]+\/)?chat\//i,                               // Kimi / 通义千问等
    /\/_\/BardChatUi\//i,   // Google Gemini（含 StreamGenerate / batchexecute）
    /\/app-chat\//i,        // Grok（xAI）：/rest/app-chat/conversations/…
    /\/rest\/sse\//i,       // Perplexity：/rest/sse/perplexity_ask
    /\/api\/gql/i,          // Poe：GraphQL 端点 /api/gql_POST
    // ── 文件/附件上传端点（ChatGPT / Claude / 各主流 Web AI / 豆包） ──
    /\/(?:api|backend-api|alice|v[0-9]+)\/.*(?:upload|files?|attachment|resource|convert|doc)/i,
  ];

  /**
   * 永不打码的路径黑名单。
   *
   * ① 认证类：EMAIL/PHONE 是**默认开启**的内置规则，登录表单里 `{"email":"a@b.com"}`
   *    会被打成 `{{EMAIL_xxxxxx}}`（实测），一旦打码就登录不上——这是硬失败，必须排除。
   * ② 埋点/遥测类：不是内容，送引擎纯属浪费，且占位符混进埋点会造成数据污染。
   * ③ 只读类（history/list/detail）：不带用户新输入，没有打码价值。
   *
   * 黑名单命中就**直接放行**，不再走引擎——宁可少打一次码，也不能让登录表单坏掉。
   */
  const NEVER_MASK_PATH = [
    /^\/backend-(?:api|anon)\/(?:f\/)?(?:sentinel(?:\/|$)|composer\/items\/interactions(?:\/|$)|conversation\/init(?:\/|$))/i,
    // 认证 / 凭据
    /(login|signin|sign-?in|signup|sign-?up|register|logout|auth|oauth|sso|session|token|password|credential|passwd|captcha|verify|verification|sms|otp|2fa|mfa)/i,
    // 埋点 / 遥测 / 监控
    /(\/|_|-|\.)(log|logs|logging|logger|beacon|telemetry|track|tracking|tracker|analytics|metric|metrics|monitor|monitoring|report|reports|collect|collector|event|events|stats|stat|perf|performance|apm|rum|sentry|clarity|error|errors|exception|crash|heartbeat|health|healthcheck|alive|ping|feedback|survey|abtest|ab-?test|experiment)($|\/|_|-|\.|\?|\d)/i,
    // 配置 / 静态字典（不含用户输入）
    /(\/|_|-|\.)(config|configs|settings|preference|preferences|i18n|locale|lang|dict|theme|version|update|upgrade|announce|notice|notification|invite|referral|share|balance|quota|billing|payment|order)($|\/|_|-|\.|\?|\d)/i,
  ];

  /** 同站判定：完全同 host，或互为子域（api.doubao.com ↔ www.doubao.com）。 */
  const isSameSite = (host) => {
    const h = String(host || '').toLowerCase();
    const p = location.hostname.toLowerCase();
    if (!h || !p) return false;
    if (h === p) return true;
    const provider = webchat && webchat.providerFor(p);
    if (provider && provider.domains.some((d) => (h === d || h.endsWith('.' + d)) && (p === d || p.endsWith('.' + d)))) return true;
    return h.endsWith('.' + p) || p.endsWith('.' + h);
  };

  /**
   * 是否该对这次请求打码。
   * 先判白名单（命中直接打码，防止 events/settings 误杀真实对话接口）；
   * 再判黑名单（排除认证/埋点/配置等非对话管理接口）；
   * 兜底：广泛模式下同站其余 POST 送引擎，默认精准模式下直接放行。
   */
  const shouldMaskUrl = (url, wide) => {
    let u, path;
    try {
      u = new URL(url, location.href);
      path = u.pathname;
    } catch (e) {
      return false;
    }
    if (webchat && webchat.match(url, location.href)) return true;
    if (NEVER_MASK_PATH.some((rx) => rx.test(path))) return false; // 认证/埋点/管理排除
    if (webchat && webchat.providerFor(location.hostname)?.id === 'gemini') return false;
    if (LLM_PATH_HINTS.some((rx) => rx.test(path))) return true;
    return !!wide && isSameSite(u.hostname);
  };

  // 广泛模式开关由 SW 下发（chrome.storage 在 MAIN world 不可用）。默认 false（精准对话模式）。
  // 且**只问一次**并缓存——每个请求都问一次 storage 不划算，而这个值改了刷新页面就生效。
  let wideModePromise = null;
  const getWideMode = async () => {
    if (!wideModePromise) {
      wideModePromise = bridge.call('config', {})
        .then((r) => {
          publishDiagnostic({ bridge: r && typeof r.wideMode === 'boolean' ? r.siteEnabled === false ? 'site-disabled' : 'ready' : 'unavailable' });
          return r && typeof r.wideMode === 'boolean' ? { wideMode: r.wideMode, siteEnabled: r.siteEnabled } : null;
        })
        .catch(() => null);
    }
    const res = await wideModePromise;
    if (!res) {
      wideModePromise = null;          // 拿不到就重试，不把「未知」固化成「精准模式」
      return { wideMode: false };      // 本次仍按精准模式（保守），但不写进缓存
    }
    return res;
  };

  // ─── 可打码 body 判定（成对原则的落地点） ───
  // Request 的 body 可能是 FormData / Blob / URLSearchParams / ReadableStream —— 把它
  // clone().text() 读成字符串再 new Request(old, {body: 字符串}) 回写，会保留原来的
  // `Content-Type: multipart/form-data; boundary=…` 却把实体换成纯文本，上游直接 400。
  // 所以文本分支只吃文本类 content-type；multipart 走**独立的 FormData 分支**（见
  // maskMultipart），不再像 v1 那样整体跳过——跳过的代价是「带附件的请求完全不脱敏」。
  const MASKABLE_CT = /^(application\/json|application\/(x-)?ndjson|application\/json-seq|application\/x-www-form-urlencoded|text\/)/i;
  const MULTIPART_CT = /^multipart\/form-data/i;
  const isMaskableBody = (req) => MASKABLE_CT.test((req.headers.get('content-type') || '').trim());
  const isMultipart = (req) => MULTIPART_CT.test((req.headers.get('content-type') || '').trim());

  const initBodyIsText = (init) => !!init && typeof init.body === 'string';
  const initBodyIsURLSearchParams = (init) =>
    !!init && typeof URLSearchParams !== 'undefined' && init.body instanceof URLSearchParams;
  const initBodyIsFormData = (init) =>
    !!init && typeof FormData !== 'undefined' && init.body instanceof FormData;
  const initBodyIsBlob = (init) =>
    !!init && typeof Blob !== 'undefined' && init.body instanceof Blob;
  const initBodyIsArrayBuffer = (init) =>
    !!init && (init.body instanceof ArrayBuffer || (typeof ArrayBuffer !== 'undefined' && ArrayBuffer.isView(init.body)));

  // ─── 文件/存储桶与上传端点判定 ───
  const OBJECT_STORAGE_HOSTS = [
    /\.oaiusercontent\.com$/i,
    /\.s3[.-][a-z0-9-]+\.amazonaws\.com$/i,
    /\.s3\.amazonaws\.com$/i,
    /\.blob\.core\.windows\.net$/i,
    /\.storage\.googleapis\.com$/i,
    /\.aliyuncs\.com$/i,
    /\.myqcloud\.com$/i,
    /\.volces\.com$/i,
    /\.volccdn\.com$/i,
  ];
  const STORAGE_OR_UPLOAD_HOSTS = [
    ...OBJECT_STORAGE_HOSTS,
    /claude\.ai$/i,
    /chatgpt\.com$/i,
    /doubao\.com$/i,
    /deepseek\.com$/i,
  ];

  const isUploadOrStorageUrl = (url) => {
    let u;
    try {
      u = new URL(url, location.href);
    } catch (e) {
      return false;
    }
    if (STORAGE_OR_UPLOAD_HOSTS.some((rx) => rx.test(u.hostname))) return true;
    if (LLM_PATH_HINTS.some((rx) => rx.test(u.pathname))) return true;
    return false;
  };

  // A Request with a JSON/text body is also readable as a Blob. Treat it as
  // an attachment only at an upload endpoint, not every AI-site endpoint.
  const isDirectFileUploadUrl = (url) => {
    try {
      const parsed = new URL(url, location.href);
      return OBJECT_STORAGE_HOSTS.some((rx) => rx.test(parsed.hostname))
        || /\/[^/]*(upload|files?|attachments?|resource)[^/]*(\/|$)/i.test(parsed.pathname);
    } catch { return false; }
  };

  /** 是否该对文件/附件类请求打码（包含跨域对象存储桶直传）。 */
  const shouldMaskFileUrl = (url, wide) => {
    return shouldMaskUrl(url, wide) || isUploadOrStorageUrl(url);
  };

  /**
   * 这次 init 的 body 是否会真的被读一遍（而不是直接丢下不管）。
   *
   * 用途只有一个：init 路径（`fetch(url, { body })`）下，判断「URL 命中对话白名单，
   * 但我们压根没读过这个 body」—— 那就是无感知漏脱敏，必须留痕。
   * 注意 Blob 的门槛是 isUploadOrStorageUrl（与 maskSingleFile 的调用条件同源）：
   * 即使在 LLM 路径上，maskSingleFile 也会实际读取并判定它，属于「处理过」，不算静默；
   * 真正没被读过的只有未枚举的形态（ReadableStream / Document…）。
   */
  const initBodyWillBeHandled = (init, url) =>
    !!init && (
      typeof init.body === 'string'
      || (typeof URLSearchParams !== 'undefined' && init.body instanceof URLSearchParams)
      || (typeof FormData !== 'undefined' && init.body instanceof FormData)
      || (typeof Blob !== 'undefined' && init.body instanceof Blob && isUploadOrStorageUrl(url))
      || initBodyIsArrayBuffer(init)
    );

  const TEXT_FILE_EXTS = new Set([
    'txt', 'text', 'md', 'markdown', 'mdown', 'csv', 'tsv', 'json', 'jsonl', 'ndjson',
    'xml', 'yaml', 'yml', 'py', 'pyw', 'js', 'mjs', 'cjs', 'jsx', 'ts', 'mts', 'cts', 'tsx',
    'html', 'htm', 'xhtml', 'css', 'scss', 'sass', 'less', 'sql', 'sh', 'bash', 'zsh',
    'bat', 'cmd', 'ps1', 'psm1', 'c', 'h', 'cpp', 'hpp', 'cc', 'hh', 'cxx', 'hxx',
    'cs', 'java', 'kt', 'kts', 'go', 'rs', 'php', 'rb', 'lua', 'r', 'swift', 'm', 'mm',
    'log', 'env', 'ini', 'conf', 'cfg', 'toml', 'properties', 'diff', 'patch', 'vue', 'svelte',
  ]);
  const TEXT_APP_MIMES = new Set([
    'application/json', 'application/x-ndjson', 'application/ndjson', 'application/jsonl',
    'application/xml', 'application/xhtml+xml', 'application/x-yaml', 'application/yaml',
    'application/toml', 'application/sql', 'application/javascript', 'application/x-javascript',
    'application/ecmascript', 'application/x-sh', 'application/x-httpd-php',
    'application/x-latex', 'application/x-tex', 'application/rtf',
  ]);
  const isMaskableTextAttachment = (v) => {
    if (!v || typeof v !== 'object' || !(v.size > 0) || v.size > 8 * 1024 * 1024) return false;
    const name = String(v.name || '').trim();
    if (name.includes('.')) {
      const ext = name.split('.').pop().toLowerCase();
      if (['docx', 'xlsx', 'pptx', 'doc', 'xls'].includes(ext)) return false;
      if (TEXT_FILE_EXTS.has(ext)) return true;
    }
    const type = String(v.type || '').toLowerCase().split(';')[0].trim();
    return type.startsWith('text/') || TEXT_APP_MIMES.has(type);
  };
  const isImageItem = (item) => !!(item && item.value && String(item.value.type || '').startsWith('image/'));
  function alignTextToSize(maskedContent, originalContent, orig) {
    const encoder = new TextEncoder();
    const target = orig && orig.size > 0 ? orig.size : encoder.encode(originalContent).length;
    const size = encoder.encode(maskedContent).length;
    if (size > target) return { content: originalContent, masked: false };
    return { content: maskedContent + ' '.repeat(target - size), masked: maskedContent !== originalContent };
  }

  function resolveFileExt(v, urlHint) {
    if (v && typeof v.name === 'string' && v.name.includes('.')) {
      return v.name.split('.').pop().toLowerCase().trim();
    }
    if (v && typeof v.type === 'string') {
      const mime = v.type.toLowerCase().split(';')[0].trim();
      if (mime === 'text/plain') return 'txt';
      if (mime === 'text/csv') return 'csv';
      if (mime === 'text/markdown') return 'md';
      if (mime === 'application/json') return 'json';
    }
    if (urlHint) {
      try {
        const p = new URL(urlHint, location.href).pathname;
        if (p.includes('.')) return p.split('.').pop().toLowerCase().trim();
      } catch (e) { /* ignore */ }
    }
    return '';
  }

  function arrayBufferToBase64(buffer) {
    const bytes = new Uint8Array(buffer);
    let binary = '';
    const chunkSize = 0x8000;
    for (let i = 0; i < bytes.length; i += chunkSize) {
      binary += String.fromCharCode.apply(null, bytes.subarray(i, i + chunkSize));
    }
    return btoa(binary);
  }

  function base64ToUint8Array(base64) {
    const binary = atob(base64);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return bytes;
  }



  /**
   * multipart 打码：文本字段和常见文本附件打码，其它二进制/图片原样透传。
   *
   * 为什么可以这么做：`Request.formData()` 会把 multipart 解析成 [name, string|File] 条目，
   * 我们用打码后的字符串 + 重新组装的 File 拼一个 FormData，再交给 fetch 自动生成**新的**
   * boundary —— 不存在手写 boundary 写坏的问题。
   *
   * 为什么必须显式删掉 `content-type`：见 `new Request(resource, {body})` 那处注释。
   * 旧头里是**旧 boundary**，留着等于告诉上游按一条已经不存在的分隔线去切分实体 → 400。
   *
   * 文本类附件（.txt/.md/.csv/.py/.json 等）：通过 `file.text()` 读取其纯文本并连同
   * 提示词一起打码，用打码后的文本构造同名 `new File` 替换原文件；
   * 图片与其它格式保持原样透传。
   */
  const FIELD_SEP = '\u0001\u0002__SILENCER_PART__\u0002\u0001';
  async function maskMultipart(fd, metadata) {
    const entries = [...fd.entries()];
    const items = [];
    const texts = [];

    for (const [k, v] of entries) {
      if (typeof v === 'string') {
        items.push({ kind: 'string', key: k, value: v });
        texts.push(v);
      } else if (isMaskableTextAttachment(v)) {
        let content = null;
        try {
          content = await v.text();
        } catch (e) {
          content = null;
        }
        if (typeof content === 'string') {
          items.push({ kind: 'textFile', key: k, file: v });
          texts.push(content);
        } else {
          items.push({ kind: 'raw', key: k, value: v });
        }
      } else {
        items.push({ kind: 'raw', key: k, value: v });
      }
    }

    if (!texts.length) {
      const raws = items.filter((item) => item.kind === 'raw');
      return null;                                  // 没有可打码的文本或文档
    }

    let sid = null;
    let parts = [];
    // 任一字符串字段/附件被真正改写？用于零触碰判定（一个敏感值都没命中就不重建请求）。
    let textChanged = false;
    if (texts.length > 0) {
      const joined = texts.join(FIELD_SEP);
      if (joined) {
        const r = await callMask(joined, metadata);
        if (r && r.blocking) return { blocking: true };  // (A) 无条件阻断，交给调用方抛
        if (r && r.ok) {
          sid = r.sid;
          parts = String(r.masked_text || '').split(FIELD_SEP);
          if (String(r.masked_text || '') !== joined) textChanged = true;
        }
      }
    }

    if (sid && parts.length !== texts.length) {
      void notifyFinal(sid, 'mask_not_applied');
      return null;
    }

    const out = new FormData();
    let textIdx = 0;
    for (const item of items) {
      if (item.kind === 'string') {
        out.append(item.key, parts.length === texts.length ? parts[textIdx++] : item.value);
      } else if (item.kind === 'textFile') {
        const orig = item.file;
        if (parts.length === texts.length) {
          const maskedContent = parts[textIdx];
          const originalContent = texts[textIdx];
          textIdx++;
          // 脱敏后必须与原始文件**等字节数**，否则上游按声明的 file_size 校验会拒收
          // （见 alignTextToSize 注释：这是「Excel / Word 能传、txt 传不了」的根因）。
          const aligned = alignTextToSize(maskedContent, originalContent, orig);
          if (maskedContent !== originalContent && !aligned.masked) {
            void notifyFinal(sid, 'mask_not_applied');
            return null;
          }
          let replacement;
          if (typeof File !== 'undefined' && orig instanceof File) {
            replacement = new File([aligned.content], orig.name, {
              type: orig.type || 'text/plain',
              lastModified: orig.lastModified || Date.now(),
            });
          } else {
            replacement = new Blob([aligned.content], { type: (orig && orig.type) || 'text/plain' });
          }
          out.append(item.key, replacement, (orig && orig.name) || 'attachment.txt');
          // 内容**真的变了**才算已脱敏：引擎没命中敏感词时会原样回传，
          // 那时报成已脱敏，就是把「未保护」包装成「已保护」。
          // 对齐失败（脱敏后反而更长）时 aligned.masked 恒为 false，同样如实计入未脱敏。
          if (aligned.masked) item.masked = true;
        } else {
          out.append(item.key, orig, orig && orig.name);
        }
      } else {
        out.append(item.key, item.value, item.value && item.value.name);
      }
    }

    // 计数只认真正替换成功的文本附件。
    let maskedCount = 0;
    let unmaskedCount = 0;
    let hasUnmaskedImage = false;
    for (const item of items) {
      if (item.kind === 'string') continue;
      if (item.masked) {
        maskedCount++;
      } else {
        unmaskedCount++;
        if (isImageItem(item)) hasUnmaskedImage = true;
      }
    }
    // 零触碰原则（与文本 / Blob 分支一致）：一个敏感值都没命中时**不要重建 FormData**——
    // 重建会换掉 boundary、改变请求字节指纹，对签名敏感的站点是纯风险而收益为零。
    // 返回 null = 调用方原样放行原始请求（顺带也就不存在「需要还原的响应」）。
    // 主域（claude.ai / chatgpt.com …）被整体当作上传域，所以这里能拦下域内**所有**
    // FormData POST，零触碰也因此变成了必要而非可选项。
    if (!textChanged && maskedCount === 0) {
      return sid ? { body: fd, sid, masked: false } : null;
    }
    return { body: out, sid: sid, masked: true };
  }

  /** 单文件文本附件脱敏；Office 和其它二进制文件原样透传。 */
  async function maskSingleFile(blob, urlHint, method) {
    if (!blob || typeof Blob === 'undefined' || !(blob instanceof Blob)) return null;

    const ext = resolveFileExt(blob, urlHint);
    const isText = isMaskableTextAttachment(blob);

    if (!isText) {
      return null;
    }

    const filename = (blob && typeof blob.name === 'string' && blob.name)
      || (ext ? `attachment.${ext}` : 'attachment.bin');
    // 二进制和 Office 文件原样放行；仅处理纯文本附件。
    // 文本类附件处理分支
    if (isText) {
      let sid = null;
      try {
        const content = await blob.text();
        if (content && content.length > 0) {
          const r = await callMask(content, { url: urlHint, method });
          sid = r && r.sid;
          if (r && r.blocking) return { blocking: true };
          if (r && r.ok && typeof r.masked_text === 'string') {
            // 无命中时引擎原样回传：不重建请求（零触碰）、不报「已脱敏」。
            if (r.masked_text === content) {
              return { body: blob, sid: r.sid, masked: false };
            }
            // 与 multipart 分支同一套对齐语义：脱敏后字节数必须与原始文件一致，
            // 否则上游按声明的 file_size 校验会拒收（见 alignTextToSize 注释）。
            const aligned = alignTextToSize(r.masked_text, content, blob);
            // 对齐失败（脱敏后反而更长）：宁可不上码，也不能让用户的文件传不上去。
            // 这里返回 null = 完全不碰请求，站点看到的是原始文件，与「未脱敏」一致。
            if (!aligned.masked) {
              void notifyFinal(r.sid, 'mask_not_applied');
              return null;
            }
            let replacement;
            if (typeof File !== 'undefined' && blob instanceof File) {
              replacement = new File([aligned.content], filename, {
                type: blob.type || 'text/plain',
                lastModified: blob.lastModified || Date.now(),
              });
            } else {
              replacement = new Blob([aligned.content], { type: blob.type || 'text/plain' });
            }
            return { body: replacement, sid: r.sid, masked: true };
          }
        }
      } catch (e) {
        void notifyFinal(sid, 'mask_not_applied');
        return null;
      }
    }

    return null;
  }

  // ─── escape 由引擎按槽位判定 ───
  // 这里原先有一段「读首个 data: 行的首字符，猜整条流要不要 JSON 转义」的启发式。
  // 它已删除，原因有两条：
  //   1. **粒度错了**。escape 的语义是「这个值是不是要放进 JSON 字符串里」，
  //      而同一条流里 `choices[].delta.content`（正文，不转义）与
  //      `tool_calls[].function.arguments`（JSON 文本，必须转义）是**并存**的，
  //      整条流只能猜出一个值，必然有一个是错的。
  //   2. **它永远猜不准 SSE**。首帧往往只是 `data: {"choices":[...`，末尾的续帧
  //      不以 `data:` 开头，靠首帧推出来的值要一路用到流结束。
  // 现在由引擎 `_sse_text_slots()` 逐槽位给出 escape（与代理链路同一份实现）。
  // 扩展只需要告诉引擎 content-type —— 分帧方式（SSE 空行 / NDJSON 换行 / 整体）
  // 也由引擎判定，避免同一套规则在两边各写一遍然后悄悄漂移。

  const streamLike = (ct) =>
    ct.includes('text/event-stream') || ct.includes('text/plain') || ct.includes('application/json') || ct.includes('ndjson') || ct.includes('json-seq');

  // ─── fetch hook ───
  const origFetch = window.fetch;
  window.fetch = async function (...args) {
    let [resource, init] = args;
    let isReq = false;
    let url = '';
    let method = 'GET';
    try {
      isReq = resource instanceof Request;
      if (isReq && init) {
        resource = new Request(resource, init);
        args = [resource];
      }
      url = isReq ? resource.url : String(resource);
      method = (isReq ? resource.method : (init && init.method) || 'GET').toUpperCase();
    } catch (e) {
      return origFetch.apply(this, args);             // 任何解析意外：原样放行
    }
    // 只有带 body 的写方法值得过桥。**方法过滤是这里的主判据**——GET 语义的查询串
    // 在进任何长度判断之前就被挡掉了（MIN_MASKABLE_LEN 原先兼着这个职责，现已退回
    // 它真正的含义：只是一个「短到不可能有 PII」的下限，见其定义处）。
    if (method !== 'POST' && method !== 'PUT' && method !== 'PATCH') {
      return origFetch.apply(this, args);
    }

    const cfg = await getWideMode();
    const isUrlMaskable = shouldMaskUrl(url, cfg.wideMode);
    const isFileUrlMaskable = shouldMaskFileUrl(url, cfg.wideMode);
    noteRequest(url, isUrlMaskable);

    if (!isUrlMaskable && !isFileUrlMaskable) {
      // 不打码不包装（成对原则）。窄模式下记一条「未命中对话接口的 POST」供反馈——
      // 广泛模式不记：它本就把同站 POST 全收了，记下来的全是跨域埋点，纯噪音。
      if (!cfg.wideMode) {
        try {
          const p = new URL(url, location.href).pathname;
          if (!NEVER_MASK_PATH.some((rx) => rx.test(p))) {
            bridge.notify('note', { kind: 'unmatched_post', path: p });
          }
        } catch (e) { /* ignore */ }
      }
      return origFetch.apply(this, args);
    }
    const minLen = MIN_MASKABLE_LEN;

    let sid = null;
    if (isReq) {
      // ── multipart：文本字段打码，文件打码 ──
      if (isMultipart(resource)) {
        const fd = await resource.clone().formData().catch(() => null);
        if (fd) {
          const m = await maskMultipart(fd, { url, method });
          if (m && m.blocking) throw new TypeError('Failed to fetch');   // (A)
          if (m) {
            // 必须删掉 content-type（旧 boundary）和 content-length（打码后体积已变）
            if (m.masked) {
              const h = new Headers(resource.headers);
              h.delete('content-type');
              h.delete('content-length');
              resource = new Request(resource, { body: m.body, headers: h, signal: resource.signal });
            }
            return m.sid ? fetchWithLedger(this, [resource], m.sid) : origFetch.call(this, resource);
          }
        }
        return origFetch.apply(this, args);
      }

      // ── 单文件直传（仅当请求发往上传/存储桶端点时，识别 Blob 附件）──
      if (isDirectFileUploadUrl(url)) {
        const ct = (resource.headers.get('content-type') || '').toLowerCase();
        if (ct.includes('application/') || ct.includes('text/') || ct === '') {
          const cloned = resource.clone();
          const blob = await cloned.blob().catch(() => null);
          if (blob && isMaskableTextAttachment(blob)) {
            const m = await maskSingleFile(blob, url, method);
            if (m && m.blocking) throw new TypeError('Failed to fetch');
            if (m) {
              if (m.masked) {
                const h = new Headers(resource.headers);
                h.delete('content-length');
                resource = new Request(resource, { body: m.body, headers: h, signal: resource.signal });
              }
              return m.sid ? fetchWithLedger(this, [resource], m.sid) : origFetch.call(this, resource);
            }
            return origFetch.call(this, resource);
          }
        }
      }

      // 其它非文本 body（Blob / ReadableStream / ArrayBuffer）原样放行。
      if (!isMaskableBody(resource) || !isUrlMaskable) return origFetch.apply(this, args);
      const raw = await resource.clone().text();
      if (raw.length > minLen) {
        const r = await maskWebText(raw, { url, method });
        if (r && r.ok) {
          sid = r.sid;
          // 零触碰原则：只有当内容确实被替换脱敏时，才重新构造 Request。
          // 若内容未变，保留原生 resource 对象，防止破坏字节跳动/豆包等严格的风控签名（a_bogus）。
          if (r.masked_text !== raw) {
            resource = new Request(resource, { body: r.masked_text, signal: resource.signal });
          }
        } else if (r && r.blocking) {
          throw new TypeError('Failed to fetch');      // (A) 无条件阻断
        }
        // r.passthrough / r === null（桥超时）→ 原样放行（(B)）
      }
      return sid
        ? fetchWithLedger(this, [resource], sid)
        : origFetch.call(this, resource);
    }

    let reqInit = init;
    const isUrlParams = initBodyIsURLSearchParams(init);
    if (initBodyIsFormData(init)) {
      const m = await maskMultipart(init.body, { url, method });
      if (m && m.blocking) throw new TypeError('Failed to fetch');       // (A)
      if (m) {
        if (m.masked) {
          const h = new Headers((init && init.headers) || {});
          h.delete('content-type');
          h.delete('content-length');
          reqInit = { ...init, body: m.body, headers: h };
        }
        return m.sid ? fetchWithLedger(this, [url, reqInit], m.sid) : origFetch.call(this, url, reqInit);
      }
    } else if (initBodyIsBlob(init) && isUploadOrStorageUrl(url)) {
      const m = await maskSingleFile(init.body, url, method);
      if (m && m.blocking) throw new TypeError('Failed to fetch');
      if (m) {
        if (m.masked) {
          const h = new Headers((init && init.headers) || {});
          h.delete('content-length');
          reqInit = { ...init, body: m.body, headers: h };
        }
        return m.sid ? fetchWithLedger(this, [url, reqInit], m.sid) : origFetch.call(this, url, reqInit);
      }
    } else if (initBodyIsArrayBuffer(init)) {
        const bytes = init.body instanceof ArrayBuffer ? new Uint8Array(init.body) : new Uint8Array(init.body.buffer, init.body.byteOffset, init.body.byteLength);
      if (isUrlMaskable) {
        // ── 字节跳动/豆包 WebAssembly 文本内存字节数组脱敏分支 ──
        let rawText = null;
        let r = null;
        try {
          rawText = new TextDecoder().decode(init.body);
        } catch (e) { rawText = null; /* 非文本字节 → (B) 原样放行 */ }
        if (rawText && rawText.length > minLen) {
          try {
            r = await maskWebText(rawText, { url, method });
          } catch (e) { r = null; }
        }
        if (r && r.blocking) throw new TypeError('Failed to fetch');   // (A) 必须阻断
        if (r && r.ok) {
          sid = r.sid;
          if (r.masked_text !== rawText) {
            const enc = new TextEncoder();
            const maskedBytes = enc.encode(r.masked_text);
            reqInit = { ...init, body: maskedBytes };
          }
        }
      }
    } else if ((initBodyIsText(init) || isUrlParams) && isUrlMaskable && (isUrlParams ? init.body.toString().length : init.body.length) > minLen) {
      const textToMask = isUrlParams ? init.body.toString() : init.body;
      const r = await maskWebText(textToMask, { url, method });
      if (r && r.ok) {
        sid = r.sid;
        // 零触碰原则：未命中敏感词时不改写 init 对象，保护签名完整性
        if (r.masked_text !== textToMask) {
          reqInit = { ...init, body: isUrlParams ? new URLSearchParams(r.masked_text) : r.masked_text };
        }
      } else if (r && r.blocking) {
        throw new TypeError('Failed to fetch');        // (A)
      }
    }
    // 未枚举/处理不了的 body 形态（Blob 发往非上传 URL、ReadableStream、Document…）：
    // 原样放行，但 **URL 命中对话白名单时必须留痕** —— 与 isReq 路径
    // （`isUrlMaskable && !isMaskableBody`）同一条原则：判定要在放行**之前**做，
    // 否则没有任何地方能知道这次是明文出网。此前 init 路径一个上报点也没有。
    if (isUrlMaskable && !initBodyWillBeHandled(init, url)) {
      bridge.notify('note', { kind: 'unsupported_protocol' });
    }
    return sid
      ? fetchWithLedger(this, [url, reqInit], sid)
      : origFetch.call(this, url, reqInit);
  };

  const notifyFinal = (sid, outcome, streamId, status = 0, contentType = '') => {
    if (!sid) return Promise.resolve(null);
    return bridge.call('restore', {
      sid, stream_id: streamId || crypto.randomUUID(), text: '', final: true,
      content_type: contentType, response_status: status, outcome,
    });
  };

  const fetchWithLedger = async (receiver, args, sid) => {
    try {
      const url = args[0] instanceof Request ? args[0].url : String(args[0]);
      const provider = webchat && webchat.match(url, location.href);
      const response = await origFetch.apply(receiver, args);
      return wrapResponse(response, sid, provider);
    } catch (error) {
      void notifyFinal(sid, error && error.name === 'AbortError' ? 'cancelled' : 'network_error');
      throw error;
    }
  };

  // Read only when the page consumes the response; record cancellation and
  // stream failures as incomplete rather than claiming successful restoration.
  const wrapResponse = (res, sid, provider) => {
    const ct = (res.headers.get('content-type') || '').toLowerCase();
    const streamId = (crypto.randomUUID && crypto.randomUUID()) || String(Math.random()).slice(2);
    if (!res.body || [204, 205, 304].includes(res.status)) {
      void notifyFinal(sid, 'completed', streamId, res.status, ct);
      return res;
    }
    if (!streamLike(ct) || res.status === 0) {
      void notifyFinal(sid, 'unsupported_response', streamId, res.status, ct);
      return res;
    }
    const dec = new TextDecoder();
    const enc = new TextEncoder();
    let reader = null;
    let ended = false;
    let upstreamEOF = false;
    let restoreFailed = false;
    let upstreamChunks = 0, upstreamBytes = 0, outputChunks = 0, outputBytes = 0;
    const codec = provider && /event-stream|json/.test(ct) && webchat.createRestorer(provider, ct, async (text) => {
      const r = await bridge.call('restore', {
        sid, stream_id: streamId, text, final: false, value_complete: true, content_type: 'text/plain', response_status: res.status,
      });
      if (ended) throw new TypeError('Silencer: cancelled');
      if (!r || !r.ok || typeof r.text !== 'string') {
        restoreFailed = true;
        throw new TypeError('Silencer: webchat_restore_failed');
      }
      return r.text;
    }, (details) => publishDiagnostic({ [details.history ? 'geminiHistory' : 'geminiStream']: { transport: 'fetch', ...details } }));
    const transformed = new ReadableStream({
      async pull(ctrl) {
        try {
          // A partial SSE/length frame may produce no output. Keep reading for
          // the pending consumer; returning without enqueue can stall HWM=0.
          while (!ended) {
            if (!reader) reader = res.body.getReader();
            const { value, done } = await reader.read();
            if (ended) return;
            if (done) upstreamEOF = true;
            if (!done && value) { upstreamChunks++; upstreamBytes += value.byteLength || 0; }
            const text = done ? dec.decode() : dec.decode(value, { stream: true });
            if (codec) {
              let out = await codec.push(text, done);
              if (ended) return;
              // ChatGPT terminates its SSE protocol with [DONE], but some
              // upstream connections stay open briefly after that frame. The
              // page consumes [DONE] and cancels fetch; waiting for HTTP EOF
              // here makes a successfully generated answer look cancelled.
              // Complete the restore acknowledgement before exposing [DONE]
              // so the page receives a naturally closed response stream.
              if (provider?.id === 'chatgpt' && codec.isTerminal?.()) {
                out += await codec.push('', true);
                if (ended) return;
                const final = await bridge.call('restore', {
                  sid, stream_id: streamId, text: '', final: true, await_ack: true,
                  content_type: 'text/plain', response_status: res.status, outcome: 'completed',
                });
                if (ended) return;
                if (!final || !final.ok || final.text) {
                  restoreFailed = true;
                  throw new TypeError('Silencer: webchat_restore_failed');
                }
                if (out) {
                  const bytes = enc.encode(out);
                  outputChunks++; outputBytes += bytes.byteLength;
                  ctrl.enqueue(bytes);
                }
                ended = true;
                ctrl.close();
                void notifyFinal(sid, 'completed', streamId, res.status, 'text/plain');
                publishDiagnostic({ chatgptStream: {
                  transport: 'fetch', state: 'completed_after_done', status: res.status, upstreamEOF,
                  upstreamChunks, upstreamBytes, outputChunks, outputBytes, codec: codec.diagnostics(),
                } });
                if (reader && !done) void reader.cancel('chatgpt_sse_done').catch(() => {});
                return;
              }
              if (out) {
                const bytes = enc.encode(out);
                outputChunks++; outputBytes += bytes.byteLength;
                ctrl.enqueue(bytes);
              }
              if (done) {
                const final = await bridge.call('restore', {
                  sid, stream_id: streamId, text: '', final: true, await_ack: true,
                  content_type: 'text/plain', response_status: res.status, outcome: 'completed',
                });
                if (ended) return;
                if (!final || !final.ok || final.text) {
                  restoreFailed = true;
                  throw new TypeError('Silencer: webchat_restore_failed');
                }
                ended = true;
                ctrl.close();
                void notifyFinal(sid, 'completed', streamId, res.status, 'text/plain');
              }
              if (out || done) return;
              continue;
            }
            if (done && restoreFailed) {
              if (text) ctrl.enqueue(enc.encode(text));
              ended = true;
              ctrl.close();
              void notifyFinal(sid, 'restore_failed', streamId, res.status, ct);
              return;
            }
            const r = await bridge.call('restore', {
              sid, stream_id: streamId, text, final: done, content_type: ct,
              await_ack: done, response_status: res.status, outcome: 'completed',
            });
            if (ended) return;
            if (!r || !r.ok) restoreFailed = true;
            const out = r && r.ok ? r.text : text;
            if (out) {
              const bytes = enc.encode(out);
              outputChunks++; outputBytes += bytes.byteLength;
              ctrl.enqueue(bytes);
            }
            if (done) {
              ended = true;
              ctrl.close();
              void notifyFinal(sid, restoreFailed ? 'restore_failed' : 'completed', streamId, res.status, ct);
            }
            return;
          }
        } catch (error) {
          if (ended) return;
          ended = true;
          noteResponseError(error);
          void notifyFinal(sid, restoreFailed || (error && error.silencerBlocking) ? 'restore_failed' : 'response_stream_error', streamId, res.status, ct);
          if (reader) void reader.cancel(error).catch(() => {});
          ctrl.error(error);
        }
      },
      async cancel(reason) {
        if (!ended) {
          ended = true;
          if (provider?.id === 'chatgpt') {
            const codecState = typeof codec?.diagnostics === 'function' ? codec.diagnostics() : null;
            publishDiagnostic({ chatgptStream: {
              transport: 'fetch', state: 'consumer_cancelled', status: res.status, upstreamEOF,
              upstreamChunks, upstreamBytes, outputChunks, outputBytes, codec: codecState,
            } });
          }
          void notifyFinal(sid, 'cancelled', streamId, res.status, ct);
        }
        if (reader) await reader.cancel(reason);
        else await res.body.cancel(reason);
      },
    }, { highWaterMark: 0 });
    const headers = new Headers(res.headers);
    headers.delete('content-length');
    headers.delete('content-encoding');
    let wrapped;
    try {
      wrapped = new Response(transformed, { status: res.status, statusText: res.statusText, headers });
    } catch (error) {
      void notifyFinal(sid, 'unsupported_response', streamId, res.status, ct);
      return res;
    }
    for (const prop of ['url', 'type', 'redirected']) {
      try { Object.defineProperty(wrapped, prop, { value: res[prop], configurable: true }); } catch (error) { /* ignore */ }
    }
    return wrapped;
  };

  // ─── XMLHttpRequest hook ───
  //
  // 注：文本类 mask 的调用点都顺手记下引擎回的 sid（`if (r && r.ok && r.sid) ctx.sid = r.sid`）。
  // 响应侧还原必须用**同一个 sid**（映射挂在它下面），而这个 sid 只有 mask 响应里才有；
  // 文件类分支不记 —— 那是二进制响应，不走文本还原。
  // 桥要求阻断（引擎 413/503/400）时，给页面一个**完整**的失败事件链。
  //
  // ⚠️ 只派 `error` 是不够的：原生 XHR 在失败时**一定同时**派发 `loadend`，而 axios
  // 正是靠 `onloadend` 收尾的 —— 少这一个事件，axios 永远停在 pending（实测：页面卡满
  // 30s 超时，用户看到的是「转圈不停」，比报错更糟）。事件必须走 `dispatchEvent`
  // （SilencerXHR 已把它接到页面注册表上），不能直接调页面的 handler。
  const failXHR = (xhr) => {
    xhr.dispatchEvent(new ProgressEvent('error'));
    xhr.dispatchEvent(new ProgressEvent('loadend'));
  };

  const trackUnsupportedXHR = (xhr, sid) => {
    let outcome = 'unsupported_response';
    const context = xhr._silencer;
    const listeners = [];
    for (const type of ['abort', 'error', 'timeout']) {
      const listener = () => {
        outcome = type === 'abort' ? 'cancelled' : 'network_error';
      };
      listeners.push([type, listener]);
      nativeXHRAddEventListener.call(xhr, type, listener, { once: true });
    }
    const onEnd = () => {
      if (context) context.ledgerEnded = true;
      cleanup();
      void notifyFinal(sid, outcome, null, xhr.status);
    };
    const cleanup = () => {
      for (const [type, listener] of listeners) nativeXHRRemoveEventListener.call(xhr, type, listener);
      nativeXHRRemoveEventListener.call(xhr, 'loadend', onEnd);
      if (xhr.__mkLedgerCleanup === cleanup) xhr.__mkLedgerCleanup = null;
    };
    xhr.__mkLedgerCleanup = cleanup;
    nativeXHRAddEventListener.call(xhr, 'loadend', onEnd, { once: true });
  };

  const captureXHRMask = (ctx, result) => {
    if (result && result.ok && result.sid) ctx.sid = result.sid;
    if (ctx.aborted) {
      void notifyFinal(ctx.sid, 'cancelled');
      return false;
    }
    return true;
  };

  const origXHROpen = window.XMLHttpRequest.prototype.open;
  const origXHRSend = window.XMLHttpRequest.prototype.send;
  const origXHRSetRequestHeader = window.XMLHttpRequest.prototype.setRequestHeader;
  const origXHRAbort = window.XMLHttpRequest.prototype.abort;

  window.XMLHttpRequest.prototype.open = function (method, url, async, user, password) {
    const isAsync = async !== false;
    const previous = this._silencer;
    if (previous) {
      previous.aborted = true;
      if (previous.sid && !previous.ledgerEnded) {
        void notifyFinal(previous.sid, 'cancelled', this.__mkRestore && this.__mkRestore.streamId);
      }
    }
    if (this.__mkRestore) this.__mkRestore.detached = true;
    this.__mkRestore = null;
    if (this.__mkLedgerCleanup) this.__mkLedgerCleanup();
    this._silencer = {
      method: String(method || 'GET').toUpperCase(),
      url: String(url || ''),
      headers: {},
      isAsync: isAsync,
      aborted: false,
    };
    return origXHROpen.apply(this, arguments);
  };

  window.XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (this._silencer && this._silencer.headers) {
      this._silencer.headers[String(name || '').toLowerCase()] = String(value || '');
    }
    return origXHRSetRequestHeader.apply(this, arguments);
  };

  window.XMLHttpRequest.prototype.abort = function () {
    if (this._silencer) {
      this._silencer.aborted = true;
    }
    return origXHRAbort.apply(this, arguments);
  };

  window.XMLHttpRequest.prototype.send = function (body) {
    const ctx = this._silencer;
    if (!ctx || (ctx.method !== 'POST' && ctx.method !== 'PUT' && ctx.method !== 'PATCH') || !body) {
      return origXHRSend.apply(this, arguments);
    }

    if (!ctx.isAsync) {
      // 同步 XHR 做不了脱敏：打码必须异步问引擎，而 sync XHR 不能用 await。
      // **但不能静默**：URL 命中对话/上传白名单时这就是一次无感知漏脱敏（正文明文出网），
      // 必须留痕。用窄模式判据（不依赖异步的 wideMode），宁可少报也不能一声不响。
      if (shouldMaskUrl(ctx.url, false)) bridge.notify('note', { kind: 'unsupported_protocol' });
      return origXHRSend.apply(this, arguments);
    }

    const xhr = this;
    const isFormData = typeof FormData !== 'undefined' && body instanceof FormData;
    const isBlob = typeof Blob !== 'undefined' && body instanceof Blob;
    const isString = typeof body === 'string' && body.length > MIN_MASKABLE_LEN;
    const isArrayBuf = typeof ArrayBuffer !== 'undefined' && (body instanceof ArrayBuffer || ArrayBuffer.isView(body));
    // fetch 路径早就显式处理了 URLSearchParams，XHR 之前直接放行 → 这类表单编码 body
    // 会静默未脱敏（同样命中对话白名单时也一声不响）。
    const isUrlParams = typeof URLSearchParams !== 'undefined' && body instanceof URLSearchParams;

    if (!isFormData && !isBlob && !isString && !isArrayBuf && !isUrlParams) {
      // 没枚举到的 body 形态：原样放行，但命中白名单时留痕（与 fetch 路径同一条原则）。
      if (shouldMaskUrl(ctx.url, false)) bridge.notify('note', { kind: 'unsupported_protocol' });
      return origXHRSend.apply(this, arguments);
    }

    const processAndSend = async () => {
      try {
        const cfg = await getWideMode();
        const isUrlMaskable = shouldMaskUrl(ctx.url, cfg.wideMode);
        const isFileUrlMaskable = shouldMaskFileUrl(ctx.url, cfg.wideMode);
        noteRequest(ctx.url, isUrlMaskable);

        if (!isUrlMaskable && !isFileUrlMaskable) {
          if (!ctx.aborted) origXHRSend.call(xhr, body);
          return;
        }
        ctx.webchatRequest = !!(webchat && webchat.match(ctx.url, location.href));

        let sendBody = body;
        if (isFormData) {
          const m = await maskMultipart(body, { url: ctx.url, method: ctx.method });
          if (!captureXHRMask(ctx, m && { ...m, ok: true })) return;
          if (m && m.blocking) {
            failXHR(xhr);
            return;
          }
          if (m && m.body) {
            sendBody = m.body;
          }
        } else if (isBlob && isFileUrlMaskable) {
          const m = await maskSingleFile(body, ctx.url, ctx.method);
          if (!captureXHRMask(ctx, m && { ...m, ok: true })) return;
          if (m && m.blocking) {
            failXHR(xhr);
            return;
          }
          if (m && m.masked) {
            sendBody = m.body;
          }
        } else if (isArrayBuf && (isUrlMaskable || isFileUrlMaskable)) {
                const bytes = body instanceof ArrayBuffer ? new Uint8Array(body) : new Uint8Array(body.buffer, body.byteOffset, body.byteLength);
      if (isUrlMaskable) {
            try {
              const dec = new TextDecoder();
              const rawText = dec.decode(body);
              if (rawText && rawText.length > MIN_MASKABLE_LEN) {
                const r = await maskWebText(rawText, { url: ctx.url, method: ctx.method });
                if (!captureXHRMask(ctx, r)) return;
                if (r && r.blocking) {
                  failXHR(xhr);
                  return;
                }
                if (r && r.ok && r.sid) ctx.sid = r.sid;
                if (r && r.ok && r.masked_text && r.masked_text !== rawText) {
                  sendBody = new TextEncoder().encode(r.masked_text);
                }
              }
            } catch (e) { /* ignore */ }
          }
        } else if (isUrlParams && isUrlMaskable) {
          const textToMask = body.toString();
          if (textToMask.length > MIN_MASKABLE_LEN) {
            const r = await maskWebText(textToMask, { url: ctx.url, method: ctx.method });
            if (!captureXHRMask(ctx, r)) return;
            if (r && r.blocking) {
              failXHR(xhr);
              return;
            }
            if (r && r.ok && r.sid) ctx.sid = r.sid;
            // 与 fetch 路径同一套做法：重新构造 URLSearchParams（不要拼字符串），
            // 否则编码可能与原文不一致（空格 / `+` 等）。
            if (r && r.ok && r.masked_text && r.masked_text !== textToMask) {
              sendBody = new URLSearchParams(r.masked_text);
            }
          }
        } else if (isString && isUrlMaskable) {
          const r = await maskWebText(body, { url: ctx.url, method: ctx.method });
          if (!captureXHRMask(ctx, r)) return;
          if (r && r.blocking) {
            failXHR(xhr);
            return;
          }
          if (r && r.ok && r.sid) ctx.sid = r.sid;
          if (r && r.ok && r.masked_text) {
            sendBody = r.masked_text;
          }
        }

        if (ctx.aborted) return;
        // 响应侧还原：仅当拿到 sid、且响应是文本型时启用（见 SilencerXHR）。
        // 这里曾是「XHR 只脱敏不还原」的已知限制 —— 直接改原生实例确实做不到
        // （`responseText` 是同步 getter，而还原要异步问引擎），但换成**继承 +
        // 截获事件派发**就做得到：先还原完，再把事件交给页面。
        if (ctx.sid && typeof xhr.__mkSetupRestore === 'function') {
          xhr.__mkSetupRestore(ctx.sid);
        } else if (ctx.sid) {
          trackUnsupportedXHR(xhr, ctx.sid);
        }
        origXHRSend.call(xhr, sendBody);
      } catch (err) {
        if (!ctx.aborted) {
          if (ctx.sid || ctx.webchatRequest) failXHR(xhr);
          else origXHRSend.call(xhr, body);
        }
      }
    };

    processAndSend();
  };

  // ─── XHR 响应侧还原（SilencerXHR）────────────────────────────────────────────
  //
  // 【为什么以前算「已知限制」】`responseText` / `response` 是**同步** getter，而还原
  // 必须异步问引擎（占位符→原文的映射只在引擎侧）。直接改原生实例的文本做不到。
  //
  // 【现在怎么做到】换个位置下手：**继承**原生 XHR，并把页面的 `addEventListener`
  // 与 `on*` 全部截获到自己的表里 —— 页面因此**根本没注册到原生实例**上，原生事件
  // 只有我们自己的内部监听收得到。于是「什么时候把事件交给页面」由我们决定：
  // 先异步还原，再派发。页面（axios 就是在 `onprogress` 里读 `responseText`）读到的
  // 已经是还原后的文本。
  //
  // 【安全边界不变】映射仍然只存在于引擎侧，MAIN world 拿到的只有还原结果，与 fetch
  // 路径完全一致。分帧同样交给引擎（`stream_id` + `final`，与 `wrapResponse` 共用同一套
  // `restore_stream_chunk`），这里只喂**新增**的原生文本增量，绝不自己切 SSE 帧 ——
  // 跨 chunk 被切开的占位符由引擎缓冲拼回来。
  //
  // 【绝不碰的场合】`responseType` 不是 text/'' 的、没拿到 sid 的，全部原样透传；
  // 还原失败/超时也恒透传（少还原一个 chunk，绝不能让用户的流卡住）。
  const RESTORE_TIMEOUT_MS = 2500;
  const XHR_EVENT_TYPES = [
    'readystatechange', 'progress', 'load', 'loadstart', 'loadend', 'error', 'abort', 'timeout',
  ];
  const OrigXHR = window.XMLHttpRequest;
  const nativeXHRAddEventListener = OrigXHR.prototype.addEventListener;
  const nativeXHRRemoveEventListener = OrigXHR.prototype.removeEventListener;
  const nativeXHRResponseTextGet = Object.getOwnPropertyDescriptor(OrigXHR.prototype, 'responseText').get;
  const nativeXHRResponseGet = Object.getOwnPropertyDescriptor(OrigXHR.prototype, 'response').get;

  class SilencerXHR extends OrigXHR {
    constructor() {
      super();
      this.__mkListeners = new Map();       // 页面注册的监听器：type -> [{fn, once}]
      this.__mkOn = Object.create(null);    // 页面的 on* 属性
      this.__mkRestore = null;              // 仅当本次请求需要还原时才有值
      // 用**原生** addEventListener 注册内部监听（绕过下面的重写），保证原生事件
      // 一定被我们收到、而页面永远拿不到「未还原」的那一份。
      for (const evType of XHR_EVENT_TYPES) {
        nativeXHRAddEventListener.call(this, evType, (ev) => { this.__mkOnNative(evType, ev); });
      }
    }

    // ── 页面注册面：全部截获，不落到原生实例上 ──
    addEventListener(type, fn, opts) {
      if (!fn || !this.__mkListeners) return nativeXHRAddEventListener.call(this, type, fn, opts);
      const t = String(type);
      const arr = this.__mkListeners.get(t) || [];
      const capture = typeof opts === 'boolean' ? opts : !!(opts && opts.capture);
      const signal = opts && typeof opts === 'object' && opts.signal;
      if (signal && signal.aborted) return;
      if (arr.some((item) => item.fn === fn && item.capture === capture)) return;
      const item = { fn, capture, once: !!(opts && typeof opts === 'object' && opts.once), cleanup: null };
      if (signal) {
        const abort = () => this.removeEventListener(t, fn, capture);
        signal.addEventListener('abort', abort, { once: true });
        item.cleanup = () => signal.removeEventListener('abort', abort);
      }
      arr.push(item);
      this.__mkListeners.set(t, arr);
    }

    removeEventListener(type, fn, opts) {
      if (!this.__mkListeners) return nativeXHRRemoveEventListener.call(this, type, fn, opts);
      const t = String(type);
      const capture = typeof opts === 'boolean' ? opts : !!(opts && opts.capture);
      const arr = this.__mkListeners.get(t);
      if (arr) this.__mkListeners.set(t, arr.filter((item) => {
        if (item.fn !== fn || item.capture !== capture) return true;
        if (item.cleanup) item.cleanup();
        return false;
      }));
    }

    // 我们在阻断分支里用的 `dispatchEvent(new ProgressEvent('error'))` 也必须走这张表
    // （否则页面注册的 onerror 收不到，阻断就成了静默失败）；页面主动派发同理。
    // 这里**不调** super：原生事件不经过本方法，不存在重复派发。
    dispatchEvent(ev) {
      if (!ev || !this.__mkListeners) return super.dispatchEvent(ev);
      this.__mkFire(String(ev.type), ev);
      return !ev.defaultPrevented;
    }

    __mkSetupRestore(sid) {
      if (this.__mkRestore) return;
      let rt = '';
      try { rt = String(this.responseType || ''); } catch (e) { return; }
      if (rt !== '' && rt !== 'text') {
        trackUnsupportedXHR(this, sid);
        return;
      }
      this.__mkRestore = {
        sid,
        streamId: (crypto.randomUUID && crypto.randomUUID()) || String(Math.random()).slice(2),
        rawLen: 0,          // 已喂给引擎的**原生**文本长度（还原后长度会变，所以按原生算）
        restored: '',       // 累积的还原结果 —— 即页面读到的内容
        contentType: '',
        codec: null,
        finalSent: false,
        failStreak: 0,      // 连续失败计数（成功即归零）
        failed: false,
        outcome: 'completed',
        degraded: false,    // 本次流已放弃还原，全部按原生文本透传
        chain: Promise.resolve(),
      };
    }

    __mkOnNative(evType, ev) {
      const st = this.__mkRestore;
      if (!st) { this.__mkFire(evType, ev); return; }   // 不还原：同步转发，零时序变化
      if (evType === 'abort') st.outcome = 'cancelled';
      else if (evType === 'error' || evType === 'timeout') st.outcome = 'network_error';
      // 串行化：progress 会连着来，必须让还原按顺序完成，否则结果会错位。
      st.chain = st.chain.then(() => this.__mkHandle(evType, ev, st)).catch(() => {
        if (!st.detached && this.__mkRestore === st) this.__mkFire(evType, ev);
      });
    }

    async __mkHandle(evType, ev, st) {
      if (st.detached || this.__mkRestore !== st) return;
      if (evType === 'abort') st.outcome = 'cancelled';
      else if (evType === 'error' || evType === 'timeout') st.outcome = 'network_error';
      const isFinal = ['loadend', 'abort', 'error', 'timeout'].includes(evType) || (this.readyState === 4 && this.status > 0);
      try { await this.__mkPull(st, isFinal); } catch (e) { /* 还原失败恒透传 */ }
      if (!st.detached && this.__mkRestore === st) this.__mkFire(evType, ev);
    }

    async __mkPull(st, isFinal) {
      let raw = '';
      // 直接调**原生** getter：我们自己的 getter 返回的是还原后的文本，不是引擎要的口径。
      try { raw = nativeXHRResponseTextGet.call(this) || ''; } catch (e) {
        if (isFinal && !st.finalSent) {
          st.finalSent = true;
          if (this._silencer) this._silencer.ledgerEnded = true;
          void notifyFinal(st.sid, st.outcome !== 'completed' ? st.outcome : 'restore_failed', st.streamId, this.status, st.contentType);
        }
        return;
      }
      const delta = raw.length > st.rawLen ? raw.slice(st.rawLen) : '';
      if (!delta && !isFinal) return;
      if (isFinal && st.finalSent) return;
      if (isFinal) st.finalSent = true;
      st.rawLen = raw.length;
      if (!st.contentType) {
        try { st.contentType = (this.getResponseHeader('content-type') || '').toLowerCase(); } catch (e) { /* ignore */ }
      }
      const provider = webchat && webchat.match(this._silencer.url, location.href);
      if (provider && /event-stream|json/.test(st.contentType)) {
        try {
          if (!st.codec) st.codec = webchat.createRestorer(provider, st.contentType, async (text) => {
            const r = await bridge.call('restore', {
              sid: st.sid, stream_id: st.streamId, text, final: false, value_complete: true,
              content_type: 'text/plain', response_status: this.status,
            }, RESTORE_TIMEOUT_MS);
            if (!r || !r.ok || typeof r.text !== 'string') throw new TypeError('Silencer: webchat_restore_failed');
            return r.text;
          }, (details) => publishDiagnostic({ [details.history ? 'geminiHistory' : 'geminiStream']: { transport: 'xhr', ...details } }));
          if (st.outcome !== 'completed') throw new TypeError('Silencer: cancelled');
          const output = await st.codec.push(delta, isFinal);
          if (st.detached || this.__mkRestore !== st) return;
          st.restored += output;
          if (isFinal) {
            const r = await bridge.call('restore', {
              sid: st.sid, stream_id: st.streamId, text: '', final: true, await_ack: true,
              content_type: 'text/plain', response_status: this.status, outcome: 'completed',
            }, RESTORE_TIMEOUT_MS);
            if (st.detached || this.__mkRestore !== st) return;
            if (!r || !r.ok || r.text) throw new TypeError('Silencer: webchat_restore_failed');
            this._silencer.ledgerEnded = true;
            void notifyFinal(st.sid, 'completed', st.streamId, this.status, 'text/plain');
          }
        } catch (error) {
          if (st.detached || this.__mkRestore !== st) return;
          noteResponseError(error);
          st.failed = true;
          this._silencer.ledgerEnded = true;
          void notifyFinal(st.sid, st.outcome !== 'completed' ? st.outcome : 'restore_failed', st.streamId, this.status, st.contentType);
          st.detached = true;
          origXHRAbort.call(this);
          this.__mkFire('error', new ProgressEvent('error'));
          this.__mkFire('loadend', new ProgressEvent('loadend'));
        }
        return;
      }
      // 引擎挂了 / 超时：**连续两次**失败就把本次流降级为纯透传。
      //
      // 只失败一次还可能是引擎忙（偶发），但若不降级，每个 chunk 都要等满
      // RESTORE_TIMEOUT_MS —— 一条 200 chunk 的流就是 500 秒，用户的流被我们拖死。
      // 那比少还原几个字严重得多（同 fetch 路径「宁可少还原，绝不卡住流」的口径）。
      // 失败一次也**不缓存失败结果**：下一 chunk 仍然试，只是不无限等。
      if (st.degraded || st.outcome !== 'completed' || (isFinal && st.failed)) {
        st.restored += delta;
        if (isFinal) {
          if (this._silencer) this._silencer.ledgerEnded = true;
          void notifyFinal(st.sid, st.outcome !== 'completed' ? st.outcome : 'restore_failed', st.streamId, this.status, st.contentType);
        }
        return;
      }
      const r = await bridge.call('restore', {
        sid: st.sid, stream_id: st.streamId, text: delta, final: !!isFinal, content_type: st.contentType,
        await_ack: !!isFinal, response_status: this.status, outcome: 'completed',
      }, RESTORE_TIMEOUT_MS);
      if (st.detached || this.__mkRestore !== st) return;
      if (r && r.ok) {
        st.failStreak = 0;
        st.restored += (typeof r.text === 'string' ? r.text : delta);
        if (isFinal) {
          if (this._silencer) this._silencer.ledgerEnded = true;
          void notifyFinal(st.sid, st.outcome, st.streamId, this.status, st.contentType);
        }
        return;
      }
      st.failed = true;
      st.failStreak += 1;
      if (st.failStreak >= 2) st.degraded = true;
      st.restored += delta;     // 还原失败恒透传（与 fetch 路径同口径）
      if (isFinal) {
        if (this._silencer) this._silencer.ledgerEnded = true;
        void notifyFinal(st.sid, st.outcome !== 'completed' ? st.outcome : 'restore_failed', st.streamId, this.status, st.contentType);
      }
    }

    __mkFire(evType, ev) {
      const list = this.__mkListeners && this.__mkListeners.get(evType);
      if (list && list.length) {
        for (const item of list.slice()) {
          if (!(this.__mkListeners.get(evType) || []).includes(item)) continue;
          if (item.once) this.removeEventListener(evType, item.fn, item.capture);
          // 页面自己的异常不该吃掉同事件上的其它监听（原生行为也是这样）。
          try {
            if (typeof item.fn === 'function') item.fn.call(this, ev);
            else if (typeof item.fn.handleEvent === 'function') item.fn.handleEvent(ev);
          } catch (e) { /* ignore */ }
        }
      }
      const on = this.__mkOn && this.__mkOn[evType];
      if (typeof on === 'function') {
        try { on.call(this, ev); } catch (e) { /* ignore */ }
      }
    }
  }

  // on* 属性：原生的是原型上的 setter/getter，直接覆盖成我们的表（含 get 回读）。
  for (const evType of XHR_EVENT_TYPES) {
    Object.defineProperty(SilencerXHR.prototype, 'on' + evType, {
      configurable: true,
      get() { return this.__mkOn ? this.__mkOn[evType] : null; },
      set(fn) {
        if (!this.__mkOn) return;
        if (typeof fn === 'function') this.__mkOn[evType] = fn;
        else delete this.__mkOn[evType];
      },
    });
  }

  Object.defineProperty(SilencerXHR.prototype, 'responseText', {
    configurable: true,
    get() {
      const st = this.__mkRestore;
      if (!st) return nativeXHRResponseTextGet.call(this);
      // 先过一遍原生 getter：readyState < 3 或 responseType 非 text 时它会按规范抛
      // InvalidStateError，这个语义必须保留（库靠它判断状态）。
      nativeXHRResponseTextGet.call(this);
      return st.restored;
    },
  });

  Object.defineProperty(SilencerXHR.prototype, 'response', {
    configurable: true,
    get() {
      const st = this.__mkRestore;
      if (!st) return nativeXHRResponseGet.call(this);
      let rt = '';
      try { rt = String(this.responseType || ''); } catch (e) { rt = 'x'; }
      if (rt === '' || rt === 'text') return st.restored;
      return nativeXHRResponseGet.call(this);
    },
  });

  // 替换全局构造器。bridge-main 是 document_start 注入，页面脚本必然在这之后解析，
  // 所以页面拿到的是 SilencerXHR；`instanceof XMLHttpRequest` 仍然成立（继承）。
  //
  // 【作用域收窄（2026-09-21 回归修复）】**只在 DeepSeek 替换**。
  // 原生 XHR 事件是**同步**派发的，而本构造器把页面监听全部截获、改成「先异步还原再派发」
  // —— 这对**每一个** XHR 请求都加了一层时序包装，任何依赖同步时序的页面代码都可能出错。
  // 实测：ChatGPT / Claude 这两个此前已验证可用的站点因此出现异常。
  // XHR 响应还原目前只为 DeepSeek（axios 在 onprogress 里读 responseText）落地，
  // 所以其余站点一律保持原生构造器，零副作用。以后要扩站点，先在那个站点真机验证。
  const XHR_RESTORE_HOSTS = /(^|\.)(?:deepseek\.com|gemini\.google\.com|doubao\.com|kimi\.com|kimi\.moonshot\.cn)$/i;
  if (XHR_RESTORE_HOSTS.test(location.hostname)) {
    window.XMLHttpRequest = SilencerXHR;
  }

  // ─── 桥失联 → 恢复原生（2026-09-21）────────────────────────────────────
  //
  // 【为什么需要】重载/更新扩展会**销毁已打开页面**里的 content script 上下文，
  // 新脚本只在下次导航时注入。而旧页面里的 fetch/XHR 仍被本脚本 patch 着，它们要
  // 调的后台（ISOLATED 侧与 SW）已经换新 —— 于是请求卡住或抛异常，页面进入「半死」
  // 状态：既不脱敏，也不能正常用。这比单纯「不脱敏」更糟（实测 ChatGPT/Claude 会这样）。
  //
  // 【怎么判断】扩展失联后 MAIN 侧的 postMessage 不再有人应答，bridge.call 必然
  // 一路超时返回 null。连续 BRIDGE_DOWN_STREAK 次拿不到结果就认定桥断了。阈值取 4
  // 而不是 1~2：mask 走 6s 超时且引擎侧有全局锁，偶发慢响应不该被当成失联。
  //
  // 【恢复成什么】恢复**原生** fetch/XHR，并从此不再干预本页。这与 SECURITY.md 里
  // 「(B) 直通 = 未脱敏」是同一个取向：项目红线是「绝不断网」，引擎/桥不可用时明文
  // 直连是刻意的默认。恢复原生只是把这种退化变得**干净、可预期**，并让用户知道
  // 「本页需要刷新」——而不是留一堆半死的 patch。
  //
  // ⚠️ 只动「注入与失联处置」，不碰任何打码/还原逻辑。
  const BRIDGE_DOWN_STREAK = 4;
  let bridgeDownStreak = 0;
  let nativeRestored = false;

  function restoreNativeAndStop(reason) {
    if (nativeRestored) return;
    nativeRestored = true;
    // 判断条件用「引用是否已变」而不是记一个标志位：页面可能自己包装过 fetch
    // （Sentry 这类 SDK 就会），那种包装是基于我们的版本的，一起换回原生会让它失效。
    // 两弊相权取其轻：半死的页面比「页面包装被拆掉」严重得多，而且下面 console 会
    // 明确提示「请刷新页面」。
    try {
      if (window.fetch !== origFetch) window.fetch = origFetch;
    } catch (_e) { /* 页面冻结了 window，放弃恢复 */ }
    try {
      if (window.XMLHttpRequest === SilencerXHR) window.XMLHttpRequest = OrigXHR;
      OrigXHR.prototype.open = origXHROpen;
      OrigXHR.prototype.send = origXHRSend;
      OrigXHR.prototype.setRequestHeader = origXHRSetRequestHeader;
      OrigXHR.prototype.abort = origXHRAbort;
    } catch (_e) { /* 同上 */ }
    try {
      console.warn('[Silencer] 与扩展后端失联（' + reason + '），已恢复原生网络行为：'
        + '本页将不再脱敏，请**刷新页面**以重新启用保护。');
    } catch (_e) { /* console 被页面接管 */ }
  }

  function noteBridgeCall(ok) {
    if (ok) { bridgeDownStreak = 0; return; }
    if (nativeRestored) return;
    bridgeDownStreak += 1;
    if (bridgeDownStreak >= BRIDGE_DOWN_STREAK) {
      restoreNativeAndStop(bridgeDownStreak + ' 次调用无响应');
    }
  }
  void getWideMode();
})();
