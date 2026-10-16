import { useCallback, useEffect, useRef, useState } from 'react'
import { RefreshCw, RotateCcw, ShieldCheck, Terminal, CheckCircle2, AlertTriangle, HelpCircle, Plus, Trash2, Copy, Check } from 'lucide-react'
import SettingsPage from '@/features/settings/pages/Settings'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { toast } from '@/lib/toast'
import { silencerFetch } from '@/lib/silencer-fetch'
import { useI18n } from '@/lib/i18n'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { getConfig, patchConfig } from '@/api/settings'
import type { ClientProfile, SilencerConfig, UpstreamConfig } from '@/types/api'

type DiscoveredClient = { client: string; version?: string | null; user?: string; config_paths?: string[]; evidence?: string[] }
type Change = { path: string; backup?: string; new_sha256?: string; old_sha256?: string; status?: string }
type Plan = { id?: string; status: string; path?: string; reason?: string; changes?: unknown[]; sha256?: string; new_sha256?: string }

// Public clients use the address currently serving the management UI. Local
// clients run beside the gateway and must never inherit that public hostname.
const LOCAL_GATEWAY_ORIGIN = 'http://127.0.0.1:8080'

function localGatewayOriginFromListenAddr(listenAddr?: string) {
  let address = listenAddr?.trim() || ''
  if (!address) return LOCAL_GATEWAY_ORIGIN
  if (address.startsWith(':')) address = `127.0.0.1${address}`

  try {
    const parsed = new URL(address.includes('://') ? address : `http://${address}`)
    if (!parsed.port) return LOCAL_GATEWAY_ORIGIN
    const host = parsed.hostname
    if (!host || host === '0.0.0.0' || host === '[::]') return `http://127.0.0.1:${parsed.port}`
    return `http://${host}:${parsed.port}`
  } catch {
    return LOCAL_GATEWAY_ORIGIN
  }
}

function clientRoute(upstream: string, protocol: ClientProfile['protocol'], origin: string) {
  const base = `${origin}/r/${encodeURIComponent(upstream)}`
  return protocol === 'openai' ? `${base}/v1` : base
}

