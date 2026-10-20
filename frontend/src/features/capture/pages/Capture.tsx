import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, Circle, Copy, Download, RefreshCw, ShieldCheck, TriangleAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { captureAction, downloadCaptureCertificate, getCapabilities, getCaptureStatus, saveCaptureConfig, saveProcessing } from '@/api/capture'
import { CertificateGuide, PermissionGuide } from '../components/SetupGuides'
import { UninstallGuide } from '../components/UninstallGuide'
import { CaptureReadiness } from '../components/CaptureReadiness'
import { captureIsActive, captureSetupStepLimit, captureStatusUsable, isCaptureStatus, newUninstallProgress, readUninstallProgress, reconcileUninstallProgress, recordUninstallDownload, writeUninstallProgress } from '@/lib/capture-lifecycle'
import type { UninstallProgress } from '@/lib/capture-lifecycle'
import type { CaptureStatus } from '@/api/capture'
import { setProcessingTarget } from '@/lib/processing-target'
import { copyText } from '@/lib/utils'
import { useI18n } from '@/lib/i18n'

const errors: Record<string, [string, string]> = {
  local_component_required: ['请先运行脚本，在本机页面继续配置。', 'Install the local component and continue on its local page.'],
  capture_runtime_missing: ['尚未安装捕获运行时，请重新运行安装脚本。', 'Capture runtime is missing. Run the installation script.'],
  capture_runtime_version_mismatch: ['捕获组件版本不匹配，请更新组件。', 'Update the capture runtime to the required version.'],
  linux_kernel_6_8_required: ['捕获需要 Linux 内核 6.8 或更新版本。', 'Capture requires Linux kernel 6.8 or later.'],
  wsl_capture_unsupported: ['首版暂不支持 WSL 捕获。', 'WSL capture is not supported.'],
  container_capture_unsupported: ['首版暂不支持容器内捕获。', 'Capture inside containers is not supported.'],
  macos_14_required: ['捕获需要 macOS 14 或更新版本。', 'Capture requires macOS 14 or later.'],
  capture_linux_authorization_ui_required: ['Linux 授权需要 sudo 和 zenity 或 kdialog，请安装后重试，并确保当前桌面会话可显示授权窗口。', 'Install sudo and zenity or kdialog, then retry from an active Linux desktop session.'],
  capture_linux_sudo_required: ['Ubuntu 缺少 sudo，请安装 sudo 后重新检查。', 'Install sudo on Ubuntu, then check again.'],
  capture_linux_lsof_required: ['Ubuntu 缺少 lsof，无法安全识别代理进程。请执行 sudo apt install lsof 后重新检查。', 'Install lsof with sudo apt install lsof so proxy processes can be identified safely, then check again.'],
  capture_linux_authorization_display_required: ['后台未连接 Linux 桌面会话，无法显示授权窗口。请在桌面终端重跑安装脚本，导入会话环境后再检查。', 'The service has no Linux desktop session for authorization. Rerun the installer from a desktop terminal to import the session environment, then check again.'],
  capture_linux_backend_unavailable: ['Linux 捕获后端加载失败，请核对内核 6.8+、官方组件版本和系统授权，再重新检查。', 'The Linux capture backend could not load. Verify kernel 6.8+, the pinned runtime and authorization, then check again.'],
  capture_authorization_denied: ['系统捕获授权未完成或被拒绝，请重新授权并完成系统确认。', 'Capture authorization was canceled or denied. Retry and complete the system prompt.'],
  proxy_config_requires_confirmation: ['代理配置无法可靠识别，请在高级网络设置中确认出站代理。', 'Confirm the unrecognized proxy in advanced network settings.'],
  proxy_auto_config_requires_confirmation: ['检测到自动代理配置，请确认实际出站代理后再保存。', 'Automatic proxy configuration was detected. Confirm the actual egress proxy before saving.'],
  certificate_trust_required: ['尚未信任本机证书。应用使用独立证书库时仍需单独配置。', 'Trust the local CA. Apps with independent trust stores may require additional setup.'],
  capture_tls_trust_failed: ['本机 TLS 信任测试失败，请重新检查证书信任设置。', 'The local TLS trust test failed. Check certificate trust.'],
  manual_certificate_trust_required: ['后台无法设置证书信任，请在系统证书管理中手动信任本机 CA，再点击检查。', 'Trust the local CA in your system certificate manager, then check again. Background trust setup is unavailable.'],
  certificate_trust_denied: ['证书信任未获批准，请完成系统确认或在系统证书管理中手动设置后重试。', 'Certificate trust was not approved. Complete the system confirmation or configure trust manually, then retry.'],
  capture_loopback_unsupported: ['macOS 捕获后端不支持回环目标。使用 cc-switch 等本机网关时，请保护它实际访问的模型域名。', 'The macOS capture backend excludes loopback. For local gateways such as cc-switch, protect their actual upstream model domains.'],
  capture_authorization_required: ['尚未验证系统捕获权限，请点击授权并完成系统提示。', 'Authorize capture and complete the system prompts.'],
  capture_authorization_or_start_failed: ['捕获组件启动或授权失败，请检查系统权限后重试。', 'Capture could not start. Check system permissions and retry.'],
  capture_ready_timeout: ['等待系统授权超时，请完成系统提示后重试。', 'Authorization timed out. Complete the system prompt and retry.'],
  capture_activation_timeout: ['捕获规则未在规定时间内生效，请检查组件后重试。', 'Capture rules were not applied in time. Check the component and retry.'],
  capture_process_exited: ['捕获后台已经退出，正在等待恢复或手动修复。', 'The capture backend exited. Recovery or repair is required.'],
  pipeline_check_required: ['请执行 TLS、脱敏与还原检查。', 'Run the TLS, masking and restoration check.'],
  capture_heartbeat_stale: ['捕获组件心跳已过期，请重新检查或通过向导修复组件。', 'The capture heartbeat is stale. Check again or repair the component through setup.'],
  capture_targets_not_applied: ['捕获进程在线，但尚未确认目标规则生效，请重新检查或修复组件。', 'The capture process is online, but target rules are not confirmed. Check again or repair the component.'],
  capture_targets_required: ['请至少配置一个保护目标。', 'Configure at least one protected target.'],
  capture_settings_open_failed: ['未能打开系统设置，请按页面路径手动打开后继续检查。', 'Could not open system settings. Follow the steps on this page, then check again.'],
  capture_certificate_missing: ['请先生成本机证书，再下载或安装。', 'Generate the local CA before downloading or installing it.'],
  managed_install_required: ['此实例未通过安装脚本登记，请先运行安装脚本。', 'Run the installer before using managed maintenance.'],
  stop_capture_before_uninstall: ['请先停止捕获，并检查停止状态后再卸载。', 'Stop capture and verify it has stopped before uninstalling.'],
  capture_stop_failed: ['捕获进程尚未确认退出，请重试停止；检查通过前不能继续卸载。', 'The capture process has not been confirmed stopped. Retry; uninstall remains blocked.'],
  maintenance_in_progress: ['维护任务仍在进行，请等待完成后重试。', 'A maintenance job is in progress. Wait for it to finish and retry.'],
  maintenance_interrupted: ['上次维护任务已中断，可重新执行修复、更新或卸载引导。', 'The previous maintenance task was interrupted. Retry repair, update or the uninstall guide.'],
  maintenance_status_unavailable: ['暂时无法核验维护进程，请重新检查；确认前不会启动第二个任务。', 'The maintenance process could not be verified. Check again before starting another task.'],
  capture_step_check_failed: ['本步骤的实际检查尚未通过，请处理提示项后重试。', 'This step has not passed its actual checks. Resolve the reported issue and retry.'],
  capture_start_unconfirmed: ['尚未确认捕获启动成功，请重新检查本机状态。', 'Capture startup is not confirmed. Check the local status again.'],
  invalid_uninstall_options: ['卸载选项无效，请返回引导重新选择。', 'Invalid uninstall options. Return to the guide and choose again.'],
  remote_auth_failed: ['远程令牌无效，本机登录状态未受影响。', 'Remote authentication failed. Your local session is unchanged.'],
  remote_protocol_unsupported: ['远程服务器未启用捕获转交或协议不兼容。', 'The remote server does not support capture forwarding.'],
  remote_connection_failed: ['无法通过 HTTPS 连接远程服务器，请检查地址、证书和代理。', 'Check the remote HTTPS address, certificate and proxy.'],
  stop_capture_before_editing_targets: ['请先停止捕获，再修改目标或出站代理。', 'Stop capture before editing targets or egress settings.'],
}

