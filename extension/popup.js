/* Silencer Browser Bridge — 弹出面板逻辑
 *
 * 目标：回答两个问题 —— ①扩展/引擎现在是什么状态；②**到底生效没有**。
 * (B) 类直通是**静默不脱敏**，只看 badge 与累计计数完全无感，所以这里必须有
 * 「本页最近请求」一屏（读 SW 写在 chrome.storage.session 的元数据缓冲）。
 *
 * 安全边界：只读元数据（ts/host/path/action/count/status）。
 * 绝不读取、展示、打印 body / 占位符 / 原文 / 响应文本；不用 storage.sync。
 */

'use strict';

const { siteCovers, unsupportedReason, EXT_PROTOCOL_VERSION } = self.SILENCER_SHARED;

const I18N = {
  zh: {
    kPage: '当前页',
    kActive: '是否生效',
    kToday: '今日计数',
    kEngine: '引擎累计',
    activeYes: '已启用',
    activeTextAdapter: '文字适配已启用 · 待请求核验',
    activeNoSite: '未生效：站点未启用',
    activeNoPath: '未生效：接口未实测',
    activeNotWeb: '未生效：非网页',
    statusOk: '引擎正常',
    statusDown: '引擎未运行，已直通 {n}s',
    statusBlocked: '引擎异常，已阻断',
    protoWarn:
      '扩展与网关协议不兼容（扩展 v{p} / 网关 v{e}），已阻断识别到的文字请求。请将网关和扩展更新为配套版本。',
    statusInvalidToken: 'token 失效，请到设置更新',
    statusInvalidTokenHold: 'token 失效，请到设置更新（已降速重试中）',
    statusDisabledPanel: '扩展已关闭（面板开关）',
    statusDisabledPanelHold: '扩展已关闭（面板开关，已降速重试中）',
    statusDisabledLocal: '扩展已停用（本扩展总开关）',
    statusPassthrough: '引擎状态未识别，已直通',
    statusUnknown: '尚未检测',
    lastBlocked: '最近一次请求被阻断：{err}',
    recentTitle: '本页最近请求',
    recentTitleAll: '最近请求（全部标签页）',
    scopePage: '只看本页',
    scopeAll: '全部标签页',
    recentEmpty: '暂无记录。请求发出后这里会逐条出现 —— 没有记录本身就是证据。',
    unmatchedTitle: '本页未命中白名单的 POST',
    // 引擎不在时的引导（扩展不能独立工作，必须明说去哪装）
    guideTitle: '未检测到本地程序',
    guideBody: '本扩展只负责转发，规则引擎在配置的网关里。网关不可达时网页照常使用，但内容不会被脱敏。',
    refreshBtn: '刷新',
    optionsBtn: '设置',
    colTime: '时间',
    colPath: 'path',
    colAction: '动作',
    colCount: '命中',
    colStatus: '状态',
    dash: '—'
  },
  en: {
    kPage: 'Page',
    kActive: 'Active',
    kToday: 'Today',
    kEngine: 'Engine totals',
    activeYes: 'Enabled',
    activeTextAdapter: 'Text adapter enabled · verify a real request',
    activeNoSite: 'Not active: site disabled',
    activeNoPath: 'Not active: unverified',
    activeNotWeb: 'Not active: not a web page',
    statusOk: 'Engine OK',
    statusDown: 'Engine not running, passthrough {n}s',
    statusBlocked: 'Engine error, blocking',
    protoWarn:
      'Extension/gateway protocol incompatible (extension v{p} / gateway v{e}). Recognized text requests are blocked. Update both to matching builds.',
    statusInvalidToken: 'Token invalid, update it in settings',
    statusInvalidTokenHold: 'Token invalid — update it in settings (retrying at low rate)',
    statusDisabledPanel: 'Extension bridge disabled (panel switch)',
    statusDisabledPanelHold: 'Extension bridge disabled (panel switch; retrying at low rate)',
    statusDisabledLocal: 'Bridge turned off (extension master switch)',
    statusPassthrough: 'Engine state unrecognized, passthrough',
    statusUnknown: 'Not checked yet',
    lastBlocked: 'Last request was blocked: {err}',
    recentTitle: 'Recent requests on this page',
    recentTitleAll: 'Recent requests (all tabs)',
    scopePage: 'This page only',
    scopeAll: 'All tabs',
    recentEmpty: 'No records yet. Entries appear as requests go out — an empty list is itself evidence.',
    unmatchedTitle: 'POST paths missing the whitelist on this page',
    // Engine-missing guidance (the extension cannot work standalone)
    guideTitle: 'Local app not detected',
    guideBody: 'This extension forwards requests to the configured gateway. Without a reachable gateway, pages work normally, but nothing gets masked.',
    refreshBtn: 'Refresh',
    optionsBtn: 'Settings',
    colTime: 'Time',
    colPath: 'path',
    colAction: 'Action',
    colCount: 'Hits',
    colStatus: 'Status',
    dash: '—'
  }
};

