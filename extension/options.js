/* Silencer Browser Bridge — 设置页逻辑
 *
 * 安全边界（违反即返工）：
 *  - token 只写入 `chrome.storage.local`，**不注入页面、不进 postMessage**；
 *  - 不用 `chrome.storage.sync`（那是把数据同步到账号 = 外传）；
 *  - 任何地方都不 `console.log(body)`；本页也从不读请求正文。
 *
 * targetOrigin 一律用 location.origin 的显式值，不用 '*'。
 */

'use strict';

// 共享常量与纯函数（唯一来源，见 shared.js 头部说明）。
const { STATIC_SITES, PRESET_SITES, siteMatchPattern, normalizeDomain, isLocalPanelUrl, isPanelUrl } = self.SILENCER_SHARED;
// `reasonFor` 只是 `unsupportedReason(domain)` 的本页别名，保留名字让调用点读起来直白。
const reasonFor = (domain) => self.SILENCER_SHARED.unsupportedReason(domain);

const I18N = {
  zh: {
    title: 'Silencer Browser Bridge',
    subtitle: '为 ChatGPT、Gemini 和 DeepSeek 网页连接 Silencer 网关，脱敏文字对话并还原流式回复。',
    secWebchat: '网页版文字适配',
    webchatHint: '这里是协议支持介绍，不代表已启用。请先在上方站点列表添加并允许浏览器权限，再用测试对话核对账本。注入成功和网关在线不等于请求已保护',
    webchatBoundary: '本轮覆盖文字；图片、语音、文件上传、WebSocket 和 Protobuf 未覆盖。请先发送非敏感测试内容，在网关账本核对脱敏与还原。网页协议变化或正文无法解析时，已识别的文字请求会报错并标记原因。',
    webchatPending: '已验证 · 仅文字对话',
    secEngine: '引擎连接',
    engineHint: '网关地址。默认端口 8080；远程地址也可使用，请确认令牌只发送到可信服务。',
    panelUrlPh: 'http://127.0.0.1:8080',
    testBtn: '测试连接',
    secToken: '访问令牌（ext_token）',
    tokenHint: '在面板「设置 → 浏览器扩展」里复制。令牌只在扩展本地存储，绝不注入页面。',
    tokenPh: '粘贴 ext_token',
    showBtn: '显示',
    hideBtn: '隐藏',
    openPanelBtn: '在面板中重置令牌',
    openPanelOk: '已在新标签页打开网关面板；请在「设置 → 浏览器扩展」里重置，再把新令牌粘回这里。',
    openPanelFail: '打开面板失败：{err}',
    secEnabled: '总开关',
    enabledLabel: '启用浏览器扩展链路（关闭后网页请求原样直连，引擎侧也会同步拒绝）',
    secScope: '拦截范围',
    scopeHint: '打码判定发生在网关引擎里：送过去的文本没匹配到敏感信息就原样返回，不会改动内容。',
    wideLabel: '广泛模式：尝试处理更多同站写请求（登录、认证及遥测请求除外）',
    secSites: '站点列表',
    sitesHint: '下面是已添加的站点。要加更多，点下面「推荐站点」里的任意一个；也可以自己在输入框里填域名。',
    presetHint: '点一下就加上（第一次加会弹一次授权，点「允许」）。',
    addAllPresets: '一键添加全部推荐站点',
    presetNone: '推荐站点都已经在上面了。',
    presetAdded: '已添加 {n} 个站点。请刷新已打开的网页，扩展才会对新页面生效。',
    presetAllWarn: '将添加 {n} 个已验证的文字对话站点。是否继续？',
    domainPh: '例如 chat.deepseek.com',
    addBtn: '添加站点',
    secNotes: '说明',
    saveBtn: '保存',
    notes: [
      '扩展事件记在**事件页**（不是「运行日志」）。快速筛法：在事件页搜索框输入 /ext/。',
      '统计开关在面板「高级设置 → 日志与隐私 → 记录浏览器扩展流量」；落库发生在引擎侧，改完不用重装扩展。',
      '关闭统计 ≠ 不脱敏：开关只停落库，脱敏/还原与「未脱敏状态」提示照常。',
      '广泛模式下，本站点发出的请求都会送进引擎；只有真正命中规则的才会被改写，其余原样返回。关掉广泛模式后，只送已知形态的对话接口。',
      '远程网关地址会提示风险但不会拦截；令牌会发送到你填写的地址。扩展不使用 storage.sync，不上报，不记录请求正文。'
    ],
    tagUnsupported: '未实测',
    removeBtn: '删除',
    saved: '已保存。',
    saveFail: '保存失败。',
    testOk: '连接成功：引擎 v{version}，统计 mask {mask} / restore {restore}。',
    testProtocolMismatch: '网关可连接，但协议不兼容（扩展 v{expected} / 网关 v{actual}）。请将网关和扩展更新为配套开发版；当前无法启用文字保护。',
    remoteWarning: '远程网关已连接；请确认令牌只发送到可信服务。',
    testFail: '连接失败：{err}。',
    testInvalidToken: '扩展令牌无效，请从面板重新复制 ext_token',
    testBridgeDisabled: '请在面板启用浏览器扩展联动',
    testUnreachable: '无法访问网关，请检查地址、网络和服务状态',
    testTimeout: '网关请求超时（5 秒）',
    testBackgroundUnavailable: '扩展后台未响应，请重新加载扩展后重试',
    testInvalidResponse: '网关返回的内容不符合扩展握手格式，请检查服务地址',
    testHttpError: '网关返回 HTTP {status}',
    testCloudflareError: 'Cloudflare 拒绝了请求（HTTP {status}，错误 {code}）',
    testPermissionDenied: '未授予网关地址的访问权限，请允许后重试',
    testPermissionFailed: '无法申请网关地址访问权限，请重新加载扩展后重试',
    testNoToken: '连接失败：未填写令牌（401/403）。',
    domainInvalid: '域名格式不合法（只允许小写字母、数字、. * -）。',
    domainDuplicate: '该站点已在列表中。',
    siteWarn: '「{domain}」的路径规则未经实测，添加后可能不生效（不会断网：未命中即直通，但也不会脱敏）。是否继续？',
    permDenied: '未授予该域名的访问权限，站点未添加。',
    regFailed: '站点已保存，但动态注册失败：{detail}（该站点不会生效）',
    resyncUnknown: '站点已保存，但没能确认是否注入成功（扩展后台没回应）。请到 chrome://extensions 点一次「重新加载」，再刷新目标网页。',
    quota: '会话存储占用 {used}（预算 {budget}）· 签发表 {sids} 条 · Chrome ≤111 约 1MB，112+ 约 10MB',
  },
  en: {
    secWebchat: 'Web-chat text adapters',
    webchatHint: 'This is a format support list, not enabled sites. Add sites in the Sites section above and grant browser access. Enabled injection and gateway connectivity do not prove protection.',
    webchatBoundary: 'Text only in this iteration. Images, audio, uploads, WebSocket and Protobuf are not covered. Start with synthetic input and verify masking/restoration in the gateway ledger. Recognized text requests fail visibly when their payload cannot be parsed.',
    webchatPending: 'Verified · text conversations only',
    title: 'Silencer Browser Bridge',
    subtitle: 'Routes ChatGPT, Gemini and DeepSeek text conversations through the Silencer gateway for masking and streaming restoration. Remote URLs show a warning but are not blocked.',
    secEngine: 'Engine',
    engineHint: 'Gateway address. Default port 8080. Remote gateways are supported; only use trusted hosts because the token is sent there.',
    panelUrlPh: 'http://127.0.0.1:8080',
    testBtn: 'Test connection',
    secToken: 'Access token (ext_token)',
    tokenHint: 'Copy it from the panel: Settings → Browser extension. The token stays in extension storage and is never injected into pages.',
    tokenPh: 'Paste ext_token',
    showBtn: 'Show',
    hideBtn: 'Hide',
    openPanelBtn: 'Reset token in the panel',
    openPanelOk: 'Opened the gateway panel in a new tab; reset under Settings → Browser extension, then paste the new token back here.',
    openPanelFail: 'Could not open the panel: {err}',
    secEnabled: 'Master switch',
    enabledLabel: 'Enable the browser-extension bridge (when off, page requests go straight through and the engine rejects too)',
    secScope: 'Interception scope',
    scopeHint: 'Masking happens in the gateway engine: text with no sensitive match is returned unchanged — content is never altered.',
    wideLabel: 'Wide mode: try more same-site writes (excluding authentication and telemetry)',
    secSites: 'Sites',
    sitesHint: 'Below are the sites already added. To add more, click any recommended site below, or type a domain yourself.',
    presetHint: 'One click adds it (the first add asks for host access once — click Allow).',
    addAllPresets: 'Add all recommended sites',
    presetNone: 'All recommended sites are already added above.',
    presetAdded: 'Added {n} sites. Reload any open AI page — the extension only injects into newly loaded pages.',
    presetAllWarn: 'This adds {n} verified text-conversation sites. Continue?',
    domainPh: 'e.g. chat.deepseek.com',
    addBtn: 'Add site',
    secNotes: 'Notes',
    saveBtn: 'Save',
    notes: [
      'Extension events land in the **Events page** (not the runtime log). Quick filter: type /ext/ in the events search box.',
      'The stats toggle lives in the panel under Advanced settings → Logging & privacy → Record browser-extension traffic. Persistence happens engine-side, so no extension reload is needed.',
      'Turning stats off ≠ disabling masking: it only stops persistence; masking/restoring and the "not masked" indicators keep working.',
      'In wide mode every request from the site is sent to the engine; only real matches are rewritten, everything else comes back as-is. With wide mode off, only known chat endpoints are sent.',
      'Remote gateway addresses show a warning but are not blocked; the token is sent to the address you enter. The extension never uses storage.sync, reports anything, or records request bodies.'
    ],
    tagUnsupported: 'unverified',
    removeBtn: 'Remove',
    saved: 'Saved.',
    saveFail: 'Save failed.',
    testOk: 'Connected: engine v{version}, stats mask {mask} / restore {restore}.',
    testProtocolMismatch: 'Gateway reachable, but protocol incompatible (extension v{expected} / gateway v{actual}). Update both to matching development builds; text protection is unavailable.',
    remoteWarning: 'Remote gateway connected; confirm that the token is sent only to a trusted service.',
    testFail: 'Connection failed: {err}.',
    testInvalidToken: 'Invalid extension token; copy ext_token from the panel again',
    testBridgeDisabled: 'Enable the browser-extension bridge in the panel',
    testUnreachable: 'Cannot reach the gateway; check the URL, network and service status',
    testTimeout: 'Gateway request timed out (5 seconds)',
    testBackgroundUnavailable: 'Extension background did not respond; reload the extension and retry',
    testInvalidResponse: 'Gateway response does not match the extension handshake; check the service URL',
    testHttpError: 'Gateway returned HTTP {status}',
    testCloudflareError: 'Cloudflare rejected the request (HTTP {status}, error {code})',
    testPermissionDenied: 'Gateway host access was not granted; allow it and retry',
    testPermissionFailed: 'Could not request gateway host access; reload the extension and retry',
    testNoToken: 'Connection failed: token missing (401/403).',
    domainInvalid: 'Invalid domain (only lowercase letters, digits, . * - are allowed).',
    domainDuplicate: 'That site is already in the list.',
    siteWarn: 'Path rules for "{domain}" are unverified; it may not take effect (it will not break the network — misses pass through — but it also will not mask). Continue?',
    permDenied: 'Host access not granted; site not added.',
    regFailed: 'Site saved, but dynamic registration failed: {detail} (it will not take effect)',
    resyncUnknown: 'Site saved, but we could not confirm injection (the extension background did not respond). Reload it once at chrome://extensions, then reload the target page.',
    quota: 'Session storage {used} of {budget} · {sids} sid entries · Chrome ≤111 ≈ 1MB, 112+ ≈ 10MB',
  }
};

