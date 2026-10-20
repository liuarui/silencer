import { silencerFetch } from '@/lib/silencer-fetch'
export interface CaptureConfig {
  setup_complete: boolean; enabled: boolean; mode: 'local' | 'remote'; active_id: string; targets: string[]
  remote_proxy: string; model_proxy: string; auto_start: boolean
  profiles: { id: string; url: string; credential_configured: boolean }[]
}
export interface CaptureStatus {
  uninstall?: { managed_install: boolean; capture_stopped: boolean; install_revision?: string }
  runtime?: { process_running: boolean; heartbeat_ready: boolean; targets_applied: boolean }
  certificate?: { subject: string; sha256: string; filename: string } | null
  job?: { id: string; action: string; state: string; error?: string }; state: string; platform: string; arch: string; config: CaptureConfig
  checks: Record<'component' | 'platform' | 'permissions' | 'certificate' | 'pipeline', { ok: boolean; error: string }>
  last_error: string; observed_requests: number; last_observed_at: string; runtime_version: string
}
export interface Capabilities { protocol: number; local: boolean; remote_processing: boolean; runtime_version: string; platform: string; arch: string; release_base?: string }
export const getCapabilities = () => silencerFetch<Capabilities>('/api/capabilities')
export const getCaptureStatus = () => silencerFetch<CaptureStatus>('/api/local/setup/status')
export const downloadCaptureCertificate = async () => (await silencerFetch<Response>('/api/local/setup/certificate', { raw: true })).blob()
export const downloadCaptureUninstaller = async (keepCertificate: boolean) => (await silencerFetch<Response>(`/api/local/setup/uninstall-script?keep_certificate=${keepCertificate ? '1' : '0'}`, { raw: true })).blob()
export const captureAction = (action: string) => silencerFetch<CaptureStatus>('/api/local/setup/actions', { method: 'POST', body: JSON.stringify({ action }), timeoutMs: 120000 })
export const saveCaptureConfig = (patch: Pick<CaptureConfig, 'targets' | 'remote_proxy' | 'model_proxy'>) => silencerFetch<CaptureConfig>('/api/local/capture/config', { method: 'POST', body: JSON.stringify(patch) })
export const saveProcessing = (mode: 'local' | 'remote', url = '', token = '') => silencerFetch<CaptureConfig>('/api/local/processing/config', { method: 'POST', body: JSON.stringify({ mode, url, token }), timeoutMs: 30000 })
