import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import dayjs from 'dayjs'
import { Loader2 } from 'lucide-react'
import { getConfigBackups, restoreConfigBackup, type ConfigBackup } from '@/api/settings'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { toast } from '@/lib/toast'
import { useI18n } from '@/lib/i18n'
import { useVisibility } from '@/lib/useVisibility'
import { cn } from '@/lib/utils'

/** A data-directory configuration backup summary with guarded one-click rollback. */
export function ConfigBackupCard() {
  const { t, tf } = useI18n()
  const queryClient = useQueryClient()
  const { hidden } = useVisibility()
  const [pending, setPending] = useState<ConfigBackup | null>(null)
  const [restoring, setRestoring] = useState(false)
  const { data, isLoading } = useQuery({
    queryKey: ['configBackups'],
    queryFn: getConfigBackups,
    refetchOnMount: 'always',
    refetchInterval: hidden ? false : 120000,
  })
  const backups = data?.backups ?? []
  const current = data?.current ?? {}

  const doRestore = async () => {
    if (!pending) return
    setRestoring(true)
    try {
      const result = await restoreConfigBackup(pending.file)
      if (!result.ok) {
        toast(result.error || t('settings.backup.restoreFail2'), 'error')
        return
      }
      result.warnings?.forEach((warning) => toast(warning, 'error'))
      toast(result.proxy_restarted ? t('settings.toast.restarted') : t('settings.backup.restored'))
      setPending(null)
      queryClient.invalidateQueries({ queryKey: ['config'] })
      queryClient.invalidateQueries({ queryKey: ['configBackups'] })
      queryClient.invalidateQueries({ queryKey: ['proxyStatus'] })
    } catch (error) {
      toast(`${t('settings.backup.restoreFail2')}：${String(error)}`, 'error')
    } finally {
      setRestoring(false)
    }
  }

  return (
    <Card className="border bg-card">
      <CardHeader className="flex-row items-center justify-between space-y-0">
        <div>
          <CardTitle className="text-sm font-semibold">{t('settings.backup.title')}</CardTitle>
          <p className="text-[11px] text-muted-foreground">{t('settings.backup.desc')}</p>
          <p className="mt-1 text-xs text-muted-foreground">{t('settings.backup.desc2')}</p>
        </div>
        <span className="shrink-0 rounded-lg border bg-muted/40 px-2.5 py-1 text-[11px] text-muted-foreground">
          {t('settings.backup.currentLabel')} <b className="font-mono text-foreground">{current.upstreams ?? '—'}</b>
          {' · '}{t('settings.backup.wordLabel')} <b className="font-mono text-foreground">{current.words ?? '—'}</b>
        </span>
      </CardHeader>
      <CardContent>
        {isLoading && <p className="py-6 text-center text-xs text-muted-foreground">{t('settings.backup.loading')}</p>}
        {!isLoading && backups.length === 0 && (
          <p className="py-6 text-center text-xs text-muted-foreground">{t('settings.backup.empty')}</p>
        )}
        <div className="space-y-1.5">
          {backups.map((backup) => {
            const richer = (backup.upstreams ?? 0) > (current.upstreams ?? 0)
              || (backup.words ?? 0) > (current.words ?? 0)
            return (
              <div
                key={backup.file}
                className={cn(
                  'flex flex-wrap items-center gap-2 rounded-lg border px-3 py-2 text-xs',
                  richer ? 'border-amber-500/40 bg-amber-500/5' : 'bg-muted/20',
                )}
              >
                <span className="font-mono text-[11px] text-muted-foreground">
                  {backup.mtime ? dayjs(backup.mtime * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
                </span>
                <span className="text-muted-foreground">
                  {t('settings.backup.summaryClients')} <b className="font-mono text-foreground">{backup.upstreams}</b>
                  {' · '}{t('settings.backup.summaryCats')} <b className="font-mono text-foreground">{backup.cats}</b>
                  {' · '}{t('settings.backup.summaryWords')} <b className="font-mono text-foreground">{backup.words}</b>
                  {' · '}{t('settings.backup.summaryDomains')} <b className="font-mono text-foreground">{backup.domains}</b>
                </span>
                {richer && (
                  <Badge className="bg-amber-500/15 text-[10px] text-amber-600 hover:bg-amber-500/15 dark:text-amber-400">
                    {t('settings.backup.richer')}
                  </Badge>
                )}
                <Button size="sm" variant="outline" className="ml-auto h-7 px-2.5 text-[11px]" onClick={() => setPending(backup)}>
                  {t('settings.backup.restore')}
                </Button>
              </div>
            )
          })}
        </div>
      </CardContent>

      <Dialog open={pending != null} onOpenChange={(open) => !open && setPending(null)}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>{t('settings.backup.confirmTitle')}</DialogTitle>
            <DialogDescription>
              {tf('settings.backup.confirmDesc', {
                time: pending?.mtime ? dayjs(pending.mtime * 1000).format('YYYY-MM-DD HH:mm:ss') : '—',
                a: current.upstreams ?? 0,
                b: pending?.upstreams ?? 0,
                c: current.words ?? 0,
                d: pending?.words ?? 0,
              })}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button size="sm" variant="outline" onClick={() => setPending(null)} disabled={restoring}>
              {t('settings.confirm.cancel')}
            </Button>
            <Button size="sm" onClick={doRestore} disabled={restoring}>
              {restoring && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
              {t('settings.backup.confirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  )
}
