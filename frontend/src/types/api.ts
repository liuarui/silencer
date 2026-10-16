/**
 * Silencer API 类型定义
 * 与 Go 网关 `/api` 返回结构对齐（改动后端字段时同步更新）
 */

// ========== 代理状态（/api/status） ==========
export interface UpstreamStatus {
  name: string
  port: number
  listening: boolean
  url: string
  target: string
}

export interface UpstreamConfig {
  name: string
  port: number
  target: string
  base_path?: string
  paths?: string[]
  api_key_header?: string
  extra_headers?: Record<string, string>
  use_proxy?: boolean
}

export interface EgressProxy {
  enabled: boolean
  url: string
}

export interface ProxyStatus {
  version: string
  panel_pid: number
  proxy_running: boolean
  proxy_starting?: boolean
  proxy_stopping?: boolean
  proxy_pid: number | null
  passthrough: boolean
  /** 当前兜底形态："" / "passthrough" / "error" */
  fallback_mode: string
  upstream: string
  capture_mode: 'reverse' | 'explicit' | 'local'
  proxy_port: number
  proxy_url: string
  upstreams: UpstreamConfig[]
  upstream_ports: UpstreamStatus[]
  admin: boolean
  ca_cert_exists: boolean
  uptime: number
  auto_recovered_at: string | null
  auto_recover_fail: string
  data_root: string
  db_size_mib: number
  filter_enabled: boolean
  fail_closed: boolean
  response_scan: boolean
  /** 敏感词统计是否记录明文（默认 true）。关闭后 daily_words 只存打码形态 */
  record_plaintext_words: boolean
  /** 记录请求/响应正文和逐词规则明细（默认关闭，可能包含敏感明文）。 */
  record_detail_data?: boolean
  stream_response: boolean
  stream_exclude_hosts: string[]
  stop_mode: 'error' | 'passthrough' | 'block'
  egress_proxy: EgressProxy
  egress_proxy_users: string[]
  debug: boolean
  auto_start_proxy: boolean
  audit: Record<string, unknown>
  /**
   * 语义实体识别（NER）状态：enabled 是开关，available/initialized 是实际可用性。
   *
   * `skips` 是「开启了但这段没做识别」的原因计数（`too_long` / `budget_exhausted` /
   * `infer_failed` / `deadline` / `init_failed` / `model_missing`）。纯整数、不含原文。
   * 不透出它的话，「开了 NER，长文本全跳过」在界面上完全看不出（审计 M7）。
   */
  ner?: {
    enabled: boolean
    available: boolean
    initialized: boolean
    reason: string
    skips?: Record<string, number>
  }
  needs_ca: boolean
  wizard_recommended: boolean
  last_error: string
}

// ========== 事件（/api/logs） ==========
export type EventType =
  | 'MASK'
  | 'RESTORE'
  | 'PASS'
  | 'BYPASS'
  | 'SKIP'
  | 'BLOCK'
  | 'ERR'
  | 'SCAN_WARN'
  | 'CANCEL'
  | 'DNS_ERROR'

/** 日志列表和详情中的逐词命中明细。 */
export interface SlimItem {
  label: string
  preview: string
  original?: string
  restored?: boolean
  restore_status?: 'restored' | 'not_mentioned' | 'unresolved' | 'unmapped' | 'unknown'
  restored_count?: number
  unresolved_count?: number
}

/** 详情回源（/api/logs/detail）中的完整 item */
export interface EventItem extends SlimItem {
  original?: string
  tok?: string
  sha256?: string
  length?: number
}

