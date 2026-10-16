import { HelpCircle } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { useI18n } from '@/lib/i18n'
import { toast } from '@/lib/toast'

interface DataRetentionCardProps {
  value?: number
  onSave: (days: number) => void
}

/** Retention validation stays next to its input; config persistence remains in the page. */
export function DataRetentionCard({ value, onSave }: DataRetentionCardProps) {
  const { t } = useI18n()
  const current = value ?? 3

  return (
    <Card className="border bg-card">
      <CardHeader>
        <CardTitle className="text-sm font-semibold">{t('settings.advanced.dataRetention')}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-wrap items-center gap-3">
        <Label className="flex items-center gap-1 text-xs">
          {t('settings.advanced.retentionDays')}
          <TooltipProvider delayDuration={200}>
            <Tooltip>
              <TooltipTrigger asChild><HelpCircle className="h-3.5 w-3.5 cursor-help text-muted-foreground/60" /></TooltipTrigger>
              <TooltipContent className="max-w-[300px] text-xs">{t('settings.advanced.retentionTooltip')}</TooltipContent>
            </Tooltip>
          </TooltipProvider>
        </Label>
        <Input
          type="number"
          min={0}
          step={1}
          className="h-8 w-24 text-xs"
          key={`retention-${current}`}
          defaultValue={current}
          onBlur={(event) => {
            const raw = event.target.value.trim()
            const parsed = Number(raw)
            if (raw === '' || !Number.isFinite(parsed) || parsed < 0 || !Number.isInteger(parsed) || parsed > 36500) {
              event.target.value = String(current)
              toast(t('settings.toast.retentionInvalid'), 'error')
              return
            }
            const next = Math.floor(parsed)
            event.target.value = String(next)
            if (next !== current) onSave(next)
          }}
        />
        <span className="text-[11px] text-muted-foreground">{t('settings.advanced.retentionHint')}</span>
      </CardContent>
    </Card>
  )
}
