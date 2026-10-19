import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './styles.css'
import { App } from './App'
import { loadConfig } from './lib/api'
import { Session, localPath } from './lib/auth'
import { Broken } from './screens/Entry'

// the theme before the first paint: the person's choice in this browser, else the system's
try {
  const kept = localStorage.getItem('hugr.theme')
  document.documentElement.dataset.theme =
    kept === 'light' || kept === 'dark' ? kept : window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
} catch {
  /* storage refused */
}

const root = createRoot(document.getElementById('root')!)

async function start() {
  let config
  try {
    config = await loadConfig()
  } catch (e) {
    root.render(<Broken message={e instanceof Error ? e.message : 'this platform has no console configured'} />)
    return
  }
  const session = new Session(config)
  if (window.location.pathname === '/callback') {
    try {
      const back = await session.callback()
      if (back === undefined) return // a popup: it closes
      window.history.replaceState(null, '', localPath(back) ?? '/')
    } catch (e) {
      root.render(<Broken message={`The sign-in did not complete: ${e instanceof Error ? e.message : 'unknown'}`} />)
      return
    }
  } else if (session.remembered) {
    // a reload: the IdP's own session signs the person in again, without a prompt when it has one
    await session.signIn(window.location.pathname + window.location.search)
    return
  }
  root.render(
    <StrictMode>
      <App config={config} session={session} />
    </StrictMode>,
  )
}

void start()