let lang = 'zh';
const $ = (id) => document.getElementById(id);

function t(key, vars) {
  let s = (I18N[lang] && I18N[lang][key]) || (I18N.zh[key] || key);
  if (vars) {
    for (const [k, v] of Object.entries(vars)) s = s.replaceAll(`{${k}}`, String(v));
  }
  return s;
}

async function requestPanelPermission(panelUrl, messageId = 'saveMsg') {
  try {
    const origin = new URL(panelUrl).origin + '/*';
    const granted = await chrome.permissions.request({ origins: [origin] });
    if (!granted) setMsg($(messageId), t('testPermissionDenied'), 'warn');
    return granted;
  } catch (err) {
    setMsg($(messageId), t('testPermissionFailed'), 'warn');
    return false;
  }
}

function applyI18n() {
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  for (const el of document.querySelectorAll('[data-i18n]')) {
    el.textContent = t(el.dataset.i18n);
  }
  for (const el of document.querySelectorAll('[data-i18n-ph]')) {
    el.placeholder = t(el.dataset.i18nPh);
  }
  $('langBtn').textContent = lang === 'zh' ? 'English' : '中文';
  $('notes').innerHTML = '';
  for (const line of I18N[lang].notes) {
    const li = document.createElement('li');
    // 只用 textContent + 手动粗体标记，绝不 innerHTML 拼外串（避免注入面）
    const parts = String(line).split(/(\*\*[^*]+\*\*)/g);
    for (const p of parts) {
      if (p.startsWith('**') && p.endsWith('**')) {
        const b = document.createElement('b');
        b.textContent = p.slice(2, -2);
        li.appendChild(b);
      } else if (p) {
        li.appendChild(document.createTextNode(p));
      }
    }
    $('notes').appendChild(li);
  }
  // 表单态里的 button 标签会随语言变
  $('tokenToggle').textContent = $('token').type === 'password' ? t('showBtn') : t('hideBtn');
}

