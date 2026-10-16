/** Web/Docker 版本信息与更新提示。 */
import { Terminal, Shield } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { useI18n } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { Logo } from '@/components/brand/Logo'

export function AboutUpdateCard({ version, dataRoot, running }: { version?: string; dataRoot?: string; running?: boolean }) {
  const { t } = useI18n()
  return (
    <Card className="border bg-card">
      <CardHeader className="flex-row items-center gap-3 space-y-0">
        <Logo className="h-10 w-10 shrink-0" />
        <div className="min-w-0">
          <CardTitle className="flex items-center gap-2 text-base">Silencer <Badge variant="outline" className="font-mono text-xs">v{version || '—'}</Badge></CardTitle>
          <p className="mt-0.5 text-[11px] text-muted-foreground">{t('about.subtitle')}</p>
          {dataRoot && <p className="mt-0.5 truncate text-[11px] text-muted-foreground">{dataRoot}</p>}
        </div>
        {running != null && (
          <Badge variant="outline" className={cn('ml-auto shrink-0 text-xs', running ? 'border-emerald-500/30 text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground')}>
            <Shield className="mr-1 h-3 w-3" />
            {running ? t('common.running') : t('common.stopped')}
          </Badge>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center justify-between text-xs">
          <span className="text-muted-foreground">{t('about.currentVersion')}</span>
          <code className="font-mono text-foreground">v{version || '—'}</code>
        </div>
        <div className="rounded-lg border bg-muted/30 p-3 text-[11px] text-muted-foreground">
          <div className="mb-2 flex items-center gap-1.5 text-xs font-semibold text-foreground">
            <Terminal className="h-3.5 w-3.5 text-primary" />
            <span>{t('about.dockerUpdateTitle')}</span>
          </div>
          <p>{t('about.dockerManualUpdate')}</p>
          <code className="mt-1 block select-all font-mono text-foreground">docker compose pull &amp;&amp; docker compose up -d</code>
        </div>
      </CardContent>
    </Card>
  )
}
