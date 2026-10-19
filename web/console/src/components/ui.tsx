import { useEffect, useRef, type ReactNode } from 'react'
import { ChevronRight, CircleAlert, RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { ApiError } from '../lib/api'

/** a state told by the circle's shape, never by colour alone: full = good, half = in between, ring = off */
export type Mark = 'ok' | 'half' | 'off'

export function Glyph({ mark }: { mark: Mark }) {
  if (mark === 'ok')
    return (
      <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
        <circle cx="5" cy="5" r="4.5" fill="var(--brand)" />
      </svg>
    )
  if (mark === 'half')
    return (
      <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
        <circle cx="5" cy="5" r="4" fill="none" stroke="var(--ink)" strokeWidth="1.25" />
        <path d="M5 1 A4 4 0 0 1 5 9 Z" fill="var(--ink)" />
      </svg>
    )
  return (
    <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
      <circle cx="5" cy="5" r="4" fill="none" stroke="var(--ink-muted)" strokeWidth="1.25" />
    </svg>
  )
}

export function Status({ mark, children }: { mark: Mark; children: ReactNode }) {
  return (
    <span className="pill pl-2">
      <Glyph mark={mark} />
      {children}
    </span>
  )
}

export function Chip({ children }: { children: ReactNode }) {
  return <span className="mono rounded-sm bg-surface-soft px-2 py-px leading-5">{children}</span>
}

export function Crumbs({ items }: { items: { label: ReactNode; to?: string }[] }) {
  return (
    <nav aria-label="Breadcrumb" className="text-sm text-ink-muted">
      <ol className="flex flex-wrap items-center gap-2">
        {items.map((item, i) => (
          <li key={i} className="flex items-center gap-2" aria-current={item.to ? undefined : 'page'}>
            {i > 0 && <ChevronRight size={14} aria-hidden="true" />}
            {item.to ? (
              <Link className="link" to={item.to}>
                {item.label}
              </Link>
            ) : (
              item.label
            )}
          </li>
        ))}
      </ol>
    </nav>
  )
}

export function Refresh({ at, loading, onClick }: { at?: Date; loading: boolean; onClick: () => void }) {
  return (
    <div className="flex flex-wrap items-center gap-4">
      {at && <span className="text-sm text-ink-muted">Read at {at.toLocaleTimeString()}</span>}
      <button className="btn-outline" type="button" onClick={onClick} disabled={loading}>
        <RefreshCw size={18} aria-hidden="true" />
        Refresh
      </button>
    </div>
  )
}

export function Header({ title, lead, actions }: { title: ReactNode; lead?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="flex flex-wrap items-end justify-between gap-4">
      <div className="flex max-w-[680px] flex-col gap-2">
        <h1 className="text-[32px] font-semibold leading-10 tracking-[-0.01em]">{title}</h1>
        {lead && <p className="text-ink-muted">{lead}</p>}
      </div>
      {actions}
    </header>
  )
}

/** what a failed read says: the API's message, never more */
export function messageOf(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error) return error.message
  return 'something went wrong'
}

export function Problem({ error }: { error: unknown }) {
  return (
    <p role="alert" className="flex items-start gap-3 rounded-md border border-line px-6 py-4 text-sm">
      <CircleAlert size={20} aria-hidden="true" className="mt-0.5 flex-none" />
      {messageOf(error)}
    </p>
  )
}

/** a native dialog, open while `open` (no component library: an injected <style> breaks the CSP) */
export function Dialog({ open, title, onClose, children }: { open: boolean; title: ReactNode; onClose: () => void;
  children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (open && !d.open) d.showModal?.()
    if (!open && d.open) d.close?.()
  }, [open])
  return (
    <dialog ref={ref} onClose={onClose} aria-labelledby="dialog-title"
      className="w-full max-w-[520px] rounded-lg border border-line bg-surface p-8 text-ink">
      {open && (
        <div className="flex flex-col gap-6">
          <h2 id="dialog-title" className="text-[22px] font-semibold leading-[30px]">
            {title}
          </h2>
          {children}
        </div>
      )}
    </dialog>
  )
}

export function Stat({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <div className="eyebrow">{label}</div>
      <div className="text-[22px] font-semibold leading-[30px]">{value}</div>
      {sub && <div className="text-sm text-ink-muted">{sub}</div>}
    </div>
  )
}

/** "4 s", "2 min", "3 h": an age a person reads */
export function age(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return '-'
  if (seconds < 60) return `${seconds} s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min`
  return `${Math.floor(seconds / 3600)} h`
}
