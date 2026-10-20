// Resolve the destination before starting a request. Never re-read it after an
// await: a response and its cache belong to the processor that received it.
let local = false
let target = 'local'
const listeners = new Set<() => void>()
export function processingScope(): string { return local ? target : 'server' }
export function subscribeProcessing(listener: () => void) { listeners.add(listener); return () => { listeners.delete(listener) } }
export function setProcessingTarget(isLocal: boolean, id = 'local') {
  if (local === isLocal && target === id) return
  local = isLocal; target = id
  for (const listener of listeners) listener()
}
const configFields = new Set(['sensitive', 'sensitive_disabled', 'sensitive_word_disabled', 'sensitive_word_whole', 'builtin_rules', 'secret_prefixes', 'audit', 'log_noise_allowlist', 'record_detail_data', 'record_plaintext_words', 'log_retention_days', 'upstreams', 'egress_proxy', 'filter_enabled', 'response_scan', 'stream_response'])
export function processingURL(url: string, method = 'GET', body?: BodyInit | null, id = target): string {
  if (url.startsWith('/api/config/backups') || url.startsWith('/api/config/backup') || url.startsWith('/api/config/restore')) return url
  if (!local || !/^\/api\/(?:config|logs|requests|stats|audit|demo|upstream)(?:[/?]|$)/.test(url)) return url
  if (method !== 'GET' && url.startsWith('/api/config') && typeof body === 'string') {
    try {
      const data = JSON.parse(body) as Record<string, unknown>
      const fields = url.startsWith('/api/config/patch') ? [String(data.key)] : url === '/api/config' ? Object.keys(data) : []
      if (fields.some((key) => !configFields.has(key))) return url
    } catch { return url }
  }
  return `/api/processing/${encodeURIComponent(id)}/${url.slice('/api/'.length)}`
}
