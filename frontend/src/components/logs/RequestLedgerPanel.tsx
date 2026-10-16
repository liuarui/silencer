import { usePrivacyStore } from '@/stores/privacyStore'
import { PlaintextSwitch } from '@/components/common/PlaintextSwitch'
import { projectPrivateBodies } from '@/lib/privacy'
import { useEffect, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { ChevronDown, RefreshCw, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useI18n } from '@/lib/i18n'
import { silencerFetch } from '@/lib/silencer-fetch'
import { toast } from '@/lib/toast'

type RequestItem = {
  request_id: string
  started: number
  ended?: number | null
  status: string
  noise_whitelisted?: boolean
  abnormal?: boolean
  anomaly?: string
  error?: string
  failure_stage?: string
  ingress?: 'proxy' | 'ext'
  host?: string
  body_scope?: string
  mask_count?: number
  mask_status?: string
  restored_count?: number
  unresolved_count?: number
  restore_status?: string
  response_status?: string
  upstream_status?: number
  upstream_status_observed?: boolean
  client?: string
  upstream?: string
  model?: string
  path?: string
  truncated?: number
  body_unavailable?: number | boolean
  body_expired?: boolean
  body_storage_error?: boolean
  request_body_incomplete?: boolean
}

type RequestDetail = RequestItem & {
  raw?: string | null
  masked?: string | null
  upstream_response?: string | null
  restored_response?: string | null
  detail_recorded?: boolean
  items?: { label?: string; original?: string; preview?: string; tok?: string; restored?: boolean; restore_status?: string }[]
  matched_rules?: string[]
}

const STATUS_STYLES: Record<string, string> = {
  completed: 'text-emerald-600',
  blocked: 'text-amber-600',
  failed: 'text-red-600',
  cancelled: 'text-muted-foreground',
  error: 'text-red-600',
  awaiting_response: 'text-amber-600',
  running: 'text-blue-600',
}

function statusStyle(status: string) {
  const code = Number(status)
  if (code >= 100) return code >= 400 ? 'text-red-600' : 'text-emerald-600'
  return STATUS_STYLES[status] || 'text-muted-foreground'
}

const STAGES = [
  ['raw', 'logs.ledger.raw'],
  ['masked', 'logs.ledger.masked'],
  ['upstream_response', 'logs.ledger.upstreamResponse'],
  ['restored_response', 'logs.ledger.restoredResponse'],
] as const

function highlightTerms(text: string, terms: string[], className: string): React.ReactNode {
  const unique = [...new Set(terms.filter(Boolean))].sort((a, b) => b.length - a.length)
  if (!unique.length) return text
  const rx = new RegExp(unique.map((term) => term.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g')
  const nodes: React.ReactNode[] = []
  let last = 0
  for (const match of text.matchAll(rx)) {
    const at = match.index ?? 0
    if (at > last) nodes.push(text.slice(last, at))
    nodes.push(<mark key={`${at}-${match[0]}`} className={className}>{match[0]}</mark>)
    last = at + match[0].length
  }
  if (last < text.length) nodes.push(text.slice(last))
  return nodes
}

/** Lists request metadata first; request bodies load only when a row is selected. */
export function RequestLedgerPanel() {
  const { t, tf } = useI18n()
  const queryClient = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const showPlaintext = usePrivacyStore((state) => state.showPlaintext)
  const [open, setOpen] = useState(true)
  const [ingress, setIngress] = useState('all')
  const requestedOutcome = searchParams.get('request_outcome') || 'all'
  const outcome = ['all', 'abnormal', 'block', 'restore_failed', 'error', 'response_warning'].includes(requestedOutcome) ? requestedOutcome : 'all'
  const listQuery = useQuery({
    queryKey: ['requestLedger', { ingress, outcome }],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({ limit: '100', outcome, items_only: '1' })
      if (ingress !== 'all') query.set('ingress', ingress)
      return silencerFetch<{ items: RequestItem[] }>(`/api/requests?${query}`, { signal })
    },
    enabled: open,
    staleTime: 2000,
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
    retry: false,
  })
  const items = listQuery.data?.items ?? []
  const loading = listQuery.isFetching
  const detailQuery = useQuery({
    queryKey: ['requestLedgerDetail', selectedId],
    queryFn: ({ signal }) => silencerFetch<{ request: RequestDetail }>(
      `/api/requests/${encodeURIComponent(selectedId!)}`, { signal },
    ),
    enabled: open && selectedId !== null,
    gcTime: 0,
    staleTime: 0,
    refetchInterval: (query) => ['running', 'awaiting_response'].includes(query.state.data?.request.status ?? '') ? 5000 : false,
    refetchIntervalInBackground: false,
    retry: false,
  })
  const selectedRaw = detailQuery.data?.request ?? null
  const selected = useMemo(() => selectedRaw ? projectPrivateBodies(selectedRaw, showPlaintext) : null, [selectedRaw, showPlaintext])
  const detailLoading = selectedId !== null && detailQuery.isPending
  useEffect(() => {
    if (listQuery.error) toast(tf('logs.ledgerReadFailed', { e: listQuery.error.message }), 'error')
  }, [listQuery.error, tf])
  useEffect(() => {
    if (detailQuery.error) toast(tf('logs.ledgerDetailFailed', { e: detailQuery.error.message }), 'error')
  }, [detailQuery.error, tf])
  const showDetail = (id: string) => {
    if (selectedId === id) void detailQuery.refetch()
    else setSelectedId(id)
  }
  const closeDetail = () => setSelectedId(null)

  const statusText = (status: string) => ['running', 'awaiting_response', 'completed', 'error'].includes(status)
    ? t(`logs.ledgerState.${status}`) : status
  const changeOutcome = (value: string) => {
    closeDetail()
    const next = new URLSearchParams(searchParams)
    if (value === 'all') next.delete('request_outcome')
    else next.set('request_outcome', value)
    setSearchParams(next, { replace: true })
  }

  return (
    <Card className="shrink-0">
      <CardHeader className="flex-row items-center justify-between space-y-0 py-3">
        <button
          type="button"
          className="flex min-w-0 items-center gap-1.5 text-left"
          aria-expanded={open}
          onClick={() => {
            if (open) {
              closeDetail()
              void queryClient.cancelQueries({ queryKey: ['requestLedger', { ingress, outcome }], exact: true })
            }
            setOpen((value) => !value)
          }}
        >
          <ChevronDown
            className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${open ? '' : '-rotate-90'}`}
            aria-hidden="true"
          />
          <span className="truncate text-sm font-semibold">{t('logs.ledgerTitle')}</span>
          <span className="hidden text-[11px] text-muted-foreground sm:inline">
            {t('logs.ledgerDescription')}
          </span>
        </button>
        <Button
          variant="ghost"
          size="sm"
          className="h-7"
          onClick={() => void listQuery.refetch()}
          disabled={loading}
          title={t('logs.ledgerRefresh')}
          aria-label={t('logs.ledgerRefresh')}
        >
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? 'animate-spin' : ''}`} aria-hidden="true" />
        </Button>
      </CardHeader>

      {open && (
        <CardContent className="space-y-2 pt-0">
          <div className="flex flex-wrap items-center gap-2">
            <Select value={outcome} onValueChange={changeOutcome}>
              <SelectTrigger className="h-7 w-36 text-xs" aria-label={t('logs.ledgerOutcomeFilter')}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {['all', 'abnormal', 'block', 'restore_failed', 'error', 'response_warning'].map((value) => (
                  <SelectItem key={value} value={value}>{t(`logs.ledgerOutcome.${value}`)}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            {['all', 'proxy', 'ext'].map((source) => (
              <Button key={source} size="sm" variant={ingress === source ? 'secondary' : 'ghost'} className="h-6 text-[10px]" onClick={() => {
                closeDetail()
                setIngress(source)
              }}>
                {t(`logs.ingress.${source}`)}
              </Button>
            ))}
          </div>
          <div className="max-h-56 overflow-auto rounded border">
            <div className="grid min-w-[560px] grid-cols-[150px_72px_minmax(90px,1fr)_minmax(90px,1fr)_minmax(140px,2fr)] gap-2 border-b bg-muted/30 px-2 py-1.5 text-[10px] text-muted-foreground">
              <span>{t('logs.ledgerTimeId')}</span>
              <span>{t('logs.ledgerStatus')}</span>
              <span>{t('logs.ledgerClient')}</span>
              <span>{t('logs.ledgerUpstream')}</span>
              <span>{t('logs.ledgerPath')}</span>
            </div>
            {items.map((item) => (
              <button
                type="button"
                key={item.request_id}
                onClick={() => void showDetail(item.request_id)}
                className="grid w-full min-w-[560px] grid-cols-[150px_72px_minmax(90px,1fr)_minmax(90px,1fr)_minmax(140px,2fr)] gap-2 border-b px-2 py-1.5 text-left text-[10px] hover:bg-muted/30"
                title={item.request_id}
              >
                <span className="truncate font-mono">
                  {new Date(item.started * 1000).toLocaleTimeString()} · {item.request_id.slice(0, 8)}
                </span>
                <span className={item.noise_whitelisted ? 'text-muted-foreground' : statusStyle(item.status)}>
                  {item.noise_whitelisted ? t('logs.noiseWhitelisted') : statusText(item.status)}
                  {item.anomaly && <span className="block text-[9px] text-amber-600">{t(`logs.ledgerOutcome.${item.anomaly}`)}</span>}
                </span>
                <span className="truncate">{item.ingress === 'ext' ? t('logs.ingress.ext') : item.client || t('logs.ingress.proxy')}</span>
                <span className="truncate">{item.host || item.upstream || '—'}</span>
                <span className="truncate font-mono">{item.path || '—'}</span>
              </button>
            ))}
            {!items.length && (
              <p className="p-4 text-center text-xs text-muted-foreground">
                {loading ? t('common.loading') : t('logs.ledgerNoRequests')}
              </p>
            )}
          </div>

          {detailLoading && <p className="text-xs text-muted-foreground">{t('common.loading')}</p>}
          {selected && (
            <section className="rounded border bg-muted/20 p-3 text-xs" aria-label={t('logs.ledgerTitle')}>
              <div className="flex items-center justify-between gap-2">
                <div className="truncate font-medium" title={selected.request_id}>
                  {selected.request_id}
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-6 shrink-0"
                  onClick={closeDetail}
                  title={t('common.close')}
                  aria-label={t('common.close')}
                >
                  <X className="h-3.5 w-3.5" aria-hidden="true" />
                </Button>
              </div>
              <div className="mt-2 flex justify-end"><PlaintextSwitch /></div>
              <div className="mt-2 flex flex-wrap gap-2">
                <Badge variant="outline" className={selected.noise_whitelisted ? 'text-muted-foreground' : statusStyle(selected.status)}>{selected.noise_whitelisted ? t('logs.noiseWhitelisted') : statusText(selected.status)}</Badge>
                <span>{selected.mask_status === 'not_applied' ? t('logs.ledgerMaskNotApplied') : tf('logs.ledgerMaskCount', { n: selected.mask_count ?? 0 })}</span>
                <span>{tf('logs.ledgerRestoreCount', { n: selected.restored_count ?? 0, left: selected.unresolved_count ?? 0 })}</span>
                {selected.restore_status && <span className="text-muted-foreground">{t(`logs.ledgerRestore.${selected.restore_status}`)}</span>}
                {selected.upstream_status_observed && <span>HTTP {selected.upstream_status}</span>}
              </div>
              {(selected.error || selected.failure_stage) && (
                <div className="mt-2 rounded bg-red-500/10 p-2 text-red-600">
                  {selected.error && <p>{tf('logs.ledgerError', { e: t(`logs.failure.${selected.error}`) === `logs.failure.${selected.error}` ? selected.error : t(`logs.failure.${selected.error}`) })}</p>}
                  {selected.failure_stage && <p className="mt-1 text-xs">{tf('logs.failureStage', { stage: t(`logs.failureStage.${selected.failure_stage}`) === `logs.failureStage.${selected.failure_stage}` ? selected.failure_stage : t(`logs.failureStage.${selected.failure_stage}`) })}</p>}
                </div>
              )}
              {selected.body_scope === 'bridge_text' && <p className="mt-2 text-[10px] text-muted-foreground">{t('logs.ledgerBridgeScope')}</p>}
              {selected.detail_recorded && (selected.items?.length || selected.matched_rules?.length) ? (
                <div className="mt-2 rounded border bg-background/70 p-2">
                  <div className="mb-1 text-[10px] font-semibold text-muted-foreground">{t('detail.matchedRules')}</div>
                  <div className="flex flex-wrap gap-1">
                    {(selected.matched_rules ?? [...new Set((selected.items ?? []).map((item) => item.label).filter(Boolean) as string[])]).map((rule) => (
                      <Badge key={rule} variant="outline" className="font-mono text-[9px]">{rule}</Badge>
                    ))}
                  </div>
                </div>
              ) : null}
              <div className="mt-2 grid gap-2 md:grid-cols-2">
                {STAGES.map(([stage, label]) => (
                  <div key={stage}>
                    <div className="mb-1 flex items-center gap-1 text-[10px] text-muted-foreground">
                      <Badge variant="outline" className="text-[9px]">{t(label)}</Badge>
                      {selected[stage] == null && t('logs.ledgerUnavailable')}
                    </div>
                    <pre className="max-h-36 overflow-auto whitespace-pre-wrap break-all rounded bg-background p-2 font-mono text-[10px]">
                      {selected[stage]
                        ? highlightTerms(
                          selected[stage]!,
                          stage === 'raw' || stage === 'restored_response'
                            ? (selected.items ?? []).map((item) => item.original ?? '').filter(Boolean)
                            : (selected.items ?? []).map((item) => item.tok ?? item.preview ?? '').filter(Boolean),
                          stage === 'raw' || stage === 'restored_response'
                            ? 'rounded bg-emerald-500/25 px-0.5 ring-1 ring-emerald-500/40'
                            : 'rounded bg-blue-500/15 px-0.5 text-blue-700 ring-1 ring-blue-500/30 dark:text-blue-300',
                        )
                        : '—'}
                    </pre>
                  </div>
                ))}
              </div>
              {selected.request_body_incomplete && (
                <p className="mt-2 text-[10px] text-amber-600">{t('logs.ledgerUploadIncomplete')}</p>
              )}
              {!selected.detail_recorded ? (
                <p className="mt-2 text-[10px] text-amber-600">{t('logs.ledgerDisabled')}</p>
              ) : (selected.truncated || selected.body_unavailable) && (
                <p className="mt-2 text-[10px] text-amber-600">{t(selected.body_storage_error ? 'logs.ledgerStorageError' : selected.body_expired ? 'logs.ledgerExpired' : selected.truncated ? 'logs.ledgerLimit' : 'logs.ledgerNoBody')}</p>
              )}
            </section>
          )}
        </CardContent>
      )}
    </Card>
  )
}