function setMsg(el, text, kind) {
  el.className = 'msg' + (kind ? ' ' + kind : '');
  el.textContent = text || '';
}

// ── 配置读写 ────────────────────────────────────────────────────────────────

const DEFAULTS = { token: '', panelUrl: 'http://127.0.0.1:8080', enabledSites: STATIC_SITES.slice(), enabled: true, wideMode: false, uiLang: '' };

async function loadConfig() {
  const stored = await chrome.storage.local.get(Object.keys(DEFAULTS));
  return { ...DEFAULTS, ...stored };
}

// ── 渲染 ────────────────────────────────────────────────────────────────────

async function renderSites() {
  const support = $('webchatSupport');
  support.innerHTML = '';
  for (const site of self.SILENCER_SHARED.WEBCHAT_SITES) {
    const item = document.createElement('li');
    item.textContent = `${site.name} · ${site.request} → ${site.response} · ${t('webchatPending')}`;
    support.appendChild(item);
  }
  const cfg = await loadConfig();
  const ul = $('siteList');
  ul.innerHTML = '';
  for (const domain of cfg.enabledSites) {
    const li = document.createElement('li');

    const chk = document.createElement('input');
    chk.type = 'checkbox';
    chk.checked = true;
    chk.addEventListener('change', () => removeSite(domain));

    const name = document.createElement('span');
    name.className = 'domain';
    name.textContent = domain;

    const spacer = document.createElement('span');
    spacer.className = 'spacer';

    li.append(chk, name, spacer);

    // 站点一律一视同仁：都能删、都能再加。
    // 原先给 chatgpt/claude 打「内置」、其余打「已添加」，用户会以为「内置」的两个特殊
    // （删不掉、或删了也没用）——而这个"特殊"在删除生效后就是假的、还会误导。
    // 现在只保留**有实际决策价值**的一个标记：该站路径未实测（= 可能不脱敏）。
    if (self.SILENCER_SHARED.webchatSupport(domain) || reasonFor(domain)) {
      const warn = document.createElement('span');
      warn.className = 'tag warn';
      warn.textContent = t(self.SILENCER_SHARED.webchatSupport(domain) ? 'webchatPending' : 'tagUnsupported');
      li.appendChild(warn);
    }

    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'link danger';
    del.textContent = t('removeBtn');
    del.addEventListener('click', () => removeSite(domain));
    li.appendChild(del);

    ul.appendChild(li);
  }
  if (!cfg.enabledSites.length) {
    const li = document.createElement('li');
    li.style.color = '#6b7280';
    li.textContent = lang === 'zh' ? '（暂无站点）' : 'No sites yet.';
    ul.appendChild(li);
  }
  return cfg;
}