let lang = 'zh';
let scope = 'page';
let currentHost = '';
let currentTabId = null;
let lastSnapshot = null;
let tickTimer = null;

const $ = (id) => document.getElementById(id);

function t(key, vars) {
  let s = (I18N[lang] && I18N[lang][key]) || (I18N.zh[key] || key);
  if (vars) for (const [k, v] of Object.entries(vars)) s = s.replaceAll(`{${k}}`, String(v));
  return s;
}

function applyStaticI18n() {
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  for (const el of document.querySelectorAll('[data-i18n]')) el.textContent = t(el.dataset.i18n);
}

function sendToSW(msg) {
  return new Promise((resolve) => {
    try {
      chrome.runtime.sendMessage(msg, (resp) => {
        if (chrome.runtime.lastError) { resolve(null); return; }
        resolve(resp === undefined ? null : resp);
      });
    } catch (e) {
      resolve(null);
    }
  });
}

// ── 渲染 ────────────────────────────────────────────────────────────────────

function fmtTime(ts) {
  try {
    const d = new Date(ts);
    const p = (n) => String(n).padStart(2, '0');
    return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  } catch (e) {
    return t('dash');
  }
}

function setDot(kind) {
  $('dot').className = 'dot' + (kind ? ' ' + kind : '');
}

/**
 * 状态优先级（§3.6 + §5.4）：
 *  本地总开关关 → 面板开关关(ext_bridge_disabled) → token 失效(invalid_token 直通期间持续红标)
 *  → 引擎在 → 最近一次被阻断 → 直通中 → 尚未检测
 */
function renderStatus(snap) {
  const st = (snap && snap.status) || { engine: 'unknown', detail: '', at: 0 };
  const cfg = (snap && snap.config) || {};
  const alive = !!(snap && snap.cache && snap.cache.alive);
  const downMs = (snap && snap.downRemainingMs) || 0;
  const downSec = Math.ceil(downMs / 1000);
  // 403 配置性拒绝的退避中（token 失效 / 面板关开关）。必须显式写出来：退避期内
  // 扩展不再逐 chunk 打 panel（只每 5s 探测一次），用户会看到"红标纹丝不动"，
  // 不说清楚就会以为扩展卡死了。
  const authHeld = ((snap && snap.authHoldRemainingMs) || 0) > 0;
  let kind = '';                       // '' | ok | warn | bad
  let text = '';
  let detail = '';

  if (cfg.enabled === false) {
    kind = 'bad'; text = t('statusDisabledLocal');
  } else if (st.engine === 'disabled') {
    kind = 'bad';
    text = authHeld ? t('statusDisabledPanelHold') : t('statusDisabledPanel');
  } else if (st.engine === 'invalid_token') {
    kind = 'bad';
    text = authHeld ? t('statusInvalidTokenHold') : t('statusInvalidToken');
  } else if (alive) {
    kind = 'ok'; text = t('statusOk');
    if (st.engine === 'error' && st.detail) detail = t('lastBlocked', { err: st.detail });
  } else if (st.engine === 'error') {
    kind = 'bad'; text = t('statusBlocked'); detail = st.detail || '';
  } else if (downSec > 0) {
    kind = 'warn'; text = t('statusDown', { n: downSec });
  } else if (st.engine === 'passthrough') {
    kind = 'warn'; text = t('statusPassthrough'); detail = st.detail || '';
  } else {
    kind = ''; text = t('statusUnknown');
  }

  setDot(kind);
  $('statusText').textContent = text;
  $('statusDetail').textContent = detail;
}

function activeState(cfg) {
  if (!currentHost) return { yes: false, text: t('activeNotWeb'), kind: 'bad' };
  const sites = cfg.enabledSites || [];
  // 覆盖判定与「路径未实测」判定都走 shared.js：options 页给同一站点打的标签
  // 必须与这里显示的「已生效 / 未生效」一致，否则用户会看到自相矛盾的两屏。
  const coveredSite = siteCovers(currentHost, sites);
  if (!coveredSite) return { yes: false, text: t('activeNoSite'), kind: 'warn' };
  if (self.SILENCER_SHARED.webchatSupport(currentHost)) {
    return { yes: true, text: t('activeTextAdapter'), kind: 'warn' };
  }
  if (unsupportedReason(currentHost, coveredSite)) {
    // 开启广泛模式时未实测路径也会自动脱敏；精准模式下才提示未实测
    if (!cfg.wideMode) {
      return { yes: false, text: t('activeNoPath'), kind: 'warn' };
    }
  }
  return { yes: true, text: t('activeYes'), kind: 'ok' };
}