export interface SilencerEvent {
  id: number
  detail_id?: string
  /** Completed outcome projected without changing the stored MASK/RESTORE phase. */
  display_type?: EventType
  noise_whitelisted?: boolean
  error?: string
  failure_stage?: string
  mask_count?: number
  /** epoch 秒 */
  ts: number
  type: EventType
  sid: string
  host: string
  method: string
  path: string
  status?: string
  http_status?: number
  count: number
  restored: number
  /**
   * 查不到原文、原样透传给客户端的占位符数（仅 RESTORE 事件）。
   * >0 多数意味着模型自造了一个我们从未生成过的占位符——它把
   * {{LABEL_<token>}} 的后缀当成可计算的数字改写了。这类占位符
   * 无法还原（对应的原文从不存在），只能如实告诉用户。
   */
  unresolved?: number
  /**
   * 靠宽松兜底修回来的占位符数（仅 RESTORE 事件）。模型把 {{}} 剥掉或写残时，
   * _LOOSE_PLACEHOLDER_RX 捞回来的那些。是成功路径，但值得看见——
   * 它说明模型在改写输出格式，是「哪天彻底还原不回来」的前兆。
   */
  degraded?: number
  items: SlimItem[] | EventItem[]
  matched_rules?: string[]
  dialog?: string
  dialog_req?: string
  dialog_resp?: string
  req_preview?: string
  resp_preview?: string
  raw?: string
  masked?: string
  upstream_response?: string
  restored_response?: string
  detail_recorded?: boolean
  body_unavailable?: boolean
  body_expired?: boolean
  body_storage_error?: boolean
  truncated?: number
  stream_mode?: 'stream' | 'whole'
  stream_actual?: 'stream' | 'whole' | 'stream_error'
  mask_ms?: number
  first_byte_ms?: number
  upstream_ms?: number
  total_ms?: number
  /** 本次请求估算费用（USD，后端按 model×usage×价格表估算，仅 RESTORE 事件带） */
  cost_usd?: number
  model?: string
  client_app?: string | number
  upstream?: string
  /**
   * 入口维度：`proxy`=CLI 代理链路，`ext`=浏览器扩展链路。
   * **老数据/导入事件该列为空，读取时按 `proxy` 解读**（不要渲染成"—"）。
   */
  ingress?: 'proxy' | 'ext'
  reason?: string
  msg?: string
  seq: number
}

export interface LogsResponse {
  events: SilencerEvent[]
  tail: string[]
  retention_days: number
  store: string
  db: string
  sensitive_only: boolean
  total: number
  /** Older engines may not include pagination metadata. */
  has_more?: boolean
  next_since?: number
  window_limited?: boolean
  /** 游标重置（清空日志/库隔离重建后 id 从 1 重新开始）：丢弃旧游标从 0 重拉 */
  reset?: boolean
}

export interface LogDetailResponse {
  ok: boolean
  event?: SilencerEvent
  error?: string
}

// ========== 今日统计（/api/stats/today） ==========
/**
 * 前缀保真度：MASK 事件三个诊断字段的聚合（silencer.infrastructure.persistence.event_store._prefix_payload）。
 *
 * 用请求改写位置和占位符复用情况辅助分析上游缓存表现；这些指标不代表上游实际命中率——
 * clean_rate 是请求体一个字节都没被改动的比例（分母 masks），reuse_rate 是
 * **在改写过的请求里**沿用复用表旧 token 的比例（分母 rewritten —— 零改写透传的
 * 请求没签发票据，算进来只会稀释），avg_first_diff 是回写后与客户端原始字节首个
 * 差异位置的平均值。
 */
export interface PrefixStats {
  /** 区间内 MASK 事件数（分母） */
  masks: number
  /** 其中回写过请求体的次数 */
  rewritten: number
  /** body_rewritten=false 的次数（零改写透传） */
  clean: number
  /** 零改写透传占比（0~1） */
  clean_rate: number
  /** 命中占位符沿用复用表旧 token 的次数 */
  suffix_reused: number
  /** 占位符复用占比（0~1，分母是 rewritten）；区间内无回写请求时为 null */
  reuse_rate: number | null
  /** 首个差异字节均值；样本全被上限挡掉时为 null */
  avg_first_diff: number | null
  /** 计入 avg_first_diff 的样本数（0 表示均值不可用） */
  diff_samples: number
}

export interface TodayStats {
  requests: number
  alerts: number
  /** Mutually exclusive primary categories, deduplicated by request. */
  alert_counts?: Record<'block' | 'restore_failed' | 'error' | 'response_warning', number>
  by_type?: Record<string, number>
  /** MASK 事件数（真实字段 mask_events） */
  mask_events: number
  /** 脱敏词项总数（真实字段 masked_items） */
  masked_items: number
  restored: number
  restore_ok: number
  restore_failed: number
  restore_by_status?: Record<string, number>
  tokens: { prompt: number; completion: number }
  /** 上游模型返回的真实 Context Cache usage；无供应商字段时 observed_requests 为 0。 */
  cache?: {
    read_tokens: number
    write_tokens: number
    observed_requests: number
    rate_observed_requests: number
    input_tokens: number
    hit_rate: number | null
    source: 'upstream_usage'
  }
  /**
   * 前缀保真度。**null = 本区间没有 MASK 事件样本**（空库 / 老库升级当天），
   * 与「有样本但零改写率为 0」是两回事，渲染时必须分开。
   */
  prefix?: PrefixStats | null
  by_label: Record<string, number>
  top_words: { label: string; word: string; count: number; original?: string; display?: 'plaintext' | 'placeholder' }[]
  /**
   * 词表**按入口分组**的同一份数据（key: `proxy` / `ext`）。分组同屏而非过滤：
   * 浏览器扩展链路的量级远大于 CLI，混算会把邮箱/电话这类高频词刷上榜，
   * 压掉用户真正关心的业务密钥词；分组后各组各取 Top N，两组都可见。
   */
  words_by_ingress?: Record<string, IngressWordGroup>
  [key: string]: unknown
}

