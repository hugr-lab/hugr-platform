import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import type { Api } from './api'
import type { Session } from './auth'
import type { Config, Rights } from './types'

export interface App {
  config: Config
  session: Session
  api: Api
  rights: Rights
}

export const AppContext = createContext<App | null>(null)

export function useApp(): App {
  const app = useContext(AppContext)
  if (!app) throw new Error('useApp outside the console')
  return app
}

export interface Loaded<T> {
  data?: T
  error?: unknown
  loading: boolean
  at?: Date
  reload: () => void
}

/** reads when the page opens and when the person asks again; nothing polls (tresor's rule, spec 003) */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[]): Loaded<T> {
  const [state, setState] = useState<{ data?: T; error?: unknown; loading: boolean; at?: Date }>({ loading: true })
  const seq = useRef(0)
  const run = useCallback(() => {
    const mine = ++seq.current
    setState((s) => ({ ...s, loading: true }))
    load().then(
      (data) => mine === seq.current && setState({ data, loading: false, at: new Date() }),
      (error) => mine === seq.current && setState({ error, loading: false, at: new Date() }),
    )
  }, deps)
  useEffect(run, [run])
  return { ...state, reload: run }
}

/** the light or dark theme: the person's choice in this browser, else the system's */
export function useTheme(): [string, () => void] {
  const [theme, setTheme] = useState<string>(() => {
    try {
      const kept = localStorage.getItem('hugr.theme')
      if (kept === 'light' || kept === 'dark') return kept
    } catch {
      /* storage refused */
    }
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  })
  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])
  const toggle = () =>
    setTheme((t) => {
      const next = t === 'dark' ? 'light' : 'dark'
      try {
        localStorage.setItem('hugr.theme', next)
      } catch {
        /* storage refused */
      }
      return next
    })
  return [theme, toggle]
}
