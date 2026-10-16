import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Trash2 } from 'lucide-react'
import { patchConfig } from '@/api/settings'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { useI18n } from '@/lib/i18n'
import { toast } from '@/lib/toast'
import type { SilencerConfig } from '@/types/api'

type Rule = NonNullable<SilencerConfig['log_noise_allowlist']>[number]

export function LogNoiseAllowlistCard({ rules = [] }: { rules?: Rule[] }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [method, setMethod] = useState('GET')
  const [path, setPath] = useState('')
  const [saving, setSaving] = useState(false)

  const change = async (op: 'list_add' | 'list_remove', rule: Rule) => {
    if (saving) return
    setSaving(true)
    try {
      const result = await patchConfig({ key: 'log_noise_allowlist', op, value: [rule] })
      if (!result.ok) throw new Error(result.error || t('settings.toast.saveFailed'))
      queryClient.setQueryData(['config'], result.config)
      for (const key of ['logs', 'recentLogs', 'logDetail', 'todayStats', 'statsModels', 'statsLeaderboard']) {
        void queryClient.invalidateQueries({ queryKey: [key] })
      }
      if (op === 'list_add') setPath('')
      toast(t('settings.toast.saved'))
    } catch (error) {
      toast(error instanceof Error ? error.message : t('settings.toast.saveFailed'), 'error')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-semibold">{t('settings.logs.noiseTitle')}</CardTitle>
        <CardDescription>{t('settings.logs.noiseDescription')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="space-y-2">
          {rules.map((rule) => (
            <div key={`${rule.method}:${rule.path}`} className="flex items-center gap-2 rounded border px-3 py-2 text-xs">
              <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 font-mono">{rule.method}</span>
              <code className="min-w-0 flex-1 break-all">{rule.path}</code>
              <span className="text-muted-foreground">404</span>
              <Button variant="ghost" size="sm" className="h-7 w-7 p-0" disabled={saving} aria-label={t('settings.logs.noiseRemove')} onClick={() => void change('list_remove', rule)}>
                <Trash2 className="h-3.5 w-3.5" />
              </Button>
            </div>
          ))}
          {!rules.length && <p className="text-xs text-muted-foreground">{t('settings.logs.noiseEmpty')}</p>}
        </div>
        <form className="flex flex-wrap items-center gap-2" onSubmit={(event) => {
          event.preventDefault()
          void change('list_add', { method, path: path.trim(), status: 404 })
        }}>
          <select className="h-8 rounded border bg-background px-2 text-xs" aria-label={t('settings.logs.noiseMethod')} value={method} onChange={(event) => setMethod(event.target.value)} disabled={saving}>
            {['GET', 'POST', 'HEAD', 'OPTIONS', 'PUT', 'PATCH', 'DELETE'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <Input className="h-8 min-w-48 flex-1 font-mono text-xs" aria-label={t('settings.logs.noisePath')} placeholder="/r/sub2api/api/tags" value={path} onChange={(event) => setPath(event.target.value)} disabled={saving} required />
          <span className="text-xs text-muted-foreground">404</span>
          <Button type="submit" size="sm" className="h-8" disabled={saving || !path.trim()}>{t('settings.logs.noiseAdd')}</Button>
        </form>
        <p className="text-[11px] text-muted-foreground">{t('settings.logs.noiseHint')}</p>
      </CardContent>
    </Card>
  )
}
