import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, Circle, Copy, Download, RefreshCw, TriangleAlert } from 'lucide-react'
import { captureAction, downloadCaptureUninstaller, getCaptureStatus } from '@/api/capture'
import type { CaptureStatus } from '@/api/capture'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { copyText } from '@/lib/utils'
import { reconcileUninstallProgress, uninstallDownloadMatches } from '@/lib/capture-lifecycle'
import type { UninstallProgress } from '@/lib/capture-lifecycle'

type Props = {
  status?: CaptureStatus
  disconnected: boolean
  current: boolean
  checking: boolean
  progress: UninstallProgress
  onProgress: (patch: Partial<UninstallProgress>) => void
  onDownloaded: (requested: UninstallProgress) => void
  resumeReason?: string
  text: (zh: string, en: string) => string
  explain: (code: string) => string
  onClose: () => void
}

export function UninstallGuide({ status, disconnected, current, checking, progress, onProgress, onDownloaded, resumeReason, text, explain, onClose }: Props) {
  const cache = useQueryClient()
  const { step, keepCertificate } = progress
  const setStep = (next: number) => onProgress({ step: next })
  const [busy, setBusy] = useState(false)
  const downloaded = progress.downloadedAt > 0
  const setKeepCertificate = (keep: boolean) => onProgress({ keepCertificate: keep, downloadedAt: 0 })
  const [error, setError] = useState('')
  const [connectionChecked, setConnectionChecked] = useState(false)
  const stopped = current && status?.uninstall?.capture_stopped === true
  const managed = status?.uninstall?.managed_install === true
  const maintenanceBusy = status?.job?.state === 'queued' || status?.job?.state === 'running'
  const platform = status?.platform ?? progress.platform
  const certificate = status?.certificate ?? (progress.certificateSha ? { subject: 'Silencer Local Capture', sha256: progress.certificateSha } : null)
  const filename = platform === 'windows' ? 'silencer-uninstall.ps1' : 'silencer-uninstall.sh'
  const command = platform === 'windows'
    ? 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\\silencer-uninstall.ps1'
    : 'bash ./silencer-uninstall.sh'
  const steps = [text('停止捕获', 'Stop capture'), text('清理范围', 'Cleanup scope'), text('卸载与检查', 'Uninstall & verify')]

  async function checkStop(stop: boolean) {
    setBusy(true); setError('')
    try {
      const result = await captureAction(stop ? 'stop' : 'check')
      cache.setQueryData(['localCaptureStatus'], result)
      if (result.uninstall?.capture_stopped !== true) {
        setError(text('捕获仍在运行，请先停止再检查。', 'Capture is still running. Stop it and check again.'))
        return
      }
      onProgress({ step: 1, platform: result.platform as UninstallProgress['platform'], certificateSha: result.certificate?.sha256 ?? '', installRevision: result.uninstall?.install_revision ?? '' })
    } catch (e) {
      showError(e)
      await cache.invalidateQueries({ queryKey: ['localCaptureStatus'] })
    } finally { setBusy(false) }
  }

  function showError(e: unknown) {
    const code = (e as { code?: string }).code
    setError(code ? explain(code) : String(e))
  }

  async function download() {
    setBusy(true); setError('')
    try {
      const blob = await downloadCaptureUninstaller(keepCertificate)
      const query = cache.getQueryState<CaptureStatus>(['localCaptureStatus'])
      const latest = query?.data
      if (query?.status !== 'success' || !latest || !uninstallDownloadMatches(reconcileUninstallProgress(progress, latest).progress, progress)) {
        setError(text('安装状态已变化或连接中断，请重新检查并下载当前脚本。', 'The installation changed or disconnected. Check again and download the current script.'))
        return
      }
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url; link.download = filename
      document.body.appendChild(link); link.click(); link.remove()
      URL.revokeObjectURL(url)
      onDownloaded(progress)
    } catch (e) { showError(e) } finally { setBusy(false) }
  }

  async function checkConnection() {
    setBusy(true); setError(''); setConnectionChecked(false)
    try {
      await cache.fetchQuery({ queryKey: ['localCaptureStatus'], queryFn: getCaptureStatus, staleTime: 0 })
      setConnectionChecked(true)
    } catch {
      setError(text('本机服务当前无法连接。请查看脚本打开的卸载报告；连接中断不能证明卸载成功。', 'The local service is unavailable. Read the uninstall report opened by the script; a lost connection does not prove success.'))
    } finally { setBusy(false) }
  }

  return <section aria-label={text('捕获模式卸载向导', 'Capture uninstall wizard')} className="space-y-4">
    <div className="flex flex-wrap items-center justify-between gap-3"><div><h2 className="text-xl font-semibold">{text('卸载捕获组件', 'Uninstall capture components')}</h2><p className="mt-1 text-sm text-muted-foreground">{text('按步骤停止保护、确认清理范围，再执行卸载和真实检查。', 'Stop protection, choose what to clean up, then uninstall and verify.')}</p></div><Button variant="outline" disabled={busy} onClick={onClose}>{text('返回捕获设置', 'Back to capture settings')}</Button></div>
    <div className="grid grid-cols-3 gap-2">{steps.map((label, index) => <button key={label} disabled={index > step || busy} onClick={() => { setStep(index); setError('') }} className={`rounded-md border p-3 text-left text-sm ${index === step ? 'border-primary bg-primary/5' : ''}`}><span className="mr-2 text-muted-foreground">{index + 1}</span>{label}</button>)}</div>
    <p className="rounded-lg border bg-muted/20 p-3 text-sm">{text('默认保留配置、证书文件、日志、映射和安装缓存。远程处理端的数据不受影响，重新安装后可继续使用。', 'Settings, CA files, logs, mappings and installation caches are retained. Remote processor data is unaffected, and retained data can be reused after reinstalling.')}</p>
    <p role="status" className="text-xs text-muted-foreground">{text('此浏览器会保存步骤和清理选项，返回后可继续；执行结果始终需要重新检查。服务已卸载时，请直接查看脚本打开的本地报告。', 'This browser saves your step and cleanup choices. Results must be checked again when you return. Once the service is removed, read the local report opened by the script.')}</p>
    {resumeReason && <p role="status" className="rounded border border-amber-400 p-3 text-sm">{resumeReason}</p>}
    {checking && !current && <p role="status" className="rounded border p-3 text-sm">{text('正在恢复进度并检查本机状态，请稍候。', 'Restoring progress and checking the local service…')}</p>}
    {status && !managed && <p role="status" className="rounded border border-amber-400 p-3 text-sm">{text('当前实例未通过安装脚本登记。可以查看引导和下载脚本，但脚本会报告“未登记安装”，不会移除其他实例的系统服务。开发实例请通过启动它的终端停止。', 'This instance is not registered by the installer. You can inspect the guide and download the script, but it will report an unmanaged installation and leave other services alone. Stop development instances from their launching terminal.')}</p>}
    {error && <p role="alert" className="rounded border border-red-300 p-3 text-sm text-red-600">{error}</p>}
    {disconnected && <p role="status" className="rounded border border-amber-400 p-3 text-sm">{text('本机服务已断开。若刚执行卸载，请查看自动打开的本地检查报告；本页保留操作说明。', 'The local service disconnected. If you just uninstalled, read the local verification report. These instructions remain available.')}</p>}
    <Card><CardHeader><CardTitle>{steps[step]}</CardTitle></CardHeader><CardContent className="space-y-4">
      {step === 0 && <>
        <p className="text-sm">{text('先停止接管请求，再移除组件。停止后，新连接将沿原网络链路访问；需要保护的任务可稍后重新开启。', 'Stop intercepting requests before removing components. New connections will then use their original network path.')}</p>
        <div role="status" className="flex items-start gap-2 rounded border p-3 text-sm">{stopped ? <CheckCircle2 className="mt-0.5 h-4 w-4 text-emerald-600" /> : <Circle className="mt-0.5 h-4 w-4 text-muted-foreground" />}<span>{stopped ? text('当前状态：捕获已停止。继续前会再检查一次。', 'Capture is stopped. It will be checked again before continuing.') : text('当前状态：尚未确认捕获已停止。', 'Capture has not been confirmed stopped.')}</span></div>
        <Button disabled={busy || maintenanceBusy || !current} onClick={() => void checkStop(!stopped)}>{busy ? text('正在检查…', 'Checking…') : stopped ? text('检查并继续', 'Check & continue') : text('停止捕获并检查', 'Stop capture & check')}</Button>
      </>}
      {step === 1 && <>
        <p className="text-sm">{text('卸载脚本先移除并检查后台服务，然后只清理这次安装对应的系统证书信任。管理员密码仅在终端或系统窗口输入。', 'The script removes and verifies the service first, then cleans up only this installation’s CA trust. Enter administrator passwords only in the terminal or system prompt.')}</p>
        <label className="flex cursor-pointer items-start gap-3 rounded border p-3 text-sm"><input className="mt-1" type="radio" name="uninstall-ca" checked={!keepCertificate} onChange={() => { setKeepCertificate(false) }} /><span><strong>{text('清理系统证书信任（推荐）', 'Remove system CA trust (recommended)')}</strong><span className="mt-1 block text-muted-foreground">{text('保留本机证书文件，后续重新安装时可以再次信任。', 'Keep the local CA files so they can be trusted again when reinstalling.')}</span></span></label>
        <label className="flex cursor-pointer items-start gap-3 rounded border p-3 text-sm"><input className="mt-1" type="radio" name="uninstall-ca" checked={keepCertificate} onChange={() => { setKeepCertificate(true) }} /><span><strong>{text('保留系统证书信任', 'Keep system CA trust')}</strong><span className="mt-1 block text-muted-foreground">{text('适合准备立即重新安装；卸载报告会明确标注“按选择保留”。', 'Useful when reinstalling immediately. The report will explicitly show trust as retained.')}</span></span></label>
        {certificate && <div className="space-y-1 rounded border bg-muted/20 p-3 text-xs"><p>{current ? text('本次安装的证书：', 'This installation’s CA: ') : text('上次记录的证书，等待重新核对：', 'Last recorded CA; awaiting verification: ')}{certificate.subject}</p><p>SHA-256</p><code className="block break-all">{certificate.sha256.match(/.{1,2}/g)?.join(' ')}</code></div>}
        <details className="rounded border p-3 text-sm"><summary className="cursor-pointer">{text('系统授权、独立证书库与共享组件', 'System prompts, app trust stores & shared components')}</summary><div className="mt-3 space-y-2">
          {platform === 'darwin' ? <><p>{text('若 macOS 拒绝自动撤销信任：打开“钥匙串访问 → 系统 → 证书”，按上方指纹找到本次 CA，将“信任 → 使用此证书时”改回“使用系统默认”，关闭详情并完成系统确认，然后重试脚本。', 'If macOS refuses automatic removal, find this CA by fingerprint in Keychain Access → System → Certificates. Set Trust → When using this certificate to Use System Defaults, close the details, complete confirmation and retry the script.')}</p><p>{text('官方 Mitmproxy 网络扩展可能与其他应用共用，因此保留。确认没有其他用途后，可在“系统设置 → 通用 → 登录项与扩展 → 网络扩展”停用。', 'The official Mitmproxy extension may be shared with other apps, so it is retained. If no longer needed, disable it in System Settings → General → Login Items & Extensions → Network Extensions.')}</p></> : platform === 'windows' ? <p>{text('脚本清理“当前用户”的本次根证书。手动导入到“本地计算机”的副本需在 certlm.msc 中核对指纹后清理；报告会提示残留。', 'The script removes this CA from Current User. Copies manually imported into Local Machine must be removed by fingerprint in certlm.msc; the report will flag them.')}</p> : <p>{text('Ubuntu 会校验本次证书文件的指纹，只移除匹配的 silencer 证书并更新系统证书库。', 'Ubuntu verifies the CA fingerprint, removes only its matching silencer certificate and updates the system trust store.')}</p>}
          <p>{text('应用独立证书库中手动导入的副本需要在相应应用中清理。系统代理、Clash/VPN、路由和 DNS 保持原设置。', 'Remove manually imported copies from app-specific trust stores in those apps. System proxy, Clash/VPN, routing and DNS settings are preserved.')}</p>
        </div></details>
        <Button disabled={busy} onClick={() => setStep(2)}>{stopped && !disconnected ? text('确认范围，下一步', 'Confirm scope & continue') : text('查看卸载命令与报告说明', 'View commands & report instructions')}</Button>
      </>}
      {step === 2 && <>
        <ol className="list-decimal space-y-3 pl-5 text-sm leading-6"><li>{text('下载下方卸载脚本。在终端进入下载目录，执行页面命令。脚本已绑定当前安装，不需要填写令牌，也不需要联网下载组件。', 'Download the script, open a terminal in your downloads folder and run the command below. It is bound to this installation and needs no token or component download.')}</li><li>{text('按终端或系统窗口提示完成授权。脚本会检查服务注册、后台进程和本次证书信任。失败项会保留具体错误，可处理后重试。', 'Complete terminal or system authorization. The script checks service registration, background processes and this CA trust. Resolve reported errors and retry when needed.')}</li><li>{text('完成后自动打开本地“卸载报告”。报告显示真实检查结果；即使本机服务已经退出，仍能查看。', 'The script opens a local uninstall report with actual verification results. It remains readable after the local service exits.')}</li></ol>
        <p className="text-sm font-medium">{keepCertificate ? text('本次选择：保留证书信任。', 'Selected: keep CA trust.') : text('本次选择：清理证书信任。', 'Selected: remove CA trust.')}</p>
        <Button disabled={busy || maintenanceBusy || !stopped || disconnected || !platform} onClick={() => void download()}><Download className="mr-2 h-4 w-4" />{downloaded ? text('重新下载卸载脚本', 'Download script again') : text('下载本机卸载脚本', 'Download uninstall script')}</Button>
        <div className="flex items-start justify-between gap-2 rounded border bg-muted/30 p-3"><code className="break-all text-xs">{command}</code><Button size="sm" variant="ghost" aria-label={text('复制卸载命令', 'Copy uninstall command')} onClick={() => void copyText(command)}><Copy className="h-4 w-4" /></Button></div>
        {downloaded && <p role="status" className="flex items-start gap-2 rounded border p-3 text-sm"><TriangleAlert className="mt-0.5 h-4 w-4 shrink-0" />{text('上次已发起脚本下载，尚未确认执行。请按上方步骤操作，以脚本生成的报告为准。', 'A script download was requested previously; execution is not confirmed. Follow the steps above and use its report to confirm completion.')}</p>}
        <Button variant="outline" disabled={busy} onClick={() => void checkConnection()}><RefreshCw className="mr-2 h-4 w-4" />{text('检查本机连接', 'Check local connection')}</Button>
        {connectionChecked && <p role="status" className="text-sm text-muted-foreground">{text('本机服务仍在线。连接正常不代表已卸载，请执行脚本并查看最终报告。', 'The local service is still online. This does not mean it is uninstalled; run the script and read its final report.')}</p>}
      </>}
      {step > 0 && <div className="border-t pt-4"><Button variant="outline" disabled={busy} onClick={() => { setStep(step - 1); setError('') }}>{text('上一步', 'Back')}</Button></div>}
    </CardContent></Card>
  </section>
}