function renderRecent(snap) {
  const all = (snap && snap.recent) || [];
  const list = scope === 'page' && currentHost ? all.filter((r) => r && r.host === currentHost) : all;
  $('recentTitle').textContent = scope === 'page' ? t('recentTitle') : t('recentTitleAll');
  $('scopeBtn').textContent = scope === 'page' ? t('scopeAll') : t('scopePage');

  const wrap = $('recentWrap');
  wrap.innerHTML = '';
  if (!list.length) {
    const d = document.createElement('div');
    d.className = 'empty';
    d.textContent = t('recentEmpty');
    wrap.appendChild(d);
    return;
  }
  const table = document.createElement('table');
  const head = document.createElement('tr');
  for (const k of ['colTime', 'colPath', 'colAction', 'colCount', 'colStatus']) {
    const th = document.createElement('th');
    th.textContent = t(k);
    head.appendChild(th);
  }
  table.appendChild(head);

  for (const r of list.slice(0, 50)) {
    const tr = document.createElement('tr');
    const action = String(r.action || '');
    const status = String(r.status || '');
    const cells = [
      { text: fmtTime(r.ts), mono: true },
      { text: String(r.path || '') + (scope === 'all' && r.host ? ` · ${r.host}` : ''), mono: true },
      { text: action, tag: action === 'mask' ? 'ok' : action === 'restore' ? 'ok' : 'warn' },
      { text: r.count ? String(r.count) : t('dash'), mono: true },
      { text: status, tag: status.indexOf('passthrough') === 0 || status === 'skip' ? 'warn' : status === 'ok' ? 'ok' : 'bad' },
    ];
    for (const c of cells) {
      const td = document.createElement('td');
      if (c.mono) td.className = 'mono';
      if (c.tag) {
        const span = document.createElement('span');
        span.className = 'tag ' + c.tag;
        span.textContent = c.text;
        td.appendChild(span);
      } else {
        td.textContent = c.text;
      }
      tr.appendChild(td);
    }
    table.appendChild(tr);
  }
  wrap.appendChild(table);
}

function renderUnmatched(snap) {
  const map = (snap && snap.unmatched) || {};
  const paths = (currentTabId != null && map[String(currentTabId)]) || [];
  const box = $('unmatchedBox');
  const ul = $('unmatchedList');
  ul.innerHTML = '';
  if (!paths.length) { box.hidden = true; return; }
  box.hidden = false;
  for (const p of paths.slice(-40)) {
    const li = document.createElement('li');
    li.textContent = String(p);
    ul.appendChild(li);
  }
}

/**
 * 引擎不在时给出去向。
 *
 * 必须区分两种「引擎没回应」：**连不上**（没装 / 没启动）与 **403**（装了，但 token 错
 * 或面板把桥关了）。只有前者该引导去下载——对后者说「去装程序」会把排查方向彻底带偏，
 * 用户会重装一遍然后发现还是同一个 403。
 */
function renderGuide(snap) {
  const st = (snap && snap.status) || {};
  const alive = !!(snap && snap.cache && snap.cache.alive);
  // invalid_token / disabled 恰恰**证明引擎在跑**（是它在回 403），不引导下载
  const engineUp = alive || st.engine === 'invalid_token' || st.engine === 'disabled' || st.engine === 'ok';
  const box = $('guideBox');
  box.hidden = engineUp;
  if (engineUp) return;
  $('guideTitle').textContent = t('guideTitle');
  $('guideBody').textContent = t('guideBody');
}

/**
 * 协议不匹配告警。
 *
 * 【为什么不复用上面那行状态】状态行表达的是「引擎可达 / 不可达 / 已阻断」，而协议
 * 不匹配恰恰发生在**引擎完全正常**的时候——塞进状态行等于把「引擎是好的」说成
 * 「引擎有问题」，直接误导排查方向。
 *
 * 触发条件的本质：客户端改了 `/api/ext/*` 的契约，而用户没重载扩展。此时脱敏语义
 * 可能已经不对（例如某字段被改名，扩展读到 undefined 却当成空值继续跑）。
 */
