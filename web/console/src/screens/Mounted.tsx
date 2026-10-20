import { useEffect, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Header } from '../components/ui'
import { useApp } from '../lib/context'
import { contract, mountTokens, type Handle, type Module, type Which } from '../lib/mounts'

const titles: Record<Which, { title: string; lead: string; product: string }> = {
  tresor: { title: 'Secrets', product: "tresor-server's console",
    lead: "tresor-server's own console, shown here. It asks for a token meant for tresor-server alone; its requests go to tresor-server through this address." },
  kista: { title: 'Extensions', product: "kista's console",
    lead: "kista's own console, shown for the tenant the platform's repository names. The nodes install extensions from this repository only." },
}

type State = { kind: 'loading' } | { kind: 'mounted' } | { kind: 'incompatible'; theirs: number } | { kind: 'failed' } | { kind: 'ended' }

/** a mounted console: its module from the service, its contract checked, its routes under basePath */
export function Mounted({ which, basePath }: { which: Which; basePath: string }) {
  const { config, session } = useApp()
  const mount = config.mounts[which]
  const ref = useRef<HTMLDivElement>(null)
  const handle = useRef<Handle | null>(null)
  const [state, setState] = useState<State>({ kind: 'loading' })
  const navigate = useNavigate()
  const location = useLocation()
  const fromMounted = useRef<string | null>(null) // the path the mounted console navigated to itself
  const t = titles[which]

  useEffect(() => {
    if (!mount) return
    let cancelled = false
    const url = `${mount.base}/ui/mfe/${which}.js`
    import(/* @vite-ignore */ url).then(
      (m: Module) => {
        if (cancelled || !ref.current) return
        if (m.contract !== contract) {
          setState({ kind: 'incompatible', theirs: Number(m.contract) })
          return
        }
        const options = {
          apiBase: mount.base,
          getToken: mountTokens(config, session),
          audience: mount.audience,
          tenant: mount.tenant,
          basePath,
          theme: document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light',
          onNavigate: (path: string, how: { replace: boolean }) => {
            fromMounted.current = path
            navigate(path, { replace: how.replace })
          },
          onTitle: (title: string) => { document.title = `${title} - hugr console` },
          onUnauthorized: () => setState({ kind: 'ended' }),
        }
        const mountFn = which === 'tresor' ? m.mountTresor : m.mountKista
        if (!mountFn) {
          setState({ kind: 'incompatible', theirs: -1 })
          return
        }
        handle.current = mountFn(ref.current, options)
        setState({ kind: 'mounted' })
      },
      () => !cancelled && setState({ kind: 'failed' }),
    )
    return () => {
      cancelled = true
      const h = handle.current
      handle.current = null
      // a React host removes the element while it commits: the other root unmounts after (kista's note)
      queueMicrotask(() => h?.unmount())
    }
    // mounted once per page: the console routes inside itself
  }, [which])

  useEffect(() => {
    // every navigation but the mounted console's own (onNavigate): back and forward, the shell's own links into it
    const path = location.pathname + location.search
    if (path !== fromMounted.current) handle.current?.update({ path })
    fromMounted.current = null
  }, [location])

  useEffect(() => {
    // the shell's theme, as the person switches it
    const root = document.documentElement
    const observer = new MutationObserver(() =>
      handle.current?.update({ theme: root.dataset.theme === 'dark' ? 'dark' : 'light' }))
    observer.observe(root, { attributes: true, attributeFilter: ['data-theme'] })
    return () => observer.disconnect()
  }, [])

  if (!mount) return <Header title={t.title} lead="This platform names no such service." />
  return (
    <>
      <Header title={t.title} lead={t.lead} />
      {state.kind === 'loading' && <p role="status" className="text-ink-muted">Loading {t.product}</p>}
      {state.kind === 'failed' && (
        <p role="alert" className="rounded-md border border-line px-6 py-4 text-sm">{t.product} did not load: the service does not answer through the console.</p>
      )}
      {state.kind === 'incompatible' && (
        <div role="alert" className="flex max-w-[720px] flex-col gap-2 rounded-lg border border-line p-8">
          <h2 className="text-[22px] font-semibold leading-[30px]">{t.product} is {state.theirs > contract ? 'newer' : 'older'} than this one</h2>
          <p className="text-ink-muted">This console speaks contract {contract}; {state.theirs > 0 ? `it speaks ${state.theirs}` : 'it speaks none this console knows'}. {state.theirs > contract ? "Update the platform's operator, which serves this console." : 'Update the service.'}</p>
        </div>
      )}
      {state.kind === 'ended' && (
        <div role="alert" className="flex flex-wrap items-center gap-4 rounded-md border border-line px-6 py-4 text-sm">
          Your session ended for {t.product}.
          {/* the popup's tokens are this page's: the mounted console comes back as it was, what it held kept */}
          <button type="button" className="btn-outline" onClick={() => session.again().then(() => setState({ kind: 'mounted' }), () => undefined)}>Sign in again</button>
        </div>
      )}
      <div ref={ref} className={`min-h-[70vh] min-w-0 ${state.kind === 'mounted' ? '' : 'hidden'}`} />
    </>
  )
}
