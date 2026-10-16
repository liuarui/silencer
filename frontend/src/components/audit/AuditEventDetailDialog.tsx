import { usePrivacyStore } from '@/stores/privacyStore'
import { PlaintextSwitch } from '@/components/common/PlaintextSwitch'
import { HIDDEN_TEXT } from '@/lib/privacy'
/**
 * 审计事件详情弹窗（审计中心与拦截日志页共用）：
 * - 日志页审计行点击后原地打开（此前跳转审计中心，用户要求本页查看）
 * - 原因分析 + 危害说明 + 证据 + 请求 + 探针 ID
 */
import { useState } from 'react'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import type { AuditEvent, AuditSeverity } from '@/types/api'
import { cn, copyText } from '@/lib/utils'
import { useI18n } from '@/lib/i18n'
import dayjs from 'dayjs'

function CopyEvidenceBtn({ text }: { text: string }) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  return (
    <Button
      size="sm"
      variant="ghost"
      className="h-5 px-1.5 text-[10px]"
      onClick={async () => {
        try {
          await copyText(text)
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        } catch {}
      }}
    >
      {copied ? t('detail.copied') : t('detail.copy')}
    </Button>
  )
}

export const SEVERITY_META: Record<AuditSeverity, { labelKey: string; cls: string }> = {
  CRITICAL: {
    labelKey: 'audit.sevCritical',
    cls: 'border-red-500/40 bg-red-500/10 text-red-600 dark:text-red-400',
  },
  HIGH: {
    labelKey: 'audit.sevHigh',
    cls: 'border-orange-500/40 bg-orange-500/10 text-orange-600 dark:text-orange-400',
  },
  MEDIUM: {
    labelKey: 'audit.sevMedium',
    cls: 'border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400',
  },
  LOW: {
    labelKey: 'audit.sevLow',
    cls: 'border-slate-400/40 bg-slate-400/10 text-slate-600 dark:text-slate-400',
  },
}

export function severityMeta(sev: string) {
  return (
    SEVERITY_META[sev as AuditSeverity] ?? {
      labelKey: sev,
      cls: 'border-border bg-muted text-muted-foreground',
    }
  )
}

/** 审计信号说明（弹窗详情引用）：信号 -> {中文名, 原因分析, 危害} */
export const SIGNAL_INFO: Record<string, { labelKey?: string; label?: string; reasonKey: string; impactKey: string }> = {
  error_leak: {
    labelKey: 'audit.function.error_leak', label: '错误泄漏',
    reasonKey: 'audit.function.error_leak.reason', impactKey: 'audit.function.error_leak.impact',
  },
  identity_swap: {
    labelKey: 'audit.function.identity_swap', label: '身份换芯',
    reasonKey: 'audit.function.identity_swap.reason', impactKey: 'audit.function.identity_swap.impact',
  },
  tool_call_rewrite: {
    labelKey: 'audit.function.tool_call_rewrite', label: '工具调用改写',
    reasonKey: 'audit.function.tool_call_rewrite.reason', impactKey: 'audit.function.tool_call_rewrite.impact',
  },
  sse_anomaly: {
    labelKey: 'audit.function.sse_anomaly', label: 'SSE 异常',
    reasonKey: 'audit.function.sse_anomaly.reason', impactKey: 'audit.function.sse_anomaly.impact',
  },
  response_poison: {
    labelKey: 'audit.function.response_poison', label: '响应投毒',
    reasonKey: 'audit.function.response_poison.reason', impactKey: 'audit.function.response_poison.impact',
  },
  cross_request_pollution: {
    labelKey: 'audit.function.cross_request_pollution', label: '跨请求污染',
    reasonKey: 'audit.function.cross_request_pollution.reason', impactKey: 'audit.function.cross_request_pollution.impact',
  },
  dangerous_action: {
    labelKey: 'audit.function.dangerous_action', label: '危险动作',
    reasonKey: 'audit.function.dangerous_action.reason', impactKey: 'audit.function.dangerous_action.impact',
  },
}

/** 未知信号的兜底（结构与已知信号一致，reason/impact 用通用文案） */
export function signalInfo(sig: string) {
  return (
    SIGNAL_INFO[sig] ?? {
      labelKey: sig,
      label: sig,
      reasonKey: 'audit.unknownReason',
      impactKey: 'audit.unknownImpact',
    }
  )
}

