// A policy view's rows as a plain table (spec 007): read on a node, filtered and paged by the console's server.
import { useState, type ReactNode } from 'react'
import { Problem } from './ui'
import { useApp, useLoad } from '../lib/context'
import { cell } from '../lib/policy'
import type { ViewPage } from '../lib/types'

const PAGE = 500

/** the rows of a page, as a table: the columns asked for (all when none), a cell may be a link */
export function Rows({ page, columns, link }: { page: ViewPage; columns?: string[];
  link?: (column: string, row: Record<string, unknown>) => ReactNode | undefined }) {
  const shown = columns ?? page.columns
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr>{shown.map((c) => <th key={c} className="th">{c.replaceAll('_', ' ')}</th>)}</tr>
        </thead>
        <tbody>
          {page.rows.map((row, i) => (
            <tr key={i}>
              {shown.map((c) => (
                <td key={c} className="td mono max-w-[480px] break-words align-top">{link?.(c, row) ?? cell(row[c])}</td>
              ))}
            </tr>
          ))}
          {page.rows.length === 0 && <tr><td className="td text-ink-muted" colSpan={shown.length}>None.</td></tr>}
        </tbody>
      </table>
    </div>
  )
}

/** a view's rows, read when shown; more pages on demand */
export function ViewTable({ view, filters = {}, columns, title, link }: { view: string; filters?: Record<string, string>;
  columns?: string[]; title?: string; link?: (column: string, row: Record<string, unknown>) => ReactNode | undefined }) {
  const { api } = useApp()
  const [limit, setLimit] = useState(PAGE)
  const key = JSON.stringify(filters)
  const page = useLoad(() => api.policyView(view, filters, 0, limit), [api, view, key, limit])
  return (
    <section aria-label={title ?? view} className="flex flex-col gap-3">
      {title && <h2 className="text-[18px] font-semibold leading-7">{title}</h2>}
      {page.error !== undefined && <Problem error={page.error} />}
      {page.loading && !page.data && <p className="text-sm text-ink-muted">Reading on a node…</p>}
      {page.data && <Rows page={page.data} columns={columns} link={link} />}
      {page.data && page.data.total > page.data.rows.length && (
        <div className="flex items-center gap-4 text-sm text-ink-muted">
          {page.data.rows.length} of {page.data.total}
          <button type="button" className="btn-quiet" onClick={() => setLimit((l) => Math.min(l + PAGE, 5000))}
            disabled={limit >= 5000}>Show more</button>
        </div>
      )}
    </section>
  )
}