/**
 * 推荐站点 chip。只列**还没添加**的 —— 已添加的在上面列表里，两处重复只会让人
 * 以为加了没生效。chip 与手输走同一条 addDomain()，不另开一条路径。
 * ⚠ 只标在 `verified:false` 的站点上：那是"没实测过"，不是"不能用"。
 */
async function renderPresets() {
  const cfg = await loadConfig();
  const box = $('presetList');
  box.innerHTML = '';
  const missing = PRESET_SITES.filter((p) => !cfg.enabledSites.includes(p.domain));
  for (const p of missing) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'chip';
    btn.title = p.domain;
    btn.textContent = lang === 'zh' ? p.zh : p.en;
    if (!p.verified) {
      const dot = document.createElement('span');
      dot.className = 'dot';
      dot.textContent = '⚠';
      btn.appendChild(dot);
    }
    btn.addEventListener('click', () => addDomain(p.domain));
    box.appendChild(btn);
  }
  $('addAllPresetsBtn').style.display = missing.length ? '' : 'none';
  if (!missing.length) {
    const span = document.createElement('span');
    span.className = 'tag';
    span.textContent = t('presetNone');
    box.appendChild(span);
  }
  return missing;
}

async function renderSnapshot() {
  const snap = await sendToSW({ action: 'admin', op: 'snapshot' });
  if (!snap || !snap.ok) return;
  const used = snap.sessionBytes == null ? '—' : `${Math.round(snap.sessionBytes / 1024)} KB`;
  const budget = `${Math.round((snap.sessionQuotaBytes || 1048576) / 1024)} KB`;
  $('quota').textContent = t('quota', { used, budget, sids: snap.sidCount });
}

