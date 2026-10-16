import { useId } from 'react'
import { Eye, EyeOff } from 'lucide-react'
import { Switch } from '@/components/ui/switch'
import { usePrivacyStore } from '@/stores/privacyStore'
import { useI18n } from '@/lib/i18n'

export function PlaintextSwitch() {
  const id = useId()
  const { t } = useI18n()
  const { showPlaintext, setShowPlaintext } = usePrivacyStore()
  const Icon = showPlaintext ? Eye : EyeOff
  return (
    <label htmlFor={id} className="flex shrink-0 cursor-pointer items-center gap-1.5 text-xs text-muted-foreground" title={t('privacy.displayHint')}>
      <Icon className="h-3.5 w-3.5" aria-hidden="true" />
      <span>{t('stats.showPlain')}</span>
      <Switch id={id} checked={showPlaintext} onCheckedChange={setShowPlaintext} />
    </label>
  )
}
