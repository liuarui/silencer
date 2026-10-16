import { Activity, FileText, Info, Loader2, Play, X } from 'lucide-react'
import type { AuditJob } from '@/types/api'
import { useI18n } from '@/lib/i18n'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

type UpstreamOption = { name: string; port: number }

interface UpstreamProbePanelProps {
  upstreams: UpstreamOption[]
  upstream: string
  onUpstreamChange: (value: string) => void
  model: string
  onModelChange: (value: string) => void
  profile: string
  onProfileChange: (value: string) => void
  job?: AuditJob
  available: boolean
  running: boolean
  done: number
  total: number
  progress: number
  reportLoading: boolean
  onStart: () => void
  onCancel: () => void
  onReport: () => void
}

/** A guided, cost-aware flow for testing one configured upstream. */
export function UpstreamProbePanel({
  upstreams,
  upstream,
  onUpstreamChange,
  model,
  onModelChange,
  profile,
  onProfileChange,
  job,
  available,
  running,
  done,
  total,
  progress,
  reportLoading,
  onStart,
  onCancel,
  onReport,
}: UpstreamProbePanelProps) {
  const { t } = useI18n()
  const ready = Boolean(upstream && model.trim())

  return (
    <Card className="border bg-card shadow-[var(--shadow-card)]">
      <CardHeader className="flex-row items-start gap-3 space-y-0">
        <span className="mt-0.5 rounded-lg bg-primary/10 p-2 text-primary">
          <Activity className="h-4 w-4" />
        </span>
        <div className="min-w-0">
          <CardTitle className="text-sm font-semibold">{t('audit.probe.title')}</CardTitle>
          <p className="mt-1 max-w-2xl text-xs leading-relaxed text-muted-foreground">
            {t('audit.probe.description')}
          </p>
        </div>
        <Badge variant={running ? 'default' : 'outline'} className="ml-auto shrink-0 text-xs">
          {running ? t('settings.advanced.auditRunning') : (job?.phase || t('common.idle'))}
        </Badge>
      </CardHeader>

      <CardContent className="space-y-5">
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="audit-upstream" className="text-xs font-medium">
              {t('audit.probe.upstream')}
            </Label>
            <Select value={upstream} onValueChange={onUpstreamChange} disabled={running || upstreams.length === 0}>
              <SelectTrigger id="audit-upstream" className="h-9 text-sm">
                <SelectValue placeholder={t('settings.advanced.chooseUpstream')} />
              </SelectTrigger>
              <SelectContent>
                {upstreams.map((item) => (
                  <SelectItem key={item.name} value={item.name}>
                    {item.name} · :{item.port}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {upstreams.length === 0 && (
              <p className="text-xs text-amber-600 dark:text-amber-500">{t('audit.probe.noUpstreams')}</p>
            )}
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="audit-model" className="text-xs font-medium">
              {t('audit.probe.model')}
            </Label>
            <Input
              id="audit-model"
              className="h-9 text-sm"
              value={model}
              onChange={(event) => onModelChange(event.target.value)}
              placeholder={t('audit.probe.modelPlaceholder')}
              disabled={running}
            />
            <p className="text-xs text-muted-foreground">{t('audit.probe.modelHint')}</p>
          </div>
        </div>

        <details className="group rounded-lg border bg-muted/20 px-3 py-2.5">
          <summary className="cursor-pointer list-none text-xs font-medium text-foreground marker:hidden">
            <span className="inline-flex items-center gap-2">
              <span className="text-muted-foreground">{t('audit.probe.coverage')}</span>
              <span>{t(`settings.advanced.profile.${profile}`)}</span>
              <span className="text-muted-foreground group-open:hidden">· {t('audit.probe.defaultCoverage')}</span>
            </span>
          </summary>
          <div className="mt-3 grid gap-2 sm:grid-cols-[minmax(0,220px)_1fr] sm:items-center">
            <Select value={profile} onValueChange={onProfileChange} disabled={running}>
              <SelectTrigger className="h-8 text-xs">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="general">{t('settings.advanced.profile.general')}</SelectItem>
                <SelectItem value="web3">{t('settings.advanced.profile.web3')}</SelectItem>
                <SelectItem value="full">{t('settings.advanced.profile.full')}</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs leading-relaxed text-muted-foreground">{t('settings.advanced.profileTooltip')}</p>
          </div>
        </details>

        <div className="flex gap-2 rounded-lg border border-amber-500/25 bg-amber-500/5 p-3">
          <Info className="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-amber-500" />
          <div className="space-y-1 text-xs leading-relaxed">
            <p className="font-medium text-foreground">{t('audit.probe.cost')}</p>
            <p className="text-muted-foreground">{t('audit.probe.scope')}</p>
          </div>
        </div>

        {running && (
          <div className="rounded-lg border border-primary/30 bg-primary/5 p-3" aria-live="polite">
            <div className="flex items-center gap-2">
              <Loader2 className="h-3.5 w-3.5 animate-spin text-primary" />
              <span className="text-xs font-medium">{job?.phase || t('settings.advanced.auditRunning')}</span>
              <Badge variant="outline" className="ml-auto shrink-0 font-mono text-[11px]">{done}/{total}</Badge>
            </div>
            <div
              className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-muted"
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={progress}
              aria-label={t('audit.probe.progress')}
            >
              <div className="h-full rounded-full bg-gradient-to-r from-primary to-cyan-500 transition-[width] duration-500" style={{ width: `${progress}%` }} />
            </div>
          </div>
        )}

        {!running && job?.phase && (
          <p className={`text-xs ${job.error ? 'text-destructive' : 'text-muted-foreground'}`}>
            {t('settings.advanced.lastTask')} {job.phase}{job.error ? ` · ${job.error.slice(0, 120)}` : ''}
          </p>
        )}

        {!available && (
          <p className="text-xs text-amber-600 dark:text-amber-500">{t('audit.probe.unavailable')}</p>
        )}

        <div className="flex flex-wrap items-center gap-2 border-t pt-4">
          <Button size="sm" onClick={onStart} disabled={!ready || running || !available} className="gap-1.5">
            <Play className="h-4 w-4" />{t('audit.probe.start')}
          </Button>
          {running && (
            <Button size="sm" variant="outline" onClick={onCancel} className="gap-1.5">
              <X className="h-4 w-4" />{t('common.cancel')}
            </Button>
          )}
          <Button size="sm" variant="ghost" onClick={onReport} loading={reportLoading} disabled={!available || reportLoading} className="gap-1.5">
            {!reportLoading && <FileText className="h-4 w-4" />}{t('settings.advanced.viewReport')}
          </Button>
          {!ready && !running && upstreams.length > 0 && (
            <span className="text-xs text-muted-foreground">{t('audit.probe.completeFields')}</span>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