// ── 与 SW 通信 ──────────────────────────────────────────────────────────────

function sendToSW(msg) {
  return new Promise((resolve) => {
    try {
      chrome.runtime.sendMessage(msg, (resp) => {
        if (chrome.runtime.lastError) { resolve({ ok: false, error: 'background_unavailable' }); return; }
        resolve(resp === undefined ? { ok: false, error: 'background_unavailable' } : resp);
      });
    } catch (e) {
      resolve({ ok: false, error: 'background_unavailable' });
    }
  });
}

// ── 事件 ────────────────────────────────────────────────────────────────────

async function saveConfig(patch, msgEl) {
  try {
    await chrome.storage.local.set(patch);
    setMsg(msgEl, t('saved'), 'ok');
    return true;
  } catch (e) {
    setMsg(msgEl, t('saveFail'), 'bad');
    return false;
  }
}

async function removeSite(domain) {
  const cfg = await loadConfig();
  const next = cfg.enabledSites.filter((d) => d !== domain);
  await chrome.storage.local.set({ enabledSites: next });
  await sendToSW({ action: 'admin', op: 'resync' });
  await renderSites();
  await renderPresets();
  setMsg($('siteMsg'), t('saved'), 'ok');
}

async function addSite() {
  await addDomain(normalizeDomain($('newDomain').value), true);
}

