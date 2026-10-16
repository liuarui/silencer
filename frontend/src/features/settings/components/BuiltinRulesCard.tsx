import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { cn } from '@/lib/utils'
import { useI18n } from '@/lib/i18n'

const BUILTIN_RULE_GROUPS: { key: string; labelKey: string; rules: string[] }[] = [
  {
    key: 'core',
    labelKey: 'settings.words.groupCore',
    rules: ['PHONE', 'EMAIL', 'IDCARD', 'LANDLINE', 'CARD', 'API_KEY', 'CONNSTR'],
  },
  {
    key: 'credentials',
    labelKey: 'settings.words.groupCredentials',
    rules: ['ACCESS_KEY', 'PRIVATE_KEY', 'SECRET', 'TOKEN', 'JWT', 'COOKIE_SESSION', 'BASIC_AUTH', 'URL_QUERY_SECRET'],
  },
  {
    key: 'network',
    labelKey: 'settings.words.groupNetwork',
    rules: ['IP_PRIVATE', 'IP_INTERNAL', 'IP_PUBLIC', 'IPV6_PRIVATE', 'MAC'],
  },
  {
    key: 'entities',
    labelKey: 'settings.words.groupEntities',
    rules: ['PLATE', 'USCC', 'HKID', 'IBAN'],
  },
]
const KNOWN_BUILTIN_RULES = new Set(BUILTIN_RULE_GROUPS.flatMap((group) => group.rules))

interface BuiltinRulesCardProps {
  rules: Record<string, boolean>
  ruleMeta: Record<string, string>
  onRuleChange: (rule: string, enabled: boolean) => void
  onSetAll: (enabled: boolean, ruleNames: string[]) => void
  disabled?: boolean
}

/** UI-only built-in rule controls; saving and request ordering stay in SettingsPage. */
export function BuiltinRulesCard({ rules, ruleMeta, onRuleChange, onSetAll, disabled = false }: BuiltinRulesCardProps) {
  const { t } = useI18n()
  const ruleNames = [...new Set([...KNOWN_BUILTIN_RULES, ...Object.keys(rules)])]
  const ungroupedRules = ruleNames.filter((rule) => !KNOWN_BUILTIN_RULES.has(rule)).sort()
  const groups = ungroupedRules.length > 0
    ? [...BUILTIN_RULE_GROUPS, { key: 'other', labelKey: 'settings.words.groupOther', rules: ungroupedRules }]
    : BUILTIN_RULE_GROUPS

  return (
    <Card className="border bg-card">
      <CardHeader className="flex-row items-center justify-between space-y-0">
        <div>
          <CardTitle className="text-sm font-semibold">{t('settings.words.builtinRules')}</CardTitle>
          <p className="mt-0.5 text-[11px] text-muted-foreground">{t('settings.words.builtinRulesHint')}</p>
        </div>
        <div className="flex gap-1.5">
          <Button size="sm" variant="outline" className="h-6 text-[11px]" disabled={disabled} onClick={() => onSetAll(true, ruleNames)}>
            {t('settings.words.allOn')}
          </Button>
          <Button size="sm" variant="outline" className="h-6 text-[11px]" disabled={disabled} onClick={() => onSetAll(false, ruleNames)}>
            {t('settings.words.allOff')}
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {groups.map((group) => {
          const groupRules = group.rules
          if (groupRules.length === 0) return null
          const onCount = groupRules.filter((rule) => rules[rule]).length
          return (
            <div key={group.key} className="space-y-2">
              <div className="flex items-center gap-2">
                <span className="text-xs font-semibold text-foreground/80">{t(group.labelKey)}</span>
                <span className="rounded-full bg-muted px-1.5 py-0.2 font-mono text-[10px] text-muted-foreground">
                  {onCount}/{groupRules.length}
                </span>
              </div>
              <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 md:grid-cols-3">
                {groupRules.map((rule) => {
                  const enabled = Boolean(rules[rule])
                  const known = KNOWN_BUILTIN_RULES.has(rule)
                  const name = known ? t(`settings.words.rule.${rule}.name`) : rule
                  const description = known ? t(`settings.words.rule.${rule}.desc`) : ruleMeta[rule]
                  return (
                    <label
                      key={rule}
                      title={description || rule}
                      className={cn(
                        'flex cursor-pointer items-start justify-between rounded-lg border px-3 py-2 transition-colors',
                        enabled
                          ? 'border-primary/25 bg-primary/5 hover:border-primary/40'
                          : 'border-border bg-muted/30 opacity-70 hover:opacity-100',
                      )}
                    >
                      <div className="min-w-0 pr-2">
                        <span className={cn('text-xs font-medium', enabled && 'text-primary')}>{name}</span>
                        <div className="mt-0.5 font-mono text-[10px] text-muted-foreground">{rule}</div>
                        {description && (
                          <div className="mt-1 text-[11px] leading-relaxed text-muted-foreground" title={description}>{description}</div>
                        )}
                      </div>
                      <Switch checked={enabled} disabled={disabled} onCheckedChange={(next) => onRuleChange(rule, next)} className="scale-75 shrink-0" />
                    </label>
                  )
                })}
              </div>
            </div>
          )
        })}
      </CardContent>
    </Card>
  )
}
