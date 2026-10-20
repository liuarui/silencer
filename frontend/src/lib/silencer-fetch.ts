/**
 * Silencer 统一 API 客户端
 *
 * 所有控制台请求统一从这里发出，浏览器和 Docker 部署使用同源 fetch。
 */
import { useAuthStore } from '@/stores/authStore'
import { processingURL } from './processing-target'
function engineBase(): string {
  return typeof window !== 'undefined' ? window.location.origin : ''
}

export class SilencerApiError extends Error {
  status: number
  body: string
  code?: string

  constructor(status: number, body: string, message?: string, code?: string) {
    super(message || `HTTP ${status}: ${body.slice(0, 200)}`)
    this.status = status
    this.body = body
    this.code = code
  }
}

interface SilencerFetchOptions extends RequestInit {
  processingTarget?: string
  /** 不注入 token（极少数公开端点用） */
  noToken?: boolean
  /** 返回原始 Response（导出下载等场景） */
  raw?: boolean
  /** 请求超时毫秒（默认 15000；传 0 禁用） */
  timeoutMs?: number
}

/** 默认请求超时：15s。引擎本地调用通常 < 200ms，15s 足够覆盖慢查询（日志聚合等）。
 * 导出等长耗时场景调用方应显式传 timeoutMs: 0 或更大值。 */
const DEFAULT_TIMEOUT_MS = 15000

/** 组合调用方 signal 与超时 signal；timeoutMs=0 时直接返回调用方 signal */
function mergeSignal(userSignal: AbortSignal | undefined, timeoutMs: number): AbortSignal | undefined {
  if (timeoutMs <= 0) return userSignal
  const timeoutSignal = AbortSignal.timeout(timeoutMs)
  if (userSignal) {
    // AbortSignal.any 在现代浏览器和 Node 18+ 可用
    if (typeof AbortSignal.any === 'function') return AbortSignal.any([userSignal, timeoutSignal])
    // 兜底：手动转发
    const ctrl = new AbortController()
    userSignal.addEventListener('abort', () => ctrl.abort(userSignal.reason), { once: true })
    timeoutSignal.addEventListener('abort', () => ctrl.abort(timeoutSignal.reason), { once: true })
    return ctrl.signal
  }
  return timeoutSignal
}

export async function silencerFetch<T>(
  url: string,
  options: SilencerFetchOptions = {},
): Promise<T> {
  const { noToken, raw, processingTarget, timeoutMs = DEFAULT_TIMEOUT_MS, ...rest } = options
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(rest.headers as Record<string, string> | undefined),
  }
  if (!noToken) {
    const token = useAuthStore.getState().token
    if (token) headers['X-Silencer-Token'] = token
  }

  // 从 rest 里拆出调用方 signal：`...rest` 会把 `signal: null | undefined` 一起带进
  // fetchOpts，而浏览器 RequestInit.signal 不接受 null。
  const { signal: userSignal, ...restOpts } = rest
  const signal = mergeSignal(userSignal ?? undefined, timeoutMs)
  const fetchOpts = { credentials: 'same-origin' as RequestCredentials, ...restOpts, headers, ...(signal ? { signal } : {}) }

  let resp: Response
  const requestedUrl = processingURL(url, options.method ?? 'GET', options.body, processingTarget)
  try {
    resp = await window.fetch(`${engineBase()}${requestedUrl}`, fetchOpts)
  } catch (err: unknown) {
    if (err instanceof Error && (err.name === 'TimeoutError' || (signal?.aborted && signal.reason?.name === 'TimeoutError'))) {
      throw new SilencerApiError(408, `Request timeout after ${timeoutMs}ms`, 'timeout', 'timeout')
    }
    throw err
  }

  if (!resp.ok) {
    const body = await resp.text().catch(() => '')
    let errCode: string | undefined
    try {
      const parsed = JSON.parse(body)
      if (parsed && typeof parsed.error === 'string') {
        errCode = parsed.error
      }
    } catch {
      // 非 JSON 响应忽略解析
    }

    // 浏览器模式 403：仅当后端明确返回 invalid_token JSON 时，清掉本 tab 保存的 token
    // 让 App 重新弹出输入框。若反向代理（如 Nginx/Cloudflare/WAF）返回 HTML 403 或后端
    // 返回 Origin 校验拦截 (origin_rejected)，绝不清空 token，避免误踢与死锁！
    if (resp.status === 403 && !options.noToken) {
      if (errCode === 'invalid_token') {
        clearBrowserToken()
        useAuthStore.getState().setToken('')
      }
    }
    if (errCode === 'origin_rejected' && typeof window !== 'undefined') {
      window.dispatchEvent(new CustomEvent('silencer:origin_rejected'))
    }
    throw new SilencerApiError(resp.status, body, undefined, errCode)
  }
  if (raw) return resp as unknown as T
  return (await resp.json()) as T
}

/**
 * 浏览器模式 token 存取（Docker / 无头部署）。
 * 只用 sessionStorage：关 tab 即失效，避免共享机器上长期留存。
 */
const BROWSER_TOKEN_KEY = 'silencer_token'

export function readBrowserToken(): string {
  try { return sessionStorage.getItem(BROWSER_TOKEN_KEY) || '' } catch { return '' }
}

export function saveBrowserToken(token: string) {
  try { sessionStorage.setItem(BROWSER_TOKEN_KEY, token) } catch { /* 隐私模式等场景忽略 */ }
}

export function clearBrowserToken() {
  try { sessionStorage.removeItem(BROWSER_TOKEN_KEY) } catch { /* ignore */ }
}

export function engineBaseUrl(): string {
  return engineBase()
}
