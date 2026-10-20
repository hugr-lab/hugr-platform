import type { Link } from './types'

/** bytes a person reads: 9.8 GiB */
export function bytes(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '-'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

/** a share as a percent for a bar's width, 0..100 */
export function share(part: number | null | undefined, whole: number | null | undefined): number {
  if (!part || !whole || whole <= 0) return 0
  return Math.max(0, Math.min(100, Math.round((part / whole) * 100)))
}

/** the trace id of a W3C traceparent (00-<trace>-<span>-<flags>), or nothing */
export function traceOf(traceparent: string | null | undefined): string | undefined {
  const parts = (traceparent ?? '').split('-')
  return parts.length === 4 && /^[0-9a-f]{32}$/.test(parts[1]) ? parts[1] : undefined
}

/** a link out (spec 003 round 2): the installation's template, each value URL-escaped; nothing when a value it names
 * is missing, so a link never points at a half-filled page */
export function fill(link: Link, values: Record<string, string | undefined>): string | undefined {
  let missing = false
  const url = link.url.replace(/\{(node|group|session|trace)\}/g, (_, name: string) => {
    const v = values[name]
    if (!v) missing = true
    return encodeURIComponent(v ?? '')
  })
  if (missing || !url.startsWith('https://')) return undefined
  return url
}

export function linksOf(links: Link[], kind: Link['kind']): Link[] {
  return links.filter((l) => l.kind === kind)
}

/** the backend's own home, for a page that names no node, session or trace: the template's origin */
export function home(link: Link): string | undefined {
  try {
    const u = new URL(link.url.replace(/\{(node|group|session|trace)\}/g, ''))
    return u.protocol === 'https:' ? u.origin : undefined
  } catch {
    return undefined
  }
}

/** a reason as a sentence: trimmed, capitalised, one full stop (none after a text that ends in one) */
export function sentence(text: string): string {
  const s = text.trim()
  if (!s) return ''
  return `${s.charAt(0).toUpperCase()}${s.slice(1)}${/[.!?]$/.test(s) ? '' : '.'}`
}
