/**
 * 轻量 toast（无外部依赖，替代 sonner——保持依赖面最小）
 * 全局单例挂载点：#toastHost
 */
type ToastKind = 'default' | 'error' | 'success'

let host: HTMLElement | null = null

function ensureHost(): HTMLElement {
  if (!host) {
    host = document.createElement('div')
    host.id = 'Silencer-toast-host'
    host.className =
      'fixed top-16 right-5 z-[200] flex flex-col gap-2 pointer-events-none'
    document.body.appendChild(host)
  }
  return host
}

export function toast(message: string, kind: ToastKind = 'success', duration?: number) {
  const el = document.createElement('div')
  const color =
    kind === 'error'
      ? 'text-destructive border-destructive/30'
      : kind === 'success'
        ? 'text-foreground border-border'
        : 'text-foreground border-border'
  el.className = `pointer-events-auto flex items-center gap-2 rounded-lg border bg-card px-3.5 py-2.5 text-sm shadow-lg animate-[toast-in_0.2s_ease] ${color}`
  el.textContent = message
  ensureHost().appendChild(el)

  // 停留时长：错误消息带原因详情，需要比成功提示更长的阅读时间；
  // 未显式传 duration 时按 kind 取默认值（原统一 2.5s 太短，长文案看不完就消失）
  const wait = duration ?? (kind === 'error' ? 6000 : 3500)

  const beginDismiss = () => {
    el.style.opacity = '0'
    el.style.transition = 'opacity 0.2s'
    setTimeout(() => el.remove(), 220)
  }
  let removeTimer = setTimeout(beginDismiss, wait)

  // 鼠标悬停时冻结倒计时（未开始淡出则保持原样），移开后给 1.5s 缓冲再走淡出，
  // 保证「没看完」的场景可以一直按住阅读
  el.addEventListener('mouseenter', () => {
    clearTimeout(removeTimer)
    if (el.style.opacity !== '0') el.style.transition = ''
  })
  el.addEventListener('mouseleave', () => {
    removeTimer = setTimeout(beginDismiss, 1500)
  })
}
