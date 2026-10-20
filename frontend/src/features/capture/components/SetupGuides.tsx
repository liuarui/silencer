import { Download, ExternalLink, CheckCircle2 } from 'lucide-react'
import { Button } from '@/components/ui/button'

type Text = (zh: string, en: string) => string
type GuideProps = { system: string; text: Text; busy: boolean; onAction: (action: string) => void }

export function PermissionGuide({ system, text, busy, onAction, ready }: GuideProps & { ready: boolean }) {
  return <div className="space-y-3 rounded-lg border bg-muted/20 p-4">
    <p className="font-medium">{ready ? text('系统捕获权限已就绪，可以继续。', 'Capture permission is ready. You can continue.') : text('在系统中允许捕获组件', 'Allow capture in your system')}</p>
    <details open={!ready || undefined}><summary className="cursor-pointer text-sm">{text('查看系统授权步骤', 'Show system authorization steps')}</summary>
    <ol className="mt-3 list-decimal space-y-2 pl-5 text-sm leading-6">
      <li>{text('首次安装时，点击下方“授权并检查”启动官方捕获组件。此安装步骤不启用业务目标。', 'During initial setup, select Authorize & check below to start the official component. This setup step does not activate business targets.')}</li>
      {system === 'darwin' ? <>
        <li>{text('打开“系统设置 → 通用 → 登录项与扩展”，切换到“按类别”，打开“网络扩展”右侧的详情。', 'Open System Settings → General → Login Items & Extensions, select By Category, then open Network Extensions details.')}</li>
        <li>{text('开启“Mitmproxy Redirector.app”，完成系统确认。如果系统提示扩展被阻止，请按提示前往“隐私与安全性”允许。', 'Enable Mitmproxy Redirector.app and complete system confirmation. If macOS reports a blocked extension, follow its prompt to allow it in Privacy & Security.')}</li>
      </> : system === 'windows' ? <li>{text('完成 Windows 的权限确认，允许官方捕获组件运行。', 'Complete the Windows permission prompt for the official capture component.')}</li> : <li>{text('在系统授权窗口中完成确认。Ubuntu 需要 sudo、lsof，以及 zenity 或 kdialog；缺少依赖时页面会显示具体原因。', 'Complete the system authorization prompt. Ubuntu needs sudo, lsof, and zenity or kdialog; missing dependencies are reported here.')}</li>}
      <li>{text('返回此页检查。只有捕获组件实际就绪，才能进入下一步。', 'Return here and check. Continue only after the component reports actual readiness.')}</li>
    </ol>
    {system === 'darwin' && <Button variant="outline" disabled={busy} onClick={() => onAction('open_permission_settings')}><ExternalLink className="mr-2 h-4 w-4" />{text('打开系统设置', 'Open System Settings')}</Button>}
    </details>
    <p className="text-xs text-muted-foreground">{text('授权密码只在系统窗口输入。网页关闭后，本机后台仍可运行。', 'Enter authorization passwords only in the system prompt. The local service can continue after this page closes.')}</p>
  </div>
}

export function CertificateGuide({ system, text, busy, onAction, certificate, trusted, manual, onDownload }: GuideProps & {
  certificate?: { subject: string; sha256: string } | null; trusted: boolean; manual: boolean; onDownload: () => void
}) {
  return <div className="space-y-4">
    <p className="text-sm">{text('每台电脑独立生成证书，用于保护 HTTPS 请求。先安装本机证书，再检查实际信任状态。', 'Each installation generates its own CA to protect HTTPS requests. Install this computer’s CA, then verify actual trust.')}</p>
    {trusted ? <p className="flex items-center gap-2 rounded border border-emerald-500/30 p-3 text-sm"><CheckCircle2 className="h-4 w-4 text-emerald-600" />{text('系统已信任此安装的证书，可以继续检查处理链路。', 'The system trusts this installation’s CA. Continue to verify the processing pipeline.')}</p> : <div className="flex flex-wrap gap-2">
      {!certificate && <Button disabled={busy} onClick={() => onAction('certificate')}>{text('生成本机证书', 'Generate local CA')}</Button>}
      {!manual && <Button disabled={busy} onClick={() => onAction('trust_certificate')}>{text('安装并信任证书', 'Install & trust CA')}</Button>}
    </div>}
    <details className="rounded-lg border bg-muted/20 p-4" open={manual || undefined}>
      <summary className="cursor-pointer text-sm font-medium">{text('需要手动完成？查看逐步操作', 'Need to finish manually? Show steps')}</summary>
      <ol className="mt-3 list-decimal space-y-2 pl-5 text-sm leading-6">
        <li>{text('下载下方“本机证书”。如果证书尚未导入系统，双击下载的文件并按系统提示导入。', 'Download this computer’s CA below. If it has not been imported yet, open the downloaded file and follow the system prompt.')}</li>
        {system === 'darwin' ? <>
          <li>{text('打开“钥匙串访问”，选择“系统 → 证书”，找到“Silencer Local Capture”。', 'Open Keychain Access → System → Certificates and find Silencer Local Capture.')}</li>
          <li>{text('双击证书，展开“信任”，将“使用此证书时”设为“始终信任”。', 'Open the certificate, expand Trust, and set When using this certificate to Always Trust.')}</li>
          <li>{text('关闭证书详情窗口，完成 macOS 的管理员确认，然后返回此页。', 'Close the certificate details, complete the macOS administrator confirmation, and return here.')}</li>
        </> : system === 'windows' ? <li>{text('在证书安装向导中选择“当前用户”，将本机 CA 安装到“受信任的根证书颁发机构”，完成系统确认。', 'In the certificate import wizard, select Current User and Trusted Root Certification Authorities, then complete system confirmation.')}</li> : <li>{text('Ubuntu：将下载的 .crt 文件复制到 /usr/local/share/ca-certificates/，再执行 sudo update-ca-certificates。网页的安装按钮会通过系统授权窗口完成同一操作。', 'Ubuntu: copy the downloaded .crt into /usr/local/share/ca-certificates/ and run sudo update-ca-certificates. The installation button performs this through system authorization.')}</li>}
        <li>{text('点击“检查证书并继续”。实际检查通过后，向导才会前进。', 'Select Check certificate & continue. The wizard advances only after actual verification succeeds.')}</li>
      </ol>
      <div className="mt-3 flex flex-wrap gap-2">
        <Button variant="outline" disabled={busy || !certificate} onClick={onDownload}><Download className="mr-2 h-4 w-4" />{text('下载本机证书', 'Download local CA')}</Button>
        {system === 'darwin' && <Button variant="outline" disabled={busy} onClick={() => onAction('open_keychain')}><ExternalLink className="mr-2 h-4 w-4" />{text('打开钥匙串访问', 'Open Keychain Access')}</Button>}
      </div>
      {certificate && <div className="mt-3 space-y-1 text-xs text-muted-foreground"><p>{text('仅信任当前安装生成的证书，可在证书“细节”中核对 SHA-256：', 'Trust only the CA generated by this installation. Verify SHA-256 in certificate details:')}</p><p className="font-medium">{certificate.subject}</p><code className="block break-all">{certificate.sha256.match(/.{1,2}/g)?.join(' ')}</code></div>}
    </details>
    <p className="text-xs text-muted-foreground">{text('有些应用使用独立证书库，安装后可能需要重启应用或单独导入证书。最终以该应用的真实请求记录为准。', 'Some apps use their own trust stores and may need restarting or a separate CA import. Verify protection using an actual request from each app.')}</p>
  </div>
}