/** 单个入口（proxy=CLI 代理链路 / ext=浏览器扩展链路）的词表视图。 */
export interface IngressWordGroup {
  by_label: Record<string, number>
  by_label_words: Record<string, { word: string; count: number; original?: string; display?: 'plaintext' | 'placeholder' }[]>
  top_words: { label: string; word: string; count: number; original?: string; display?: 'plaintext' | 'placeholder' }[]
  /** 该入口下的命中总数（组头计数；与 top_words 的 Top N 截断无关） */
  label_total: number
}

// ========== 配置（/api/config） ==========
export interface MaskRule {
  label: string
  pattern?: string
  enabled?: boolean
  type?: string
  [key: string]: unknown
}

export interface SilencerConfig {
  /** Actual Go HTTP listener address, for client setup URLs. */
  listen_addr?: string
  upstreams: UpstreamConfig[]
  client_profiles?: ClientProfile[]
  fail_closed: boolean
  filter_enabled: boolean
  response_scan: boolean
  /** 控制面 Origin 校验开关（默认 true）。反代/CDN 回源 Origin 不匹配致面板 403 时可关 */
  origin_check?: boolean
  /** 敏感词统计是否记录明文（默认 true）。关闭后 daily_words 只存打码形态 */
  record_plaintext_words: boolean
  stream_response: boolean
  stream_exclude_hosts: string[]
  stop_mode: 'error' | 'passthrough' | 'block'
  egress_proxy: EgressProxy
  capture_mode: 'reverse' | 'explicit' | 'local'
  model_prices?: Record<string, { input: number; output: number }>
  price_sync_enabled?: boolean
  price_sync_url?: string
  auto_start_proxy: boolean
  debug: boolean
  log_retention_days?: number
  log_noise_allowlist?: { method: string; path: string; status: 404 }[]
  session_ttl?: number
  diagnostic_unmatched?: boolean
  http2?: boolean
  /** 浏览器扩展链路总开关（默认关）。关闭时三个 /api/ext/* 端点 403 `ext_bridge_disabled` */
  ext_bridge_enabled?: boolean
  /**
   * 扩展访问令牌。**面板只回显，不进任何日志/导出/诊断包**。
   * 轮换即扩展失效（403 invalid_token → 直通、未脱敏）直到用户在扩展设置里更新。
   */
  ext_token?: string
  /** 引擎不可达时扩展侧是否阻断（默认关=直通）。**只管 (B) 类**，(A) 类无开关 */
  ext_block_when_engine_down?: boolean
  /** 扩展链路是否写入本地事件库与统计（默认 true）。只管落库，不管脱敏 */
  ext_record_events?: boolean
  target_domains?: string[]
  api_paths?: string[]
  wizard_done?: boolean
  data_root?: string
  db_size_mib?: number
  sensitive?: Record<string, string[]>
  sensitive_disabled?: string[]
  sensitive_word_disabled?: Record<string, string[]>
  /** 整词匹配开关：开启后该词两侧加边界。 */
  sensitive_word_whole?: string[]
  builtin_rules?: Record<string, boolean>
  secret_prefixes?: string[]
  audit?: Record<string, unknown>
  _meta?: {
    builtin_rule_meta: Record<string, unknown>
    version: string
  }
  [key: string]: unknown
}

export interface ClientProfile {
  name: string
  upstream: string
  protocol: 'openai' | 'anthropic' | 'gemini' | 'custom'
}

// ========== 审计（/api/audit/*） ==========
export type AuditSeverity = 'LOW' | 'MEDIUM' | 'HIGH' | 'CRITICAL'

export interface AuditEvent {
  seq: number
  /** epoch 秒 */
  ts: number
  sid?: string
  host?: string
  method?: string
  path?: string
  signal_type: string
  severity: AuditSeverity
  evidence?: string
  action?: 'block' | 'rewrite' | 'observe'
  confirmation_required?: boolean
  tool_guard_rules?: string[]
  probe_id?: string
  request_hash?: string
  response_hash?: string
  [key: string]: unknown
}

export interface AuditEventsResponse {
  events: AuditEvent[]
  count: number
}

export interface AuditJob {
  running: boolean
  started_at?: number
  done?: number
  total?: number
  phase?: string
  error?: string
  result?: unknown
  [key: string]: unknown
}