/** 凭据回流的 kind 名单（与引擎 `credential_labels.CREDENTIAL_ECHO_KINDS` 同源）。
 * 判据是「以 kind 名开头且紧跟 ` len=`」——**没有冒号**，与下面其它子类型的
 * `kind: …` 形态不同，不能用 `split(':')` 统一处理。
 * 漂移由 `tests/test_regressions.py::test_credential_label_sets_stay_in_sync` 同源的
 * 思路守死：引擎侧改了名单，这里的子类型文案就会失效（回落通用响应投毒文案，不会崩）。 */
const CREDENTIAL_ECHO_KINDS = [
  'github_token', 'google_api_key', 'aliyun_ak', 'tencent_ak',
  'slack_token', 'stripe_key', 'aws_ak', 'jwt',
] as const

/** 子类型文案表：键名与 `evidenceKind()` 的返回值一一对应。 */
const SUBTYPE_INFO: Record<string, { reasonKey: string; impactKey: string }> = {
  credentialEcho: { reasonKey: 'audit.sub.credentialEchoReason', impactKey: 'audit.sub.credentialEchoImpact' },
  hiddenUnicode: { reasonKey: 'audit.sub.hiddenUnicodeReason', impactKey: 'audit.sub.hiddenUnicodeImpact' },
  exfilUrl: { reasonKey: 'audit.sub.exfilUrlReason', impactKey: 'audit.sub.exfilUrlImpact' },
  fakeSystem: { reasonKey: 'audit.sub.fakeSystemReason', impactKey: 'audit.sub.fakeSystemImpact' },
  promptExtraction: { reasonKey: 'audit.sub.promptExtractionReason', impactKey: 'audit.sub.promptExtractionImpact' },
  exfilInstruction: { reasonKey: 'audit.sub.exfilInstructionReason', impactKey: 'audit.sub.exfilInstructionImpact' },
  instructionOverride: { reasonKey: 'audit.sub.instructionOverrideReason', impactKey: 'audit.sub.instructionOverrideImpact' },
  encodedInstruction: { reasonKey: 'audit.sub.encodedInstructionReason', impactKey: 'audit.sub.encodedInstructionImpact' },
}

/**
 * 从证据前缀推导信号**子类型**（审计 A3 / W1-3）。
 *
 * 为什么要这层：一条 `credential_echo:github_token` 事件的 signal_type 只是
 * `response_poison`，弹窗按 signal_type 取文案就会用「Unicode 双向控制符 / 外链载荷」
 * 去解释一条密钥形态命中 —— 用户读到的是「我被攻击了」，实际是 AI 在代码块里
 * 写了个示例令牌。
 *
 * 落库侧零改动：子类型本来就以证据前缀的形式存在（见引擎 `audit_signals.py`
 * 的各类 `evidence` 构造），这里只做读侧推导。
 *
 * 返回 null 表示未命中已知子类型 → 调用方回落到 signal_type 的通用文案。
 */
export function evidenceKind(evidence?: string | null): string | null {
  const ev = (evidence ?? '').trim()
  if (!ev) return null
  // 凭据回流：`<kind> len=N sha256=…`（无冒号）
  if (CREDENTIAL_ECHO_KINDS.some((k) => ev.startsWith(k + ' len='))) return 'credentialEcho'
  // 外链外带：`exfil_url host=… len=… sha256=…`（也无冒号）
  if (ev.startsWith('exfil_url ')) return 'exfilUrl'
  const head = ev.split(':', 1)[0]
  switch (head) {
    case 'hidden_unicode': return 'hiddenUnicode'
    case 'fake_system_block': return 'fakeSystem'
    case 'prompt_extraction': return 'promptExtraction'
    case 'credential_exfil_instruction': return 'exfilInstruction'
    case 'instruction_override': return 'instructionOverride'
    case 'encoded_instruction': return 'encodedInstruction'
    default: return null
  }
}

