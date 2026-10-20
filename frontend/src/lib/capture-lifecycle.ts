import type { CaptureStatus } from '@/api/capture'

export const UNINSTALL_PROGRESS_KEY = 'silencer.capture.uninstall.v1'
const maxProgressAge = 7 * 24 * 60 * 60 * 1000
type Platform = 'darwin' | 'windows' | 'linux' | ''
export interface UninstallProgress {
  version: 1
  active: boolean
  step: number
  keepCertificate: boolean
  downloadedAt: number
  platform: Platform
  certificateSha: string
  installRevision: string
  updatedAt: number
}

// Deliberately select individual, non-sensitive fields. Never persist the API
// status/config object, credentials, proxy URLs, logs, or a success assertion.
export function cleanUninstallProgress(value: unknown, now = Date.now()): UninstallProgress | null {
  if (!value || typeof value !== 'object') return null
  const data = value as Record<string, unknown>
  if (data.version !== 1 || typeof data.updatedAt !== 'number' || !Number.isFinite(data.updatedAt) || now - data.updatedAt > maxProgressAge || data.updatedAt > now + 60000) return null
  return {
    version: 1, active: data.active === true,
    step: typeof data.step === 'number' && Number.isInteger(data.step) && data.step >= 0 && data.step <= 2 ? data.step : 0,
    keepCertificate: data.keepCertificate === true,
    downloadedAt: typeof data.downloadedAt === 'number' && Number.isFinite(data.downloadedAt) && data.downloadedAt > 0 && data.downloadedAt <= now + 60000 ? data.downloadedAt : 0,
    platform: data.platform === 'darwin' || data.platform === 'windows' || data.platform === 'linux' ? data.platform : '',
    certificateSha: typeof data.certificateSha === 'string' && /^[A-Fa-f0-9]{64}$/.test(data.certificateSha) ? data.certificateSha.toUpperCase() : '',
    installRevision: typeof data.installRevision === 'string' && /^\d{1,20}$/.test(data.installRevision) ? data.installRevision : '',
    updatedAt: data.updatedAt,
  }
}

export function readUninstallProgress(storage: Pick<Storage, 'getItem'>, now = Date.now()) {
  try { return cleanUninstallProgress(JSON.parse(storage.getItem(UNINSTALL_PROGRESS_KEY) ?? 'null'), now) } catch { return null }
}
export function writeUninstallProgress(storage: Pick<Storage, 'setItem' | 'removeItem'>, value: UninstallProgress | null) {
  try {
    const safe = cleanUninstallProgress(value)
    if (safe) storage.setItem(UNINSTALL_PROGRESS_KEY, JSON.stringify(safe))
    else storage.removeItem(UNINSTALL_PROGRESS_KEY)
  } catch { /* Storage can be disabled; the current session remains usable. */ }
}
export function newUninstallProgress(status?: CaptureStatus, now = Date.now()): UninstallProgress {
  return { version: 1, active: true, step: 0, keepCertificate: false, downloadedAt: 0, platform: status?.platform as Platform || '', certificateSha: status?.certificate?.sha256 ?? '', installRevision: status?.uninstall?.install_revision ?? '', updatedAt: now }
}
export function reconcileUninstallProgress(progress: UninstallProgress, status: CaptureStatus, now = Date.now()) {
  const revision = status.uninstall?.install_revision ?? ''
  if (progress.installRevision && revision && progress.installRevision !== revision) {
    return { progress: { ...newUninstallProgress(status, now), active: false }, reason: 'installation_changed' as const }
  }
  const certificateSha = status.certificate?.sha256 ?? ''
  const stopped = status.uninstall?.capture_stopped === true
  const changed = progress.certificateSha !== certificateSha
  const next = { ...progress, platform: status.platform as Platform, certificateSha, installRevision: revision }
  if (!stopped) { next.step = 0; next.downloadedAt = 0 }
  else if (changed) { next.step = Math.min(next.step, 1); next.downloadedAt = 0 }
  return { progress: next, reason: changed ? 'certificate_changed' as const : null }
}

export function uninstallDownloadMatches(current: UninstallProgress, requested: UninstallProgress) {
  return current.active && current.step === 2 && current.platform === requested.platform && current.certificateSha === requested.certificateSha && current.installRevision === requested.installRevision && current.keepCertificate === requested.keepCertificate
}
export function recordUninstallDownload(current: UninstallProgress, requested: UninstallProgress, verified: boolean, now = Date.now()) {
  return verified && uninstallDownloadMatches(current, requested) ? { ...current, downloadedAt: now, updatedAt: now } : current
}

export function captureStatusUsable(status: CaptureStatus | undefined, fetchedAfterMount: boolean, failed: boolean) {
  return !!status && fetchedAfterMount && !failed
}
export function capturePrerequisitesReady(status: CaptureStatus | undefined) {
  return !!status && Object.values(status.checks).every((check) => check.ok)
}
export function captureIsActive(status: CaptureStatus | undefined) {
  return !!status && status.config.enabled && status.state === 'running' && capturePrerequisitesReady(status)
}
export function captureAttentionReasons(status: CaptureStatus | undefined) {
  if (!status) return []
  const reasons: { key: keyof CaptureStatus['checks'] | 'runtime' | 'targets' | 'last_error'; code: string }[] = []
  for (const key of ['platform', 'component', 'permissions', 'certificate', 'pipeline'] as const) {
    if (!status.checks[key].ok) reasons.push({ key, code: status.checks[key].error || 'capture_step_check_failed' })
  }
  if (status.config.enabled && status.runtime) {
    if (!status.runtime.process_running && status.checks.permissions.ok) reasons.push({ key: 'runtime', code: 'capture_process_exited' })
    else if (!status.runtime.heartbeat_ready && status.checks.permissions.ok) reasons.push({ key: 'runtime', code: 'capture_heartbeat_stale' })
    else if (status.runtime.heartbeat_ready && !status.runtime.targets_applied) reasons.push({ key: 'targets', code: 'capture_targets_not_applied' })
  }
  if (status.last_error && !reasons.some((reason) => reason.code === status.last_error)) reasons.push({ key: 'last_error', code: status.last_error })
  return reasons
}
export function captureSetupStepLimit(status: CaptureStatus, processorMatches = true) {
  const c = status.checks
  return !c.component.ok || !c.platform.ok ? 1 : !c.permissions.ok ? 2 : !c.certificate.ok ? 3 : !c.pipeline.ok || !processorMatches ? 4 : 5
}
export function isCaptureStatus(value: unknown): value is CaptureStatus {
  if (!value || typeof value !== 'object') return false
  const data = value as Partial<CaptureStatus>
  return typeof data.state === 'string' && !!data.config && !!data.checks && typeof data.platform === 'string'
}
