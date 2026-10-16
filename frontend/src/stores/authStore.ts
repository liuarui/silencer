/**
 * 认证状态。浏览器使用 HttpOnly session cookie；token 仅用于本地 API 调用。
 */
import { create } from 'zustand'
import { resetPrivacy } from './privacyStore'

interface AuthState {
  token: string
  authChecked: boolean
  authenticated: boolean
  authInitialized: boolean
  username: string
  engineReady: boolean
  engineError: string | null
  setToken: (token: string) => void
  setAuth: (value: { authenticated: boolean; initialized: boolean; username?: string }) => void
  setEngineReady: (ready: boolean) => void
  setEngineError: (err: string | null) => void
}

export const useAuthStore = create<AuthState>((set) => ({
  token: '',
  authChecked: false,
  authenticated: false,
  authInitialized: false,
  username: '',
  engineReady: false,
  engineError: null,
  setToken: (token) => set({ token }),
  setAuth: (value) => {
    if (!value.authenticated) resetPrivacy()
    set({ authChecked: true, authenticated: value.authenticated, authInitialized: value.initialized, username: value.username || '' })
  },
  setEngineReady: (ready) => set({ engineReady: ready }),
  setEngineError: (err) => set({ engineError: err }),
}))