/**
 * 添加单个站点（手输与推荐站点 chip 共用）。
 *
 * `fromInput` 只影响"成功后要不要清空输入框"，其余五步——校验 → 手势内授权 →
 * 落配置 → SW 重注册 → 如实报注册失败——**只有这一份实现**。两处各写一遍的下场
 * 就是某天只改了一边，用户看到"添加成功"但实际没注入。
 */
async function addDomain(domain, fromInput) {
  if (!domain) { setMsg($('siteMsg'), t('domainInvalid'), 'bad'); return; }

  const cfg = await loadConfig();
  if (cfg.enabledSites.includes(domain)) { setMsg($('siteMsg'), t('domainDuplicate'), 'warn'); return; }

  if (!confirm(t('siteWarn', { domain }))) return;

  // ① 用户手势内申请权限（先于任何网络/存储 await，避免丢失 transient activation）
  let granted = true;
  if (!STATIC_SITES.includes(domain)) {
    try {
      granted = await chrome.permissions.request({ origins: [siteMatchPattern(domain)] });
    } catch (e) {
      granted = false;
    }
  }
  if (!granted) { setMsg($('siteMsg'), t('permDenied'), 'bad'); return; }

  // ② 落配置 → 由 SW 统一重注册（单一写入者）
  const next = cfg.enabledSites.concat([domain]);
  await chrome.storage.local.set({ enabledSites: next });
  const res = await sendToSW({ action: 'admin', op: 'resync' });

  if (fromInput) $('newDomain').value = '';
  await renderSites();
  await renderPresets();
  await renderSnapshot();

  // 关键：**没拿到 SW 的确认就不能报成功**。
  // `sendToSW` 在 lastError / SW 抛异常时会 resolve 成 `{ok:false}`，
  // 而"站点已写进 storage"和"脚本真的注入了"是两件事——前者成功后者失败，用户看到的就是
  // 「已保存」+ 实际不生效，也就是最难排查的那种"加了没反应"。宁可报黄也不能报绿。
  if (!res || res.ok !== true) {
    setMsg($('siteMsg'), t('resyncUnknown'), 'warn');
    return;
  }
  const failed = Array.isArray(res.failed) ? res.failed.filter((f) => f.id.endsWith(domain)) : [];
  if (failed.length) {
    setMsg($('siteMsg'), t('regFailed', { detail: failed[0].error }), 'warn');
  } else {
    setMsg($('siteMsg'), t('saved'), 'ok');
  }
}

/**
 * 一键添加全部推荐站点。
 *
 * **故意只在一次 `permissions.request` 里申请全部 origins**：Chrome 会把同一次请求
 * 合并成一个授权弹窗，用户点一次「允许」就完事。逐个申请会弹十几次，而弹窗一多
 * 用户就会习惯性点掉，反而失去授权的意义。
 */
async function addAllPresets() {
  const cfg = await loadConfig();
  const missing = PRESET_SITES.map((p) => p.domain).filter((d) => !cfg.enabledSites.includes(d));
  if (!missing.length) { setMsg($('siteMsg'), t('presetNone'), 'ok'); return; }

  if (!confirm(t('presetAllWarn', { n: missing.length }))) return;

  let granted = true;
  try {
    granted = await chrome.permissions.request({ origins: missing.map(siteMatchPattern) });
  } catch (e) {
    granted = false;
  }
  if (!granted) { setMsg($('siteMsg'), t('permDenied'), 'bad'); return; }

  await chrome.storage.local.set({ enabledSites: cfg.enabledSites.concat(missing) });
  const res = await sendToSW({ action: 'admin', op: 'resync' });

  await renderSites();
  await renderPresets();
  await renderSnapshot();

  // 同 addDomain：没有 SW 的确认就不报成功（原因见那里的注释）
  if (!res || res.ok !== true) {
    setMsg($('siteMsg'), t('resyncUnknown'), 'warn');
    return;
  }
  const failed = Array.isArray(res.failed) ? res.failed : [];
  if (failed.length) {
    setMsg($('siteMsg'), t('regFailed', { detail: failed[0].error }), 'warn');
  } else {
    // 强调"刷新页面"：动态注册只对新加载的文档注入，不刷新就是"加了没反应"。
    setMsg($('siteMsg'), t('presetAdded', { n: missing.length }), 'ok');
  }
}

