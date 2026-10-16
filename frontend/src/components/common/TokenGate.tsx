/** Browser login and first-run setup for the Silencer Web console. */
import { useEffect, useState, type FormEvent } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Logo } from '@/components/brand/Logo'
import { useI18n } from '@/lib/i18n'
import { silencerFetch, SilencerApiError } from '@/lib/silencer-fetch'
import { useAuthStore } from '@/stores/authStore'

export function TokenGate() {
  const { t } = useI18n()
  const initialized = useAuthStore((s) => s.authInitialized)
  const setAuth = useAuthStore((s) => s.setAuth)
  const [username, setUsername] = useState('admin')
  const [password, setPassword] = useState('')
  const [initCode, setInitCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (useAuthStore.getState().authChecked) return
    silencerFetch<{ initialized: boolean; authenticated: boolean; username?: string }>(
      '/api/auth/status', { noToken: true },
    ).then((status) => setAuth(status)).catch(() => setAuth({ initialized: false, authenticated: false }))
  }, [setAuth])

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      const endpoint = initialized ? '/api/auth/login' : '/api/auth/setup'
      const body = initialized
        ? { username: username.trim(), password }
        : { username: username.trim(), password, init_code: initCode.trim() }
      const result = await silencerFetch<{ username?: string }>(endpoint, {
        method: 'POST', noToken: true, body: JSON.stringify(body),
      })
      setAuth({ initialized: true, authenticated: true, username: result.username || username.trim() })
    } catch (err: unknown) {
      if (err instanceof SilencerApiError && err.code === 'origin_rejected') {
        setError(t('tokenGate.originMismatch'))
      } else if (!initialized) {
        setError(t('tokenGate.setupInvalid'))
      } else {
        setError(t('tokenGate.invalid'))
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-6">
      <form onSubmit={submit} className="w-full max-w-sm space-y-4 rounded-lg border bg-card p-6 shadow-sm">
        <div className="flex items-center gap-2">
          <Logo className="h-6 w-6" />
          <span className="text-sm font-semibold">{t('tokenGate.title')}</span>
        </div>
        <p className="text-xs text-muted-foreground">
          {initialized ? t('tokenGate.loginDesc') : t('tokenGate.setupDesc')}
        </p>
        <Input type="text" autoFocus autoComplete="username" value={username}
          onChange={(e) => { setUsername(e.target.value); setError('') }} placeholder={t('tokenGate.username')} />
        {!initialized && (
          <Input type="text" autoComplete="one-time-code" value={initCode}
            onChange={(e) => { setInitCode(e.target.value); setError('') }} placeholder={t('tokenGate.initCode')} />
        )}
        <Input type="password" autoComplete={initialized ? 'current-password' : 'new-password'} value={password}
          onChange={(e) => { setPassword(e.target.value); setError('') }} placeholder={t('tokenGate.password')} />
        {error && <p className="text-xs text-red-600 dark:text-red-400">{error}</p>}
        <Button type="submit" className="w-full" disabled={busy || !username.trim() || !password || (!initialized && !initCode.trim())}>
          {busy ? t('tokenGate.working') : initialized ? t('tokenGate.submit') : t('tokenGate.setup')}
        </Button>
      </form>
    </div>
  )
}
