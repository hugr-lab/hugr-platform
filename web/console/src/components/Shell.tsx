import type { ReactNode } from 'react'
import { Layers, LogOut, Moon, Sun } from 'lucide-react'
import { NavLink } from 'react-router-dom'
import logo from '../assets/hugr-logo-on-dark.svg'
import { useApp, useTheme } from '../lib/context'

/** the console's frame: the navy menu (what the person's rights open), the person, the content */
export function Shell({ children }: { children: ReactNode }) {
  const { config, session, rights, api } = useApp()
  const [theme, toggle] = useTheme()
  const signOut = async () => {
    await api.signOut().catch(() => undefined) // the nodes' sessions end; the IdP's next
    await session.signOut()
  }
  const nav = 'flex items-center gap-3 rounded-full px-4 py-2.5 text-[15px] font-medium text-[#e8f1f2] transition-colors hover:bg-navy-line hover:text-white'
  return (
    <div className="flex min-h-screen flex-wrap items-stretch">
      <nav aria-label="Console" className="flex flex-[1_1_248px] flex-col gap-8 bg-navy px-4 py-8 text-[#e8f1f2]">
        <div className="flex flex-col gap-2 px-4">
          <img src={logo} alt="hugr" width={112} className="block h-auto w-[112px]" />
          <div className="text-[13px] font-semibold uppercase leading-5 tracking-[0.08em] text-navy-muted">
            Console · {config.platform}
          </div>
        </div>
        <ul className="flex flex-col gap-1">
          {rights.fleet && (
            <li>
              <NavLink to="/" end={false} className={({ isActive }) => `${nav} ${isActive ? 'bg-navy-line text-white' : ''}`}>
                <Layers size={20} aria-hidden="true" className="text-[#5cbdb6]" />
                Fleet
              </NavLink>
            </li>
          )}
        </ul>
        <div className="mt-auto flex flex-col gap-4 rounded-md border border-navy-line p-4">
          <div className="flex flex-col gap-0.5">
            <div className="text-[13px] font-semibold uppercase leading-5 tracking-[0.08em] text-navy-muted">Signed in</div>
            <div className="break-all text-sm font-medium text-white">{session.displayName ?? 'signed in'}</div>
          </div>
          {rights.bundles.length > 0 && (
            <ul className="flex flex-wrap gap-2" aria-label="Your rights on this platform">
              {rights.bundles.map((b) => (
                <li key={b} className="rounded-full border border-navy-line px-2.5 py-0.5 text-[13px] font-semibold leading-5">
                  {b}
                </li>
              ))}
            </ul>
          )}
          <div className="flex flex-wrap gap-2">
            <button type="button" className="btn-dark flex-1" onClick={signOut}>
              <LogOut size={18} aria-hidden="true" />
              Sign out
            </button>
            <button type="button" className="btn-dark w-11 px-0" onClick={toggle}
              aria-label={theme === 'dark' ? 'Light theme' : 'Dark theme'}>
              {theme === 'dark' ? <Sun size={18} aria-hidden="true" /> : <Moon size={18} aria-hidden="true" />}
            </button>
          </div>
        </div>
      </nav>
      <main className="flex min-w-0 flex-[999_1_560px] flex-col gap-8 px-[clamp(16px,5vw,64px)] pb-16 pt-12">
        {children}
      </main>
    </div>
  )
}