/** 宿主机工具发现、接入计划和安全回滚；配置 CRUD 仍复用设置页的成熟表单。 */
export default function ClientsPage() {
  const { t, tf } = useI18n()
  const [clients, setClients] = useState<DiscoveredClient[]>([])
  const [scope, setScope] = useState('')
  const [capability, setCapability] = useState('')
  const [available, setAvailable] = useState(true)
  const [loading, setLoading] = useState(false)
  const [selected, setSelected] = useState<DiscoveredClient | null>(null)
  const [configPath, setConfigPath] = useState('')
  const [proxyUrl, setProxyUrl] = useState(() => `${LOCAL_GATEWAY_ORIGIN}/r/default/v1`)
  const [plan, setPlan] = useState<Plan | null>(null)
  const [change, setChange] = useState<Change | null>(null)
  const [history, setHistory] = useState<Change[]>([])
  const [busy, setBusy] = useState(false)
  const [upstreams, setUpstreams] = useState<UpstreamConfig[]>([])
  const [profiles, setProfiles] = useState<ClientProfile[]>([])
  const [profileName, setProfileName] = useState('')
  const [profileUpstream, setProfileUpstream] = useState('')
  const [profileProtocol, setProfileProtocol] = useState<ClientProfile['protocol']>('openai')
  const [copiedProfile, setCopiedProfile] = useState('')
  const [localOrigin, setLocalOrigin] = useState(LOCAL_GATEWAY_ORIGIN)
  const proxyUrlEdited = useRef(false)

  const loadProfiles = useCallback(async () => {
    try {
      const cfg = await getConfig()
      const nextLocalOrigin = localGatewayOriginFromListenAddr(cfg.listen_addr)
      setLocalOrigin(nextLocalOrigin)
      setProxyUrl((current) => proxyUrlEdited.current ? current : `${nextLocalOrigin}/r/default/v1`)
      setUpstreams(cfg.upstreams || [])
      setProfiles((cfg.client_profiles || []) as ClientProfile[])
      setProfileUpstream((current) => current || cfg.upstreams?.[0]?.name || '')
    } catch (e) { toast(e instanceof Error ? e.message : t('clients.readFailed'), 'error') }
  }, [])

  const discover = useCallback(async () => {
    setLoading(true)
    try {
      const data = await silencerFetch<{ clients: DiscoveredClient[]; scope: string; available?: boolean; capability?: string }>('/api/clients/discover')
      const historyData = await silencerFetch<{ changes: Change[] }>('/api/clients/history')
      const hostAvailable = data.available !== false
      setClients(data.clients || []); setScope(data.scope || 'gateway'); setAvailable(hostAvailable); setCapability(data.capability || '')
      setHistory(historyData.changes || [])
      if (!hostAvailable) {
        setSelected(null); setConfigPath(''); setPlan(null); setChange(null)
      } else if (!selected && data.clients?.[0]) { setSelected(data.clients[0]); setConfigPath(data.clients[0].config_paths?.[0] || '') }
    } catch (e) { toast(e instanceof Error ? e.message : t('clients.scanFailed'), 'error') }
    finally { setLoading(false) }
  }, [selected])
  useEffect(() => { void discover(); void loadProfiles() }, [discover, loadProfiles])

  const saveProfiles = async (next: ClientProfile[]) => {
    setBusy(true)
    try {
      const result = await patchConfig({ key: 'client_profiles', op: 'set', value: next })
      setProfiles(((result.config as SilencerConfig).client_profiles || []) as ClientProfile[])
      toast(t('clients.saved'), 'success')
    } catch (e) { toast(e instanceof Error ? e.message : t('clients.saveFailed'), 'error') }
    finally { setBusy(false) }
  }

  const addProfile = async () => {
    const name = profileName.trim()
    if (!name || !profileUpstream) return toast(t('clients.fillName'), 'error')
    if (profiles.some((item) => item.name.toLowerCase() === name.toLowerCase())) return toast(t('clients.nameExists'), 'error')
    await saveProfiles([...profiles, { name, upstream: profileUpstream, protocol: profileProtocol }])
    setProfileName('')
  }

  const copyProfile = async (profile: ClientProfile) => {
    const domainUrl = clientRoute(profile.upstream, profile.protocol, window.location.origin)
    const localUrl = clientRoute(profile.upstream, profile.protocol, localOrigin)
    const example = profile.protocol === 'anthropic'
      ? `DOMAIN_BASE_URL=${domainUrl}\nLOCAL_BASE_URL=${localUrl}`
      : profile.protocol === 'gemini'
        ? `DOMAIN_BASE_URL=${domainUrl}\nLOCAL_BASE_URL=${localUrl}`
        : `DOMAIN_BASE_URL=${domainUrl}\nLOCAL_BASE_URL=${localUrl}`
    try {
      await navigator.clipboard.writeText(`${t('clients.domain')}：${domainUrl}\n${t('clients.local')}：${localUrl}\n\n${example}`)
      setCopiedProfile(`${profile.name}:all`)
      window.setTimeout(() => setCopiedProfile(''), 1400)
      toast(t('clients.copyDone'), 'success')
    } catch { toast(t('clients.copyFailed'), 'error') }
  }

  const makePlan = async () => {
    if (!available) return toast(t('clients.hostAgentUnavailable'), 'error')
    if (!configPath) return toast(t('clients.chooseConfig'), 'error')
    setBusy(true)
    try {
      const data = await silencerFetch<{ plan: Plan }>('/api/clients/plan', { method: 'POST', body: JSON.stringify({ config_path: configPath, proxy_url: proxyUrl }) })
      setPlan(data.plan)
      if (data.plan.status === 'manual_required') toast(tf('clients.manualRequired', {}) + ': ' + (data.plan.reason || ''), 'error')
    } catch (e) { toast(e instanceof Error ? e.message : t('clients.planFailed'), 'error') }
    finally { setBusy(false) }
  }
  const apply = async () => {
    if (!available || !plan || plan.status !== 'planned') return
    setBusy(true)
    try { const data = await silencerFetch<{ change: Change }>('/api/clients/apply', { method: 'POST', body: JSON.stringify({ plan }) }); setChange(data.change); toast(t('clients.applied'), 'success') }
    catch (e) { toast(e instanceof Error ? e.message : t('clients.applyFailed'), 'error') }
    finally { setBusy(false) }
  }
  const rollback = async () => {
    if (!available || !change) return
    setBusy(true)
    try { await silencerFetch('/api/clients/rollback', { method: 'POST', body: JSON.stringify({ change }) }); toast(t('clients.restored'), 'success'); setChange(null); setPlan(null); await discover() }
    catch (e) { toast(e instanceof Error ? e.message : t('clients.restoreFailed'), 'error') }
    finally { setBusy(false) }
  }
  const rollbackHistory = async (entry: Change) => {
    if (!available) return toast(t('clients.hostAgentUnavailable'), 'error')
    setBusy(true)
    try { await silencerFetch('/api/clients/rollback', { method: 'POST', body: JSON.stringify({ change: entry }) }); toast(t('clients.historyRestored'), 'success'); await discover() }
    catch (e) { toast(e instanceof Error ? e.message : t('clients.restoreFailed'), 'error') }
    finally { setBusy(false) }
  }

  return <div className="space-y-5">
    <Card className="border-primary/20 bg-primary/[0.04]">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center gap-2 text-base"><Plus className="h-4 w-4" />{t('clients.createTitle')}</CardTitle>
        <CardDescription>{t('clients.createDesc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="grid gap-2 md:grid-cols-[1fr_1fr_150px_auto]">
          <Input value={profileName} onChange={(e) => setProfileName(e.target.value)} placeholder={t('clients.namePh')} />
          <select className="h-9 rounded-md border bg-background px-2 text-sm" value={profileUpstream} onChange={(e) => setProfileUpstream(e.target.value)}>
            <option value="">{t('clients.chooseUpstream')}</option>
            {upstreams.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
          <select className="h-9 rounded-md border bg-background px-2 text-sm" value={profileProtocol} onChange={(e) => setProfileProtocol(e.target.value as ClientProfile['protocol'])}>
            <option value="openai">OpenAI</option><option value="anthropic">Anthropic</option><option value="gemini">Gemini</option><option value="custom">{t('clients.customProtocol')}</option>
          </select>
          <Button onClick={() => void addProfile()} disabled={busy || !upstreams.length}><Plus className="mr-1 h-4 w-4" />{t('clients.create')}</Button>
        </div>
        {profiles.length === 0 ? <p className="text-xs text-muted-foreground">{t('clients.emptyProfiles')}</p> : <div className="grid gap-2 md:grid-cols-2">
          {profiles.map((profile) => {
            const domainUrl = clientRoute(profile.upstream, profile.protocol, window.location.origin)
            const localUrl = clientRoute(profile.upstream, profile.protocol, localOrigin)
            const CopyIcon = copiedProfile === `${profile.name}:all` ? Check : Copy
            return <div key={profile.name} className="rounded-lg border bg-background p-3">
              <div className="flex items-center gap-2"><span className="font-medium text-sm">{profile.name}</span><Badge variant="outline" className="text-[10px]">{profile.protocol}</Badge><span className="text-xs text-muted-foreground">→ {profile.upstream}</span><Button size="sm" variant="ghost" className="ml-auto h-7 px-2 text-xs" onClick={() => void saveProfiles(profiles.filter((item) => item.name !== profile.name))} disabled={busy}><Trash2 className="h-3.5 w-3.5" /></Button></div>
              <div className="mt-2 space-y-1.5 text-xs"><div><span className="mr-2 text-muted-foreground">{t('clients.domain')}</span><code className="rounded bg-muted px-2 py-1" title={domainUrl}>{domainUrl}</code></div><div><span className="mr-2 text-muted-foreground">{t('clients.local')}</span><code className="rounded bg-muted px-2 py-1" title={localUrl}>{localUrl}</code></div></div>
              <Button size="sm" variant="outline" className="mt-3 h-7 text-xs" onClick={() => void copyProfile(profile)}><CopyIcon className="mr-1 h-3.5 w-3.5" />{copiedProfile === `${profile.name}:all` ? t('clients.copied') : t('clients.copyUrls')}</Button>
            </div>
          })}
        </div>}
      </CardContent>
    </Card>
    <Card className="border-primary/20 bg-primary/[0.04]">
      <CardContent className="flex items-start gap-3 p-4">
        <HelpCircle className="mt-0.5 h-4 w-4 shrink-0 text-primary" />
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold">{t('clients.concepts.title')}</div>
          <TooltipProvider delayDuration={200}>
            <div className="mt-2 grid gap-2 md:grid-cols-2">
              {(['upstream', 'client'] as const).map((key) => (
                <Tooltip key={key}>
                  <TooltipTrigger asChild>
                    <button type="button" className="flex items-center gap-1 text-left text-xs font-medium text-foreground underline decoration-dotted underline-offset-4">
                      {t(`clients.concepts.${key}`)} <HelpCircle className="h-3 w-3 text-muted-foreground" />
                    </button>
                  </TooltipTrigger>
                  <TooltipContent side="bottom" className="max-w-[260px] text-xs leading-relaxed">
                    {t(`clients.concepts.${key}Desc`)}
                  </TooltipContent>
                </Tooltip>
              ))}
            </div>
          </TooltipProvider>
        </div>
      </CardContent>
    </Card>
    <Card><CardHeader className="flex-col items-start justify-between gap-3 space-y-0 sm:flex-row"><div><CardTitle className="flex items-center gap-2"><ShieldCheck className="h-5 w-5" />{t('settings.pageTitle.clients')}</CardTitle><CardDescription>{t('clients.pageDesc')}</CardDescription></div><Button className="shrink-0" variant="outline" size="sm" onClick={() => void discover()} disabled={loading}><RefreshCw className="mr-1.5 h-4 w-4" />{t('clients.rescan')}</Button></CardHeader>
      <CardContent className="space-y-4"><div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground"><Terminal className="h-4 w-4" />{t('clients.scope')}<Badge variant="secondary">{scope || 'gateway'}</Badge><TooltipProvider delayDuration={200}><Tooltip><TooltipTrigger asChild><HelpCircle className="h-3.5 w-3.5 cursor-help text-muted-foreground/60" /></TooltipTrigger><TooltipContent className="max-w-[280px] text-xs">{t('clients.scopeHint')}</TooltipContent></Tooltip></TooltipProvider></div>
        {!available && <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm text-amber-900 dark:text-amber-200"><div className="font-medium">{t('clients.hostAgentUnavailable')}</div><div className="mt-1 text-xs">{capability || t('clients.gatewayOnly')}</div></div>}
        {clients.length === 0 && !loading && <div className="rounded-lg border border-dashed p-5 text-sm text-muted-foreground"><div>{t('clients.empty')}</div><div className="mt-1 text-xs">{t('clients.emptyHint')}</div></div>}
        <div className="grid gap-3 md:grid-cols-3">{clients.map((item) => <button type="button" key={item.client} onClick={() => { setSelected(item); setConfigPath(item.config_paths?.[0] || '') }} className={`rounded-lg border p-3 text-left transition ${selected?.client === item.client ? 'border-primary bg-primary/5' : 'hover:border-primary/50'}`}><div className="flex items-center justify-between"><span className="font-medium capitalize">{item.client}</span>{item.version && <Badge variant="outline" className="text-[10px]">{item.version}</Badge>}</div><p className="mt-2 text-xs text-muted-foreground">{tf('clients.configFiles', { n: item.config_paths?.length || 0 })} · {item.user || t('clients.currentUser')}</p></button>)}</div>
      </CardContent></Card>
    {selected && <Card><CardHeader><CardTitle className="flex items-center gap-2 text-base">{tf('clients.wizardTitle', { client: selected.client })}<TooltipProvider delayDuration={200}><Tooltip><TooltipTrigger asChild><HelpCircle className="h-3.5 w-3.5 cursor-help text-muted-foreground/60" /></TooltipTrigger><TooltipContent className="max-w-[300px] text-xs">{t('clients.wizardHint')}</TooltipContent></Tooltip></TooltipProvider></CardTitle><CardDescription>{t('clients.wizardDesc')}</CardDescription></CardHeader><CardContent className="space-y-3"><label className="block text-xs font-medium">{t('clients.chooseFile')}<select className="mt-1 h-9 w-full rounded-md border bg-background px-2 text-xs" value={configPath} onChange={(e) => setConfigPath(e.target.value)}><option value="">{t('clients.chooseFile')}</option>{(selected.config_paths || []).map((p) => <option key={p} value={p}>{p}</option>)}</select></label><label className="block text-xs font-medium">{t('clients.routeUrl')}<Input className="mt-1 font-mono text-xs" value={proxyUrl} onChange={(e) => { proxyUrlEdited.current = true; setProxyUrl(e.target.value) }} /></label><div className="flex flex-wrap gap-2"><Button className="w-full sm:w-auto" onClick={() => void makePlan()} disabled={!available || busy || !configPath}>{t('clients.preview')}</Button>{change && <Button className="w-full sm:w-auto" variant="outline" onClick={() => void rollback()} disabled={!available || busy}><RotateCcw className="mr-1.5 h-4 w-4" />{t('clients.rollback')}</Button>}</div>{plan && <div className="rounded-lg border bg-muted/30 p-3 text-xs"><div className="flex items-center gap-2 font-medium">{plan.status === 'planned' ? <CheckCircle2 className="h-4 w-4 text-green-600" /> : <AlertTriangle className="h-4 w-4 text-amber-600" />}{t('clients.status')}{plan.status}</div>{plan.reason && <p className="mt-2 text-muted-foreground">{plan.reason}</p>}{plan.changes?.length ? <pre className="mt-2 max-h-48 overflow-auto rounded bg-background p-2 text-[11px]">{JSON.stringify(plan.changes, null, 2)}</pre> : null}{plan.status === 'planned' && <Button className="mt-3" size="sm" onClick={() => void apply()} disabled={!available || busy}>{t('clients.apply')}</Button>}</div>}</CardContent></Card>}
    {history.length > 0 && <Card><CardHeader><CardTitle className="flex items-center gap-2 text-base">{t('clients.historyTitle')}<TooltipProvider delayDuration={200}><Tooltip><TooltipTrigger asChild><HelpCircle className="h-3.5 w-3.5 cursor-help text-muted-foreground/60" /></TooltipTrigger><TooltipContent className="max-w-[300px] text-xs">{t('clients.historyHint')}</TooltipContent></Tooltip></TooltipProvider></CardTitle><CardDescription>{t('clients.historyDesc')}</CardDescription></CardHeader><CardContent className="space-y-2">{history.slice(0, 10).map((entry, index) => <div key={`${entry.backup}-${index}`} className="flex items-center justify-between gap-3 rounded border px-3 py-2 text-xs"><span className="min-w-0 truncate font-mono">{entry.path}</span><Button size="sm" variant="outline" className="shrink-0" disabled={!available || busy} onClick={() => void rollbackHistory(entry)}><RotateCcw className="mr-1 h-3.5 w-3.5" />{t('clients.rollbackAction')}</Button></div>)}</CardContent></Card>}
    <SettingsPage embeddedTab="clients" />
  </div>
}