export default function CapturePage() {
  const { lang } = useI18n()
  const en = lang === 'en'
  const text = (zh: string, english: string) => en ? english : zh
  const cache = useQueryClient()
  const [step, setStep] = useState(() => { try { const n = Number(sessionStorage.getItem('silencer_capture_step')); return Number.isInteger(n) && n >= 0 && n < 6 ? n : 0 } catch { return 0 } })
  useEffect(() => { try { sessionStorage.setItem('silencer_capture_step', String(step)) } catch { /* storage unavailable */ } }, [step])
  const [modeHint] = useState(() => new URLSearchParams(window.location.hash.split("?")[1] ?? "").get("mode"))
  const [resumeHint] = useState(() => new URLSearchParams(window.location.hash.split('?')[1] ?? '').get('resume') === '1')
  useEffect(() => { if (modeHint || resumeHint) { const url = new URL(window.location.href); const [route, raw] = url.hash.split('?'); const params = new URLSearchParams(raw); params.delete('mode'); params.delete('resume'); url.hash = route + (params.size ? '?' + params : ''); window.history.replaceState(null, '', url) } }, [modeHint, resumeHint])
  const [mode, setMode] = useState<'local' | 'remote'>(modeHint === "remote" ? "remote" : "local")
  const [remoteURL, setRemoteURL] = useState('')
  const [remoteToken, setRemoteToken] = useState('')
  const [system, setSystem] = useState(() => /Windows/i.test(navigator.userAgent) ? 'windows' : /Macintosh|Mac OS/i.test(navigator.userAgent) ? 'darwin' : 'linux')
  const [targets, setTargets] = useState('')
  const [remoteProxy, setRemoteProxy] = useState('')
  const [modelProxy, setModelProxy] = useState('')
  const [networkDirty, setNetworkDirty] = useState(false)
  const [releaseBase, setReleaseBase] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [manualTrust, setManualTrust] = useState(false)
  const [setupRequested, setSetupRequested] = useState(() => { try { return sessionStorage.getItem('silencer_capture_setup_open') === '1' } catch { return false } })
  useEffect(() => { try { sessionStorage.setItem('silencer_capture_setup_open', setupRequested ? '1' : '0') } catch { /* unavailable */ } }, [setupRequested])
  const [uninstallProgress, setUninstallProgress] = useState<UninstallProgress | null>(() => { try { return readUninstallProgress(localStorage) } catch { return null } })
  const [resumeReason, setResumeReason] = useState('')
  useEffect(() => { try { writeUninstallProgress(localStorage, uninstallProgress) } catch { /* unavailable */ } }, [uninstallProgress])
  const loopbackPage = ['127.0.0.1', 'localhost', '[::1]', '::1'].includes(window.location.hostname)
  const cap = useQuery({ queryKey: ['captureCapabilities'], queryFn: getCapabilities, staleTime: 0, refetchOnMount: 'always', refetchOnWindowFocus: 'always', retry: false })
  const local = cap.data?.local === true
  const uninstallOpen = uninstallProgress?.active === true && (local || loopbackPage)
  const status = useQuery({ queryKey: ['localCaptureStatus'], queryFn: getCaptureStatus, enabled: local || (uninstallOpen && loopbackPage), refetchInterval: 10000, refetchOnMount: 'always', refetchOnWindowFocus: 'always', retry: false })
  const current = captureStatusUsable(status.data, status.isFetchedAfterMount, !!status.error)
  const configured = status.data?.config.setup_complete === true && !setupRequested
  useEffect(() => {
    if (!current || !status.data || !uninstallProgress) return
    const result = reconcileUninstallProgress(uninstallProgress, status.data)
    if (JSON.stringify(result.progress) !== JSON.stringify(uninstallProgress)) setUninstallProgress(result.progress)
    if (result.reason) setResumeReason(result.reason)
  }, [status.data, current, uninstallProgress])
  const updateUninstallProgress = (patch: Partial<UninstallProgress>) => setUninstallProgress((previous) => ({ ...(previous ?? newUninstallProgress(status.data)), ...patch, version: 1, updatedAt: Date.now() }))
  const recordDownload = (requested: UninstallProgress) => setUninstallProgress((previous) => {
    if (!previous) return previous
    const query = cache.getQueryState<CaptureStatus>(['localCaptureStatus'])
    const latest = query?.data
    return recordUninstallDownload(latest ? reconcileUninstallProgress(previous, latest).progress : previous, requested, query?.status === 'success')
  })
  const [seeded, setSeeded] = useState(false)
  useEffect(() => {
    if (!seeded && current && status.data) {
      const cfg = status.data.config
      if (modeHint !== "remote") setMode(cfg.mode); setTargets(cfg.targets.join('\n')); setRemoteProxy(cfg.remote_proxy); setModelProxy(cfg.model_proxy)
      setRemoteURL(cfg.profiles.find((p) => p.id === cfg.active_id)?.url ?? '')
      if (resumeHint && !cfg.setup_complete) {
        const c = status.data.checks
        setStep(!c.component.ok || !c.platform.ok ? 1 : !c.permissions.ok ? 2 : !c.certificate.ok ? 3 : 4)
      }
      setSeeded(true)
    }
  }, [status.data, current, seeded, modeHint, resumeHint])
  useEffect(() => { if (cap.data?.release_base) setReleaseBase(cap.data.release_base) }, [cap.data?.release_base])
  const explain = (code: string) => errors[code]?.[en ? 1 : 0] ?? code
  const execute = async (fn: () => Promise<unknown>, advance?: keyof CaptureStatus['checks'] | 'next') => {
    setBusy(true); setError('')
    try {
      const response = await fn()
      let actual: CaptureStatus | undefined
      if (isCaptureStatus(response)) {
        actual = response
        cache.setQueryData(['localCaptureStatus'], response)
      } else {
        const result = await status.refetch()
        if (result.error) throw result.error
        actual = result.data
      }
      if (advance === 'next') {
        if (actual && captureSetupStepLimit(actual, processorMatches) > step) setStep((value) => Math.min(value + 1, 5))
        else setError(explain('capture_step_check_failed'))
      } else if (advance && actual?.checks[advance].ok) setStep((value) => Math.min(value + 1, 5))
    } catch (e) {
      const code = (e as { code?: string }).code
      if (code === 'manual_certificate_trust_required') setManualTrust(true)
      setError(code ? explain(code) : String(e))
      await status.refetch()
    } finally { setBusy(false) }
  }
  const checks = current ? status.data?.checks : undefined
  const maintenanceBusy = status.data?.job?.state === 'queued' || status.data?.job?.state === 'running'
  const steps = [text('处理位置', 'Processing'), text('安装组件', 'Install'), text('系统权限', 'Permissions'), text('证书信任', 'Certificate'), text('链路检查', 'Verify'), text('完成配置', 'Finish')]
  const processorConfig = status.data?.config
  const processorMatches = !!processorConfig && processorConfig.mode === mode && (mode === 'local' || (processorConfig.profiles.find((p) => p.id === processorConfig.active_id)?.url === remoteURL.replace(/\/$/, '') && !remoteToken))
  const finishReady = !!(current && local && !networkDirty && processorMatches && checks?.pipeline.ok && checks?.component.ok && checks?.platform.ok && checks?.permissions.ok && checks?.certificate.ok)
  useEffect(() => {
    if (configured || !current || !status.data) return
    const maxStep = captureSetupStepLimit(status.data, processorMatches)
    setStep((current) => Math.min(current, maxStep))
  }, [configured, current, status.data, processorMatches])
  const canNext = step === 0 || (current && ((step === 1 && local && checks?.component.ok && checks?.platform.ok) || (step === 2 && checks?.permissions.ok) || (step === 3 && checks?.certificate.ok) || (step === 4 && !networkDirty && processorMatches && checks?.pipeline.ok && local && checks?.component.ok && checks?.permissions.ok && checks?.certificate.ok)))
  const activeProfile = status.data?.config.profiles.find((p) => p.id === status.data?.config.active_id)
  const scriptName = system === 'windows' ? 'install.ps1' : 'install.sh'
  const command = system === 'windows' ? `$env:SILENCER_SETUP_MODE='${mode}'; $env:SILENCER_RELEASE_BASE='${releaseBase.replaceAll("'", "''")}'; powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\\install.ps1` : `SILENCER_SETUP_MODE='${mode}' SILENCER_RELEASE_BASE='${releaseBase.replaceAll("'", "'\\''")}' bash ./install.sh`
  const check = (key: keyof CaptureStatus['checks'], label: string) => <div className="flex items-start gap-2 rounded-md border p-3" key={key}>
    {checks?.[key].ok ? <CheckCircle2 className="mt-0.5 h-4 w-4 text-emerald-600" /> : <Circle className="mt-0.5 h-4 w-4 text-muted-foreground" />}
    <div><p className="text-sm font-medium">{label}</p>{!current ? <p className="text-xs text-muted-foreground">{text('等待重新检查本机状态', 'Awaiting a fresh local check')}</p> : checks?.[key].error && <p className="text-xs text-muted-foreground">{explain(checks[key].error)}</p>}</div>
  </div>
  const chooseProcessor = () => execute(async () => {
    const cfg = await saveProcessing(mode, remoteURL, remoteToken)
    setRemoteToken(''); setProcessingTarget(true, cfg.active_id)
  })
  const downloadCertificate = () => execute(async () => {
    const blob = await downloadCaptureCertificate()
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url; link.download = 'silencer-local-ca.crt'
    document.body.appendChild(link); link.click(); link.remove()
    URL.revokeObjectURL(url)
  })
  const saveNetwork = () => execute(async () => {
    const cfg = await saveCaptureConfig({ targets: targets.split(/\r?\n/).map((x) => x.trim()).filter(Boolean), remote_proxy: remoteProxy, model_proxy: modelProxy })
    setTargets(cfg.targets.join('\n')); setRemoteProxy(cfg.remote_proxy); setModelProxy(cfg.model_proxy); setNetworkDirty(false)
  })
  return <div className="mx-auto max-w-5xl space-y-5 p-4 md:p-6">
    <div className="flex flex-wrap items-center justify-between gap-3"><div><h1 className="text-2xl font-semibold">{text('捕获模式', 'Capture mode')}</h1><p className="mt-1 text-sm text-muted-foreground">{text('保持客户端原地址和 Key，通过本机组件保护 AI API 请求。', 'Protect AI API requests while keeping your clients’ original URLs and keys.')}</p></div>
      {configured && !uninstallOpen && <Button variant="outline" disabled={busy || maintenanceBusy || !current} onClick={() => { setSetupRequested(true); setStep(0) }}>{text('安装与修复向导', 'Setup & repair')}</Button>}</div>
    <div className="rounded-lg border bg-muted/30 p-3 text-sm"><ShieldCheck className="mr-2 inline h-4 w-4" />{text('当前处理端：', 'Processing on: ')}<strong>{local ? status.data?.config.mode === 'remote' ? activeProfile?.url : text('本机', 'This computer') : text('当前服务器（本机尚未连接）', 'Current server — local component not connected')}</strong></div>
    {status.data?.job && <p role="status" className="rounded border p-3 text-sm">{text('维护任务：', 'Maintenance job: ')}{({ repair: text('修复', 'Repair'), update: text('更新', 'Update'), uninstall: text('卸载', 'Uninstall') } as Record<string, string>)[status.data.job.action] ?? status.data.job.action} · {({ queued: text('等待启动', 'Queued'), running: text('进行中', 'Running'), complete: text('已完成', 'Complete'), failed: text('失败，可重试', 'Failed; retry available'), interrupted: text('已中断，可重试', 'Interrupted; retry available') } as Record<string, string>)[status.data.job.state] ?? status.data.job.state}{status.data.job.error ? ' · ' + explain(status.data.job.error) : ''}</p>}
    {(error || (!uninstallOpen && (cap.error || status.error))) && <div role="alert" className="rounded-lg border border-red-300 p-3 text-sm text-red-600"><TriangleAlert className="mr-2 inline h-4 w-4" />{error || text('本机连接或能力检查失败，请重新检查；旧的检查结果不会用于开启保护。', 'The local connection or capability check failed. Check again; cached results cannot enable protection.')}</div>}
    {!uninstallOpen && status.data?.last_error && <p role="alert" className="rounded border border-amber-400 p-3 text-sm">{explain(status.data.last_error)}</p>}
    {!uninstallOpen && local && cap.data?.platform === 'darwin' && <p className="rounded border p-3 text-sm text-muted-foreground">{text('macOS 不捕获 localhost、127.0.0.1 或 ::1 回环流量。使用 cc-switch 等本机网关时，请将它实际访问的模型服务域名加入保护目标，并确认该网关信任本机 CA。', 'macOS capture excludes localhost, 127.0.0.1 and ::1. For local gateways such as cc-switch, protect their upstream model domains and ensure the gateway trusts the local CA.')}</p>}
    {!local && loopbackPage && uninstallProgress && !uninstallOpen && <Button variant="outline" onClick={() => updateUninstallProgress({ active: true })}>{text('继续卸载引导', 'Resume uninstall')}</Button>}
    {local && !uninstallOpen && <CaptureReadiness status={status.data} current={current} checking={status.isFetching} text={text} explain={explain} />}
    {resumeReason === 'installation_changed' && !uninstallOpen && <p role="status" className="rounded border p-3 text-sm">{text('检测到组件更新或重新安装，之前的卸载进度已重置。', 'An update or reinstall was detected. Previous uninstall progress was reset.')}</p>}
    {uninstallOpen && uninstallProgress && <UninstallGuide status={status.data} disconnected={!!status.error} current={current} checking={status.isFetching} progress={uninstallProgress} onProgress={updateUninstallProgress} onDownloaded={recordDownload} resumeReason={resumeReason === 'certificate_changed' ? text('证书已发生变化，旧脚本下载记录已清除；请重新确认清理范围并下载。', 'The CA changed. The old download receipt was cleared; confirm the scope and download again.') : undefined} text={text} explain={explain} onClose={() => updateUninstallProgress({ active: false })} />}
    {!uninstallOpen && !configured && <>
      <div className="grid grid-cols-3 gap-2 md:grid-cols-6">{steps.map((label, index) => <button key={label} disabled={index > step || busy} onClick={() => setStep(index)} className={`rounded-md border p-3 text-left text-sm ${index === step ? 'border-primary bg-primary/5' : ''}`}><span className="mr-2 text-muted-foreground">{index + 1}</span>{label}</button>)}</div>
      <Card><CardHeader><CardTitle>{steps[step]}</CardTitle></CardHeader><CardContent className="space-y-4">
        {step === 0 && <><p>{text('默认在本机脱敏和保存日志，也可交给你自己的远程 Silencer 服务器。', 'Process and store logs locally by default, or use your own remote Silencer server.')}</p><div className="flex gap-3"><Button variant={mode === 'local' ? 'default' : 'outline'} onClick={() => setMode('local')}>{text('本地模式（默认）', 'Local (default)')}</Button><Button variant={mode === 'remote' ? 'default' : 'outline'} onClick={() => setMode('remote')}>{text('远程模式', 'Remote')}</Button></div>{mode === 'remote' && <p className="text-sm text-muted-foreground">{text('请求原文会经 HTTPS 交给指定服务器处理。安装本机组件后配置地址与令牌。', 'Original requests will be sent over HTTPS to your chosen server. Configure it after installing the local component.')}</p>}</>}
        {step === 1 && <>{local && checks?.component.ok && checks?.platform.ok ? <p className="flex items-center gap-2 rounded border border-emerald-500/30 p-3 text-sm"><CheckCircle2 className="h-4 w-4 text-emerald-600" />{text('本机组件已安装并通过检测，可以直接进入下一步。', 'The local component is installed and verified. Continue to the next step.')}</p> : <><label className="block text-sm">{text('这台电脑的系统（可手动修正）', 'This computer’s OS (you can correct it)')}<select className="ml-3 rounded border bg-background p-2" value={system} onChange={(e) => setSystem(e.target.value)}><option value="windows">Windows</option><option value="darwin">macOS</option><option value="linux">Linux</option></select></label>
          <p className="text-sm text-muted-foreground">{text('下载脚本并在本机终端执行。脚本完成后自动打开本机的同一向导继续。', 'Download and run the script in your local terminal. It opens this wizard on your local service.')}</p>
          {!releaseBase && <p role="status" className="text-sm text-amber-700">{text('安装资源尚未发布，请联系此实例的管理员。', 'Installation resources are not published yet. Contact this instance’s administrator.')}</p>}
          <Button variant="outline" asChild><a href={`/capture-assets/${scriptName}`} download={scriptName}><Download className="mr-2 h-4 w-4" />{text('下载安装脚本', 'Download script')}</a></Button>
          <div className="rounded border bg-muted/30 p-3"><code className="break-all text-xs">{command}</code><Button variant="ghost" size="sm" disabled={!releaseBase.startsWith('https://')} onClick={() => void copyText(command)}><Copy className="h-4 w-4" /></Button></div>
          {!local && <Button variant="outline" asChild><a href={`http://127.0.0.1:18711/#/capture?mode=${mode}&resume=1`} target="_blank" rel="noreferrer">{text('打开本机向导', 'Open local wizard')}</a></Button>}
          </>}
          {local && <>{check('platform', text('系统与内核', 'System & kernel'))}{check('component', text('组件与版本', 'Runtime version'))}</>}
        </>}
        {step === 2 && <><PermissionGuide system={status.data?.platform ?? system} text={text} busy={busy || maintenanceBusy || !current} ready={checks?.permissions.ok === true} onAction={(action) => void execute(() => captureAction(action))} />{check('permissions', text('真实捕获组件已就绪', 'Capture component is ready'))}<Button disabled={busy || maintenanceBusy || !current} onClick={() => void execute(() => captureAction(checks?.permissions.ok ? 'check' : 'authorize'), 'permissions')}>{checks?.permissions.ok ? text('检查权限并继续', 'Check permissions & continue') : text('授权并检查', 'Authorize & check')}</Button></>}
        {step === 3 && <><CertificateGuide system={status.data?.platform ?? system} text={text} busy={busy || maintenanceBusy || !current} onAction={(action) => void execute(() => captureAction(action))} certificate={status.data?.certificate} trusted={checks?.certificate.ok === true} manual={manualTrust} onDownload={() => void downloadCertificate()} />{check('certificate', text('本机证书信任', 'Local CA trust'))}<Button disabled={busy || maintenanceBusy || !current} onClick={() => void execute(() => captureAction('check'), 'certificate')}>{text('检查证书并继续', 'Check certificate & continue')}</Button></>}
        {step === 4 && <>{renderProcessing()}<div className="rounded-lg border p-4">{renderNetworkConfig()}</div><Button variant="outline" disabled={busy || maintenanceBusy || !current || !processorMatches} onClick={() => void execute(() => captureAction('diagnose'))}>{text('验证 TLS、脱敏与还原', 'Verify TLS, masking & restoration')}</Button>{check('pipeline', text('TLS 与处理端自检', 'TLS & processor self-test'))}{check('component', text('捕获组件', 'Capture runtime'))}{check('permissions', text('系统权限', 'Permissions'))}{check('certificate', text('系统证书信任', 'System CA trust'))}<p className="text-sm text-muted-foreground">{text('这些检查确认组件条件；开启后还需从原客户端发送一次请求，才能验证该客户端实际受到保护。', 'These checks verify prerequisites. Send a request from your original client after enabling capture to verify actual protection.')}</p></>}
        {step === 5 && <><p>{text('准备完成。开启后，从原客户端发起一次请求，在请求记录中验证。已有长连接可能需要重连。', 'Ready. Enable capture and send a request from your client, then inspect its ledger entry. Existing connections may need reconnecting.')}</p><Button disabled={busy || maintenanceBusy || !finishReady} onClick={() => void execute(async () => { const actual = await captureAction('start'); cache.setQueryData(['localCaptureStatus'], actual); if (!captureIsActive(actual)) throw Object.assign(new Error('capture_start_unconfirmed'), { code: 'capture_start_unconfirmed' }); setSetupRequested(false); return actual })}>{text('开启捕获模式', 'Enable capture')}</Button></>}
        <div className="flex justify-between border-t pt-4"><Button variant="outline" disabled={busy || !local} onClick={() => void execute(getCaptureStatus)}><RefreshCw className="mr-2 h-4 w-4" />{text('检查 / 重试', 'Check / retry')}</Button>{step < 5 && <Button disabled={!canNext || busy} onClick={() => { if (step === 0) setStep(1); else void execute(getCaptureStatus, 'next') }}>{text('下一步', 'Next')}</Button>}</div>
      </CardContent></Card>
    </>}
    {!uninstallOpen && configured && <><Card><CardHeader><CardTitle>{text('本机保护', 'Local protection')}</CardTitle></CardHeader><CardContent className="space-y-3">
      <p>{text('服务状态：', 'Service: ')}{!current ? text('等待重新检查', 'Awaiting a fresh check') : status.data?.state === 'running' ? text('捕获运行中', 'Capture running') : status.data?.config.enabled ? text('需要检查，具体原因见上方提示', 'Check the reasons shown above') : text('捕获未开启', 'Capture is off')}</p>
      <p>{text('经过捕获入口的请求：', 'Requests through capture: ')}{status.data?.observed_requests ?? 0}</p>
      <p className="text-sm text-muted-foreground">{text('服务运行不代表每个应用均已保护。请结合处理端请求账本确认。', 'A running service does not prove every app is protected. Confirm requests in the processor ledger.')}</p>
      <div className="flex flex-wrap gap-3"><Button disabled={busy || !current || (maintenanceBusy && !status.data?.config.enabled)} onClick={() => void execute(() => captureAction(status.data?.config.enabled ? 'stop' : 'start'))}>{status.data?.config.enabled ? text('停止捕获', 'Stop capture') : text('开启捕获', 'Start capture')}</Button><Button variant="outline" disabled={busy || maintenanceBusy || !current} onClick={() => void execute(() => captureAction('diagnose'))}><RefreshCw className="mr-2 h-4 w-4" />{text('重新检查保护链路', 'Recheck protection pipeline')}</Button></div>
    </CardContent></Card>
      <Card><CardHeader><CardTitle>{text('处理位置', 'Processing location')}</CardTitle></CardHeader><CardContent>{renderProcessing()}</CardContent></Card>
      <Card><CardHeader><CardTitle>{text('捕获目标与网络', 'Targets & network')}</CardTitle></CardHeader><CardContent>{renderNetworkConfig()}</CardContent></Card></>}
    {local && !uninstallOpen && <Card><CardHeader><CardTitle>{text('维护', 'Maintenance')}</CardTitle></CardHeader><CardContent className="flex flex-wrap gap-3">
      <Button variant="outline" disabled={busy || maintenanceBusy || !current} onClick={() => void execute(async () => { const result = await captureAction('detect_proxy'); setRemoteProxy(result.config.remote_proxy); setModelProxy(result.config.model_proxy) })}>{text('检测现有代理', 'Detect existing proxy')}</Button>
      <Button variant="outline" disabled={busy || maintenanceBusy || !current} onClick={() => void execute(() => captureAction('repair'))}>{text('修复组件', 'Repair components')}</Button>
      <Button variant="outline" disabled={busy || maintenanceBusy || !current} onClick={() => void execute(() => captureAction('update'))}>{text('更新组件', 'Update components')}</Button>
      <Button variant="outline" disabled={busy || maintenanceBusy} onClick={() => { setError(''); setResumeReason(''); setUninstallProgress((previous) => ({ ...(previous ? current && status.data ? reconcileUninstallProgress(previous, status.data).progress : previous : newUninstallProgress(status.data)), active: true, updatedAt: Date.now() })) }}>{uninstallProgress && uninstallProgress.step > 0 ? text('继续卸载引导', 'Resume uninstall') : text('卸载引导', 'Uninstall guide')}</Button>
    </CardContent></Card>}
  </div>

  function renderNetworkConfig() {
    return <div className="space-y-3">
      <p className="text-sm">{text('常见 API 域名已内置。使用 cc-switch、自建中转或故障转移时，请将实际使用的所有模型域名加入下方列表。', 'Common API domains are included. For cc-switch, custom gateways or failover, add every upstream model domain you actually use.')}</p>
      <label className="block text-sm">{text('保护目标，一行一个域名与端口', 'Protected targets, one host:port per line')}<textarea className="mt-2 min-h-40 w-full rounded border bg-background p-3 font-mono" value={targets} onChange={(e) => { setTargets(e.target.value); setNetworkDirty(true) }} /></label>
      <details><summary className="cursor-pointer text-sm text-muted-foreground">{text('高级网络设置', 'Advanced network settings')}</summary><div className="mt-3 space-y-3"><Input placeholder={text('本机到远程服务器的代理，留空直连', 'Proxy to remote processor; empty for direct')} value={remoteProxy} onChange={(e) => { setRemoteProxy(e.target.value); setNetworkDirty(true) }} /><Input placeholder={text('本地处理时到模型的代理，留空直连', 'Local processor proxy to model; empty for direct')} value={modelProxy} onChange={(e) => { setModelProxy(e.target.value); setNetworkDirty(true) }} /><p className="text-xs text-muted-foreground">{text('代理支持 HTTP / HTTPS / SOCKS5。TUN 保持原有路由；本机 API 网关地址不是出站代理。修改前先停止捕获。', 'HTTP / HTTPS / SOCKS5 egress is supported. TUN keeps existing routing. A local API gateway is not an egress proxy. Stop capture before editing.')}</p></div></details>
      <Button variant="outline" disabled={busy || maintenanceBusy || !current || status.data?.config.enabled} onClick={() => void saveNetwork()}>{text('保存保护目标', 'Save protected targets')}</Button>
      {networkDirty && <p role="status" className="text-sm text-amber-700">{text('有未保存的捕获设置，请保存后继续。', 'Save your capture changes before continuing.')}</p>}
    </div>
  }

  function renderProcessing() {
    return <div className="space-y-3"><select aria-label="Processing mode" className="rounded border bg-background p-2" value={mode} onChange={(e) => setMode(e.target.value as 'local' | 'remote')}><option value="local">{text('本地模式（默认）', 'Local (default)')}</option><option value="remote">{text('远程模式', 'Remote')}</option></select>{mode === 'remote' && <><Input type="url" placeholder="https://silencer.example.com" value={remoteURL} onChange={(e) => setRemoteURL(e.target.value)} /><Input type="password" autoComplete="off" placeholder={text('远程网关管理令牌（保存后不回显）', 'Remote management token (never displayed after saving)')} value={remoteToken} onChange={(e) => setRemoteToken(e.target.value)} /><p className="text-sm text-muted-foreground">{text('原文与供应商凭据交给该服务器处理，日志和映射留在远程。服务器需显式启用捕获转交并允许目标域名。', 'Original requests and provider credentials are processed remotely. Logs and mappings stay there. The server must enable capture forwarding and allow the target domains.')}</p></>}<Button disabled={busy || maintenanceBusy || !current || (mode === 'remote' && !remoteToken)} onClick={chooseProcessor}>{text('验证并保存处理位置', 'Verify & save processor')}</Button></div>
  }
}
