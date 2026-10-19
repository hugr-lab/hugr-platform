import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { Shell } from './components/Shell'
import { Api, ApiError } from './lib/api'
import type { Session } from './lib/auth'
import { AppContext } from './lib/context'
import type { Config, Rights } from './lib/types'
import { Broken, NoNode, Refused, SignIn } from './screens/Entry'
import { Fleet } from './screens/Fleet'
import { Group } from './screens/Group'
import { Cluster } from './screens/Cluster'
import { Mounted } from './screens/Mounted'
import { Events } from './screens/node/Events'
import { Inventory } from './screens/node/Inventory'
import { Logs } from './screens/node/Logs'
import { Overview } from './screens/node/Overview'
import { Sessions } from './screens/node/Sessions'

type State =
  | { kind: 'loading' }
  | { kind: 'ready'; rights: Rights }
  | { kind: 'no_node' }
  | { kind: 'refused' }
  | { kind: 'broken'; message: string }

export function App({ config, session }: { config: Config; session: Session }) {
  const signedIn = useSyncExternalStore(
    (l) => session.subscribe(l),
    () => session.signedIn,
  )
  const [state, setState] = useState<State>({ kind: 'loading' })
  // a token the nodes refuse twice: sign in again, back to this page (a popup without a click is blocked) - once: refused
  // right after a sign-in, the page says so instead (the IdP would answer without a prompt, the nodes refuse again)
  const reauth = useCallback(() => {
    if (session.signedInJustNow) setState({ kind: 'refused' })
    else void session.signIn(window.location.pathname + window.location.search).catch(() => setState({ kind: 'refused' }))
  }, [session])
  const api = useMemo(() => new Api(session, config.audiences.nodes, reauth), [session, config, reauth])
  const learn = useCallback(() => {
    setState({ kind: 'loading' })
    api.me().then(
      (me) => setState({ kind: 'ready', rights: me.rights }),
      (e) => {
        if (e instanceof ApiError && e.code === 'no_node') setState({ kind: 'no_node' })
        else if (e instanceof ApiError && e.status === 401) return // the Api's reauth decided
        else setState({ kind: 'broken', message: e instanceof Error ? e.message : 'the console could not start' })
      },
    )
  }, [api])
  useEffect(() => {
    if (signedIn) learn()
  }, [signedIn, learn])

  if (!signedIn) return <SignIn config={config} session={session} />
  if (state.kind === 'loading') return <div className="p-16 text-ink-muted" role="status">Reading your rights</div>
  if (state.kind === 'no_node') return <NoNode config={config} session={session} onRetry={learn} />
  if (state.kind === 'refused') return <Refused config={config} session={session} />
  if (state.kind === 'broken') return <Broken message={state.message} />
  const rights = state.rights
  return (
    <AppContext.Provider value={{ config, session, api, rights }}>
      <BrowserRouter>
        <Shell>
          <Routes>
            {rights.fleet && <Route path="/" element={<Fleet />} />}
            {rights.fleet && <Route path="/groups/:name" element={<Group />} />}
            {rights.fleet && <Route path="/cluster" element={<Cluster />} />}
            {rights.fleet && <Route path="/nodes/:label" element={<Overview />} />}
            {rights.sessions && <Route path="/nodes/:label/sessions" element={<Sessions />} />}
            {rights.fleet && <Route path="/nodes/:label/inventory" element={<Inventory />} />}
            {rights.fleet && <Route path="/nodes/:label/logs" element={<Logs />} />}
            {rights.events && <Route path="/nodes/:label/events" element={<Events />} />}
            {config.mounts.tresor && <Route path="/secrets/*" element={<Mounted which="tresor" basePath="/secrets" />} />}
            {config.mounts.kista && <Route path="/extensions/*" element={<Mounted which="kista" basePath="/extensions" />} />}
            <Route path="*" element={rights.fleet ? <Navigate to="/" replace /> : <NothingHere />} />
          </Routes>
        </Shell>
      </BrowserRouter>
    </AppContext.Provider>
  )
}

function NothingHere() {
  return (
    <div className="flex max-w-[640px] flex-col gap-2">
      <h1 className="text-[32px] font-semibold leading-10">Nothing here for you</h1>
      <p className="text-ink-muted">Your rights on this platform open none of the console's pages. They come from the policy; ask its administrator.</p>
    </div>
  )
}
