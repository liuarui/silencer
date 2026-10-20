import { CheckCircle2, Circle, TriangleAlert } from 'lucide-react'
import { captureAttentionReasons, captureIsActive } from '@/lib/capture-lifecycle'
import type { CaptureStatus } from '@/api/capture'

export function CaptureReadiness({ status, current, checking, text, explain }: { status?: CaptureStatus; current: boolean; checking: boolean; text: (zh: string, en: string) => string; explain: (code: string) => string }) {
  const active = current && captureIsActive(status)
  const observed = current && (status?.observed_requests ?? 0) > 0
  const needsRepair = current && status?.config.enabled && !active
  const reasons = current ? captureAttentionReasons(status) : []
  const pipelinePending = reasons.length === 1 && reasons[0].code === 'pipeline_check_required'
  const reasonLabels = {
    platform: text('系统与内核', 'System & kernel'), component: text('捕获组件', 'Capture runtime'),
    permissions: text('系统权限', 'Permissions'), certificate: text('证书信任', 'CA trust'),
    pipeline: text('处理链路', 'Processing pipeline'), runtime: text('捕获进程', 'Capture process'),
    targets: text('目标规则', 'Target rules'), last_error: text('后台错误', 'Service error'),
  }
  const Icon = needsRepair ? TriangleAlert : active ? CheckCircle2 : Circle
  return <div aria-label={text('本机保护状态', 'Local protection status')} className="grid gap-3 md:grid-cols-3">
    <div className="rounded-lg border bg-muted/20 p-4"><p className="flex items-center gap-2 text-sm font-medium">{current ? <CheckCircle2 className="h-4 w-4 text-emerald-600" /> : <Circle className="h-4 w-4 text-muted-foreground" />}{text('本机服务', 'Local service')}</p><p className="mt-2 text-sm">{current ? text('已连接', 'Connected') : checking ? text('正在重新检查', 'Checking connection') : text('连接中断，状态待确认', 'Disconnected; status unconfirmed')}</p></div>
    <div className="rounded-lg border bg-muted/20 p-4"><p className="flex items-center gap-2 text-sm font-medium"><Icon className={`h-4 w-4 ${needsRepair ? 'text-amber-600' : active ? 'text-emerald-600' : 'text-muted-foreground'}`} />{text('捕获保护', 'Capture protection')}</p><p className="mt-2 text-sm">{!current ? text('等待本机检查', 'Awaiting local check') : active ? text('已开启，捕获条件检查通过', 'Enabled; capture checks passed') : needsRepair ? pipelinePending ? text('处理链路待检查', 'Processing pipeline needs verification') : text('保护异常，需要检查', 'Protection needs attention') : text('未开启捕获', 'Capture is off')}</p>{needsRepair && reasons.length > 0 && <ul aria-label={text('需要处理的检查项', 'Checks needing attention')} className="mt-2 space-y-1 text-xs text-amber-700 dark:text-amber-400">{reasons.map((reason) => <li key={reason.key}>{reasonLabels[reason.key]}：{explain(reason.code)}</li>)}</ul>}</div>
    <div className="rounded-lg border bg-muted/20 p-4"><p className="flex items-center gap-2 text-sm font-medium"><Circle className="h-4 w-4 text-muted-foreground" />{text('客户端请求验证', 'Client request verification')}</p><p className="mt-2 text-sm">{observed ? text(`本次后台运行已观察到 ${status?.observed_requests} 次捕获请求`, `${status?.observed_requests} capture requests observed in this service session`) : text('尚未确认客户端实际覆盖', 'Client coverage is not yet confirmed')}</p><p className="mt-1 text-xs text-muted-foreground">{text('从原客户端发送请求后，在账本中核对脱敏与还原；服务运行或请求计数不能代替核验。', 'Send a request from your client and verify masking and restoration in the ledger. Service status and request counts alone do not prove protection.')}</p></div>
  </div>
}