function renderProto(snap) {
  const box = $('protoBox');
  const mismatch = !!(snap && snap.protoMismatch);
  box.hidden = !mismatch;
  if (!mismatch) return;
  const engineProto = (snap && snap.engineProtocol != null) ? snap.engineProtocol : '?';
  $('protoText').textContent = t('protoWarn', {
    p: String(EXT_PROTOCOL_VERSION), e: String(engineProto),
  });
}

function render(snap) {
  lastSnapshot = snap;
  const cfg = (snap && snap.config) || {};
  const active = activeState(cfg);

  $('pageHost').textContent = currentHost || t('dash');
  $('pageActive').textContent = active.text;

  const daily = (snap && snap.daily) || null;
  $('today').textContent = daily && daily.day
    ? `mask ${daily.mask || 0} · restore ${daily.restore || 0} · skip ${daily.skip || 0}`
    : t('dash');

  const stats = (snap && snap.cache && snap.cache.stats) || null;
  const pin = (snap && snap.cache && snap.cache.version) || '';
  $('engineStats').textContent = stats
    ? `mask ${stats.mask || 0} · restore ${stats.restore || 0}${pin ? ' · v' + pin : ''}`
    : t('dash');

  renderStatus(snap);
  renderGuide(snap);
  renderProto(snap);
  renderRecent(snap);
  renderUnmatched(snap);
}

// ── 生命周期 ────────────────────────────────────────────────────────────────

async function refresh() {
  // 请求 SW 重新 ping 一次并回传快照（popup 打开即是用户主动查看，值得一次实时探测）
  await sendToSW({ action: 'admin', op: 'ping' }).catch(() => {});
  const snap = await sendToSW({ action: 'admin', op: 'snapshot' });
  if (snap && snap.ok) {
    // 渲染失败必须被关在这一层：popup 只有「刷新 / 设置」两个按钮，任何一次未捕获异常
    // 都会把 init() 掐断在绑定事件之前，两个按钮一起变死键——而「设置」恰恰是配置出问题
    // 时唯一的出口。（实测事故：popup.html 少写 id="recentTitle"，render 抛 TypeError，
    // 「设置」点了一点反应都没有。）
    try {
      render(snap);
    } catch (e) {
      console.error('[Silencer] popup 渲染失败（按钮仍可用）:', e);
    }
  }

  // 「已直通 Ns」需要逐秒走字，否则用户看不到倒计时。
  // 403 退避（invalid_token / disabled）的文案是静态的（"已降速重试中"），不需要逐秒重绘。
  if (tickTimer) clearInterval(tickTimer);
  tickTimer = setInterval(() => {
    if (!lastSnapshot) return;
    if (lastSnapshot.downRemainingMs > 0) {
      lastSnapshot.downRemainingMs = Math.max(0, lastSnapshot.downRemainingMs - 1000);
      renderStatus(lastSnapshot);
    } else {
      clearInterval(tickTimer);
      tickTimer = null;
    }
  }, 1000);
}

/**
 * 绑定三个交互。**必须在 `refresh()` 之前调用**：拉数据/渲染失败时用户仍要能点「刷新」
 * 重试、点「设置」去改配置——把绑定放在 await 之后，任何异常都会让按钮全成死键。
 */
function bindControls() {
  $('refreshBtn').addEventListener('click', refresh);
  $('scopeBtn').addEventListener('click', () => {
    scope = scope === 'page' ? 'all' : 'page';
    if (lastSnapshot) { renderRecent(lastSnapshot); renderStatus(lastSnapshot); }
    $('scopeBtn').textContent = scope === 'page' ? t('scopeAll') : t('scopePage');
    $('recentTitle').textContent = scope === 'page' ? t('recentTitle') : t('recentTitleAll');
  });
  $('optionsBtn').addEventListener('click', () => chrome.runtime.openOptionsPage());
}

async function init() {
  const stored = await chrome.storage.local.get(['uiLang']).catch(() => ({}));
  lang = stored && stored.uiLang === 'en'
    ? 'en'
    : stored && stored.uiLang === 'zh'
      ? 'zh'
      : (/^zh/i.test(chrome.i18n.getUILanguage() || '') ? 'zh' : 'en');

  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    const tab = tabs && tabs[0];
    if (tab) {
      currentTabId = tab.id;
      try { currentHost = new URL(tab.url).hostname; } catch (e) { currentHost = ''; }
    }
  } catch (e) { /* tabs 权限被拒或非普通页面：按"非网页"处理 */ }

  applyStaticI18n();
  bindControls();
  await refresh();
}

document.addEventListener('DOMContentLoaded', init);