function connectionErrorText(error) {
  const keys = {
    invalid_token: 'testInvalidToken',
    ext_bridge_disabled: 'testBridgeDisabled',
    engine_unreachable: 'testUnreachable',
    engine_timeout: 'testTimeout',
    background_unavailable: 'testBackgroundUnavailable',
    invalid_ping_response: 'testInvalidResponse',
  };
  if (keys[error]) return t(keys[error]);
  const http = /^http_(\d+)(?:_cloudflare_(\d+))?$/.exec(error);
  if (http) return t(http[2] ? 'testCloudflareError' : 'testHttpError', { status: http[1], code: http[2] });
  return error || t('testBackgroundUnavailable');
}

async function testConnection() {
  // 优先读取当前输入框中用户刚粘贴/输入的 URL 与 Token 并自动同步存盘，
  // 避免用户未手动滑动到底部点击「保存设置」时被误判为「未填写令牌 (401/403)」。
  const panelUrl = String($('panelUrl').value || '').trim().replace(/\/+$/, '') || DEFAULTS.panelUrl;
  const token = String($('token').value || '').trim();
  const remotePanel = !isLocalPanelUrl(panelUrl);
  setMsg($('testMsg'), '…', '');
  if (!token) { setMsg($('testMsg'), t('testNoToken'), 'bad'); return; }
  if (!isPanelUrl(panelUrl)) { setMsg($('testMsg'), lang === 'zh' ? '网关地址格式不合法。' : 'Invalid gateway URL.', 'bad'); return; }
  if (!isLocalPanelUrl(panelUrl)) {
    setMsg($('testMsg'), lang === 'zh'
      ? '提示：这是远程网关，令牌会发送到该地址。正在请求浏览器访问权限。'
      : 'Warning: this is a remote gateway; the token will be sent there. Requesting browser access.', 'warn');
    if (!await requestPanelPermission(panelUrl, 'testMsg')) return;
  }
  try {
    await chrome.storage.local.set({ panelUrl, token });
  } catch (e) {
    setMsg($('testMsg'), t('saveFail'), 'bad');
    return;
  }
  const res = await sendToSW({ action: 'admin', op: 'ping' });
  const ping = res && res.ping;
  if (ping && ping.alive) {
    if (ping.protocol !== self.SILENCER_SHARED.EXT_PROTOCOL_VERSION) {
      setMsg($('testMsg'), t('testProtocolMismatch', { expected: self.SILENCER_SHARED.EXT_PROTOCOL_VERSION, actual: ping.protocol ?? '?' }), 'bad');
      await renderSnapshot();
      return;
    }
    const stats = ping.stats || { mask: 0, restore: 0 };
    const message = t('testOk', { version: ping.version || '?', mask: stats.mask || 0, restore: stats.restore || 0 });
    setMsg($('testMsg'), message + (remotePanel ? ' ' + t('remoteWarning') : ''), remotePanel ? 'warn' : 'ok');
  } else {
    setMsg($('testMsg'), t('testFail', { err: connectionErrorText((ping && ping.error) || (res && res.error)) }), 'bad');
  }
  await renderSnapshot();
}

async function openPanelSettings() {
  // 旋转令牌**必须**在面板里做：`/api/ext/rotate-token` 是 API_TOKEN 鉴权，
  // 而扩展手里只有 ext_token（SPEC §5.3 的 T9 断言「ext_token 打 rotate-token → 403」）。
  // 原先这里是个"在本机面板旋转"按钮，用 ext_token 去调 —— 它**永远失败**，
  // 用户每次点都只得到一句报错。改成打开面板设置页，是唯一诚实的做法。
  const cfg = await loadConfig();
  const base = String(cfg.panelUrl || '').replace(/\/+$/, '');
  const url = `${isPanelUrl(base) ? base : DEFAULTS.panelUrl}/settings`;
  try {
    await chrome.tabs.create({ url });
    setMsg($('tokenMsg'), t('openPanelOk'), 'ok');
  } catch (e) {
    setMsg($('tokenMsg'), t('openPanelFail', { err: String((e && e.message) || e) }), 'bad');
  }
}

