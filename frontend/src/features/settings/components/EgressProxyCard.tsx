import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { useI18n } from '@/lib/i18n'
import type { EgressProxy } from '@/types/api'

interface EgressProxyCardProps {
  value?: EgressProxy
  onEnabledChange: (enabled: boolean) => void
  onUrlCommit: (url: string) => void
}

/** Egress settings are isolated here; persistence and save ordering remain owned by SettingsPage. */
export function EgressProxyCard({ value, onEnabledChange, onUrlCommit }: EgressProxyCardProps) {
  const { t } = useI18n()
  const enabled = !!value?.enabled
  const url = value?.url ?? ''

  return (
    <Card id="egress-proxy" className="border bg-card scroll-mt-6">
      <CardHeader>
        <CardTitle className="text-sm font-semibold">{t('settings.advanced.egress')}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <label className="flex cursor-pointer select-none items-center gap-2 text-xs text-muted-foreground transition-colors hover:text-foreground">
          <Switch checked={enabled} onCheckedChange={onEnabledChange} />
          {t('settings.advanced.egressHint')}
        </label>
        {/* Keep the input uncontrolled so typing does not save or interrupt the edit. */}
        <Input
          key={url}
          className="h-8 max-w-md font-mono text-xs"
          defaultValue={url}
          placeholder={t('settings.advanced.egressPh')}
          onBlur={(event) => {
            if (event.target.value !== url) onUrlCommit(event.target.value)
          }}
        />
        <p className="text-[11px] text-muted-foreground">{t('settings.advanced.egressTip')}</p>
      </CardContent>
    </Card>
  )
}