export function AuditEventDetailDialog({
  event,
  onOpenChange,
}: {
  event: AuditEvent | null
  onOpenChange: (v: boolean) => void
}) {
  const { t } = useI18n()
  const showPlaintext = usePrivacyStore((state) => state.showPlaintext)
  return (
    <Dialog open={event !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        {event && (() => {
          const info = signalInfo(event.signal_type)
          const sev = severityMeta(event.severity)
          const isInfoMode = event.seq === -1
          // 子类型优先：按证据前缀推导（W1-3）。信息模式（点击信号名看说明）没有证据，
          // 仍走 signal_type 的通用文案。
          const sub = isInfoMode ? null : SUBTYPE_INFO[evidenceKind(event.evidence) ?? '']
          const reasonKey = sub ? sub.reasonKey : info.reasonKey
          const impactKey = sub ? sub.impactKey : info.impactKey
          return (
            <>
              <DialogHeader>
                <DialogTitle className="flex items-center gap-2">
                  {isInfoMode ? t('audit.signalInfo') : t('audit.detailTitle')}
                  <Badge variant="outline" className={cn('rounded-full text-[11px]', sev.cls)}>
                    {t(sev.labelKey)}
                  </Badge>
                </DialogTitle>
                <DialogDescription className="flex items-center gap-1.5">
                  <span className="font-mono text-xs">{t(info.labelKey ?? info.label ?? '')}</span>
                  {!isInfoMode && event.ts > 0 && (
                    <span className="text-xs text-muted-foreground">
                      · {dayjs(event.ts * 1000).format('YYYY-MM-DD HH:mm:ss')}
                    </span>
                  )}
                </DialogDescription>
              </DialogHeader>
              <div className="space-y-4 text-sm">
                <div>
                  <div className="mb-1 text-xs font-semibold text-muted-foreground">{t('audit.dialogSignalType')}</div>
                  <code className="rounded bg-muted/60 px-1.5 py-0.5 font-mono text-xs">{t(info.labelKey ?? info.label ?? '')}</code>
                </div>
                {event.action && (
                  <div>
                    <div className="mb-1 text-xs font-semibold text-muted-foreground">{t('audit.guard.mode')}</div>
                    <Badge variant="outline">{t(`audit.guard.result.${event.action}`)}</Badge>
                    {event.tool_guard_rules?.length ? <div className="mt-2 flex flex-wrap gap-1">{[...new Set(event.tool_guard_rules)].map((rule) => <Badge key={rule} variant="secondary">{t(`audit.guard.rule.${rule}`)}</Badge>)}</div> : null}
                  </div>
                )}
                <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
                  <div className="mb-1 text-xs font-semibold text-amber-700 dark:text-amber-400">{t('audit.dialogReason')}</div>
                  <p className="text-xs leading-relaxed text-amber-800/90 dark:text-amber-200/90">{t(reasonKey)}</p>
                </div>
                <div className="rounded-lg border border-red-500/30 bg-red-500/5 p-3">
                  <div className="mb-1 text-xs font-semibold text-red-700 dark:text-red-400">{t('audit.dialogImpact')}</div>
                  <p className="text-xs leading-relaxed text-red-800/90 dark:text-red-200/90">{t(impactKey)}</p>
                </div>
                {event.evidence && !isInfoMode && (
                  <div>
                    <div className="mb-1 flex items-center justify-between">
                      <span className="text-xs font-semibold text-muted-foreground">{t('audit.evidence')}</span>
                      <PlaintextSwitch />
                      <CopyEvidenceBtn text={showPlaintext ? event.evidence : HIDDEN_TEXT} />
                    </div>
                    <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-muted/60 p-2.5 font-mono text-[11px] leading-relaxed text-foreground">
                      {showPlaintext ? event.evidence : HIDDEN_TEXT}
                    </pre>
                  </div>
                )}
                {!isInfoMode && (event.host || event.method || event.path) && (
                  <div>
                    <div className="mb-1 text-xs font-semibold text-muted-foreground">{t('audit.request')}</div>
                    <code className="rounded bg-muted/60 px-1.5 py-0.5 font-mono text-xs">
                      {[event.method, event.host, event.path].filter(Boolean).join(' ')}
                    </code>
                  </div>
                )}
                {!isInfoMode && event.probe_id && (
                  <div>
                    <div className="mb-1 text-xs font-semibold text-muted-foreground">{t('audit.probeId')}</div>
                    <code className="rounded bg-muted/60 px-1.5 py-0.5 font-mono text-xs">{event.probe_id}</code>
                  </div>
                )}
              </div>
              <DialogFooter>
                <Button size="sm" variant="outline" onClick={() => onOpenChange(false)}>
                  {t('audit.close')}
                </Button>
              </DialogFooter>
            </>
          )
        })()}
      </DialogContent>
    </Dialog>
  )
}
