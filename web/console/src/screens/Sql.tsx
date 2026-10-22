// The SQL page (spec 007 stage 1): a statement run through the platform's quack door by DuckDB in this page, as the
// person - or, with passthrough, under ACL NATIVE, the break-glass door where nothing is refused. The door's secret is
// tresor's (spec 009): the page gives tresor the person's token for tresor, nothing else.
import { useState } from 'react'
import { Header, Problem, Segmented } from '../components/ui'
import { useApp } from '../lib/context'
import { ROWS_MAX, run, type Answer } from '../lib/duckdb'

type Mode = 'you' | 'native'

export function Sql() {
  const { session, config, rights } = useApp()
  const native = rights.bundles.includes('passthrough')
  const [sql, setSql] = useState('')
  const [mode, setMode] = useState<Mode>('you')
  const [busy, setBusy] = useState(false)
  const [answer, setAnswer] = useState<Answer>()
  const [error, setError] = useState<unknown>()
  const [first, setFirst] = useState(true)
  const runIt = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const token = await session.getToken(config.audiences.tresor)
      setAnswer(await run(sql, token, config.issuer, native && mode === 'native'))
    } catch (e) {
      setError(e)
      setAnswer(undefined)
    } finally {
      setBusy(false)
      setFirst(false)
    }
  }
  return (
    <>
      <Header title="SQL"
        lead="A statement runs on a node of your group through the platform's quack door, by DuckDB in this page, under your sign-in. The node judges it as any client's." />
      <div className="flex flex-col gap-4">
        {native && (
          <Segmented legend="Run" name="mode" value={mode} onChange={setMode}
            options={[{ value: 'you', label: 'As you' }, { value: 'native', label: 'ACL NATIVE' }]} />
        )}
        {native && mode === 'native' && (
          <p role="note" className="rounded-md border border-line px-4 py-3 text-sm">
            ACL NATIVE is the break-glass door: plain SQL outside the virtual catalog, refused nothing, on the node's physical databases. The node's audit names you.
          </p>
        )}
        <label htmlFor="sql" className="text-sm font-semibold">Statement</label>
        <textarea id="sql" value={sql} onChange={(e) => setSql(e.target.value)} rows={8} spellCheck={false}
          className="mono w-full rounded-sm border border-ink-muted bg-surface p-3 text-sm text-ink" />
        <div className="flex flex-wrap items-center gap-4">
          <button type="button" className="btn-primary" disabled={busy || sql.trim() === ''} onClick={runIt}>Run</button>
          {busy && <span role="status" className="text-sm text-ink-muted">{first ? 'Starting DuckDB in this page, then running…' : 'Running…'}</span>}
          <span className="text-sm text-ink-muted">An answer is cut at {ROWS_MAX} rows or 4 MiB.</span>
        </div>
      </div>
      {error !== undefined && <Problem error={error} />}
      {answer && (
        <section aria-label="The answer" className="flex flex-col gap-3">
          <div className="overflow-x-auto rounded-md border border-line">
            <table className="w-full border-collapse text-sm">
              <thead>
                <tr>{answer.columns.map((c, i) => <th key={i} className="th normal-case">{c}</th>)}</tr>
              </thead>
              <tbody>
                {answer.rows.map((row, r) => (
                  <tr key={r}>
                    {row.map((v, i) => (
                      <td key={i} className={`td mono max-w-[480px] whitespace-pre-wrap break-words align-top ${v === null ? 'text-ink-muted' : ''}`}>
                        {v ?? 'NULL'}
                      </td>
                    ))}
                  </tr>
                ))}
                {answer.rows.length === 0 && <tr><td className="td text-ink-muted" colSpan={answer.columns.length || 1}>No rows.</td></tr>}
              </tbody>
            </table>
          </div>
          <p className="text-sm text-ink-muted">{answer.rows.length} {answer.rows.length === 1 ? 'row' : 'rows'}{answer.cut ? `; cut: ${answer.cut}` : ''}.</p>
        </section>
      )}
    </>
  )
}