async function init() {
  const cfg = await loadConfig();
  lang = cfg.uiLang === 'en' ? 'en' : cfg.uiLang === 'zh' ? 'zh' : (/^zh/i.test(chrome.i18n.getUILanguage() || '') ? 'zh' : 'en');

  $('panelUrl').value = cfg.panelUrl || DEFAULTS.panelUrl;
  $('token').value = cfg.token || '';
  $('enabled').checked = cfg.enabled !== false;
  $('wideMode').checked = cfg.wideMode === true;

  applyI18n();
  await renderSites();
  await renderPresets();
  await renderSnapshot();

  $('langBtn').addEventListener('click', async () => {
    lang = lang === 'zh' ? 'en' : 'zh';
    await chrome.storage.local.set({ uiLang: lang });
    applyI18n();
    await renderSites();
    await renderPresets();
    await renderSnapshot();
  });

  $('testBtn').addEventListener('click', testConnection);

  $('tokenToggle').addEventListener('click', () => {
    const isPwd = $('token').type === 'password';
    $('token').type = isPwd ? 'text' : 'password';
    $('tokenToggle').textContent = isPwd ? t('hideBtn') : t('showBtn');
  });

  $('openPanelBtn').addEventListener('click', openPanelSettings);
  $('addBtn').addEventListener('click', addSite);
  $('addAllPresetsBtn').addEventListener('click', addAllPresets);

  $('saveBtn').addEventListener('click', async () => {
    const panelUrl = String($('panelUrl').value || '').trim().replace(/\/+$/, '') || DEFAULTS.panelUrl;
    const remotePanel = !isLocalPanelUrl(panelUrl);
    if (!isPanelUrl(panelUrl)) { setMsg($('saveMsg'), lang === 'zh' ? '网关地址格式不合法。' : 'Invalid gateway URL.', 'bad'); return; }
    if (!isLocalPanelUrl(panelUrl)) {
      setMsg($('saveMsg'), lang === 'zh'
        ? '提示：这是远程网关，令牌会发送到该地址。正在请求浏览器访问权限。'
        : 'Warning: this is a remote gateway; the token will be sent there. Requesting browser access.', 'warn');
      await requestPanelPermission(panelUrl);
    }
    await chrome.storage.local.set({
      panelUrl,
      token: String($('token').value || '').trim(),
      enabled: !!$('enabled').checked,
      wideMode: !!$('wideMode').checked,
    });
    // 拦截范围由页面侧在**首次请求时**取一次并缓存（MAIN world 读不到 storage），
    // 所以改完要刷新已打开的网页才生效——和站点增删一样，明说出来免得用户以为没保存。
    setMsg($('scopeMsg'), lang === 'zh'
      ? '已保存。请刷新已打开的网页，新的拦截范围才会生效。'
      : 'Saved. Reload any open page for the new scope to take effect.', 'ok');
    setMsg($('saveMsg'), t('saved'), 'ok');
    if (remotePanel) setMsg($('saveMsg'), t('remoteWarning'), 'warn');
    // 保存后立刻探一次：ping 成功会解开 SW 侧的两个退避窗口（见 background.js
    // 的 AUTH_HOLD_MS 注释）。不探的话，用户"刚把 token 改对"仍要盲等最多 60s，
    // 而这 60s 的流量是**静默未脱敏**的。
    await sendToSW({ action: 'admin', op: 'ping' });
    await renderSnapshot();
  });
}

document.addEventListener('DOMContentLoaded', init);
