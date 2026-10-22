// The SQL page's DuckDB (spec 007): duckdb-wasm in a worker of the console's own origin, loaded when the page is first
// used. It attaches the platform as a quack client does: a quack URI has no path, so the client posts to the console's
// own /quack, which forwards to the platform's door. The node behind the door judges every statement under the
// person's token; nothing in the page enforces anything.
import type { AsyncDuckDB, AsyncDuckDBConnection } from '@duckdb/duckdb-wasm'

/** what the page reads of an answer before it cuts it, and says so */
export const ROWS_MAX = 1000
export const BYTES_MAX = 4 << 20

export interface Answer {
  columns: string[]
  /** each value as DuckDB writes it as text (NULL: null) */
  rows: (string | null)[][]
  /** why the answer is not whole: more rows, or more bytes, than the page reads */
  cut?: string
}

let opened: Promise<AsyncDuckDBConnection> | undefined

async function open(): Promise<AsyncDuckDBConnection> {
  const duckdb = await import('@duckdb/duckdb-wasm')
  const wasm = (await import('@duckdb/duckdb-wasm/dist/duckdb-base.wasm?url')).default
  const script = (await import('@duckdb/duckdb-wasm/dist/duckdb-browser-base.worker.js?url')).default
  // quack built with one change (no heartbeat thread without pthreads, duckdb/duckdb-quack#328) and the httpfs its HTTP
  // needs, both shipped with the package and served from the console's own origin: unsigned, as nothing DuckDB signed
  // connects from this build
  const httpfs = (await import('@duckdb/duckdb-wasm/dist/extensions/httpfs.duckdb_extension.wasm?url')).default
  const quack = (await import('@duckdb/duckdb-wasm/dist/extensions/quack.duckdb_extension.wasm?url')).default
  const db: AsyncDuckDB = new duckdb.AsyncDuckDB(new duckdb.VoidLogger(), new Worker(script))
  await db.instantiate(wasm)
  await db.open({ path: ':memory:', allowUnsignedExtensions: true })
  const conn = await db.connect()
  // no extension from elsewhere: DuckDB-wasm would fetch a known one it autoloads, and only the CSP would stop it
  await conn.query('SET autoinstall_known_extensions = false')
  await conn.query('SET autoload_known_extensions = false')
  for (const extension of [httpfs, quack]) {
    await conn.query(`LOAD ${literal(new URL(extension, window.location.href).href)}`)
  }
  // nothing changes the page's DuckDB after this: no LOAD, no setting (the person's text never runs here anyway)
  await conn.query('SET lock_configuration = true')
  return conn
}

/** the platform's door as quack names it: the console's own host and port */
export function doorURI(location: { hostname: string; port: string; protocol: string }): string {
  const port = location.port || (location.protocol === 'https:' ? '443' : '80')
  return `quack:${location.hostname}:${port}`
}

/** a value as an SQL string literal of the page's own DuckDB */
export const literal = (s: string) => `'${s.replaceAll("'", "''")}'`

/** the statement the door runs: the person's text, behind ACL NATIVE in the break-glass mode */
export const statementOf = (sql: string, native: boolean) => (native ? `ACL NATIVE ${sql}` : sql)

/** the text's size in bytes, as UTF-8 */
const encoder = new TextEncoder()
const size = (v: string | null) => (v === null ? 4 : encoder.encode(v).length)

/** takes the row into the answer while it is within the page's bounds; false once cut, the cut said */
function admit(out: Answer, row: (string | null)[], used: { bytes: number }): boolean {
  if (out.rows.length === ROWS_MAX) {
    out.cut = `the first ${ROWS_MAX} rows`
    return false
  }
  used.bytes += row.reduce((n, v) => n + size(v), 0)
  if (used.bytes > BYTES_MAX) {
    out.cut = `the first ${out.rows.length} rows, ${BYTES_MAX >> 20} MiB`
    return false
  }
  out.rows.push(row)
  return true
}

/** rows of the answer up to the page's bounds: whole rows only, the cut said */
export function bounded(columns: string[], rows: Iterable<(string | null)[]>): Answer {
  const out: Answer = { columns, rows: [] }
  const used = { bytes: 0 }
  for (const row of rows) if (!admit(out, row, used)) break
  return out
}

/** runs the statement through the platform's door, as the person: the token goes into the page's quack secret for the
 * door and as the console's header (the console forwards only a signed-in person's requests). DuckDB writes every
 * value as text (a DECIMAL's scale, a date, a list, a 64-bit integer as they are); the answer is read batch by batch and
 * the read stops at the page's bounds. */
export async function run(sql: string, token: string, native: boolean): Promise<Answer> {
  opened ??= open().catch((e) => {
    opened = undefined
    throw e
  })
  const conn = await opened
  const uri = doorURI(window.location)
  await conn.query(`CREATE OR REPLACE SECRET hugr_door (TYPE quack, SCOPE ${literal(uri)}, TOKEN ${literal(token)}, ` +
    `EXTRA_HTTP_HEADERS MAP {'Authorization': ${literal('Bearer ' + token)}})`)
  // TLS as the page's own: quack goes without it to a local address by default, and the console's origin is https
  const statement = await conn.prepare('SELECT COLUMNS(*)::VARCHAR FROM ' +
    `(FROM quack_query(?, ?, disable_ssl := ?) LIMIT ${ROWS_MAX + 1})`)
  try {
    const reader = await statement.send(uri, statementOf(sql, native), window.location.protocol !== 'https:')
    let columns: string[] = []
    const rows = async function* () {
      for await (const batch of reader) {
        columns = batch.schema.fields.map((f) => f.name)
        for (let r = 0; r < batch.numRows; r++) {
          // by index: a column named toJSON or constructor is a column, never the row's method
          yield columns.map((_c, i) => {
            const v = batch.getChildAt(i)?.get(r)
            return v === null || v === undefined ? null : String(v)
          })
        }
      }
    }
    const out: Answer = { columns: [], rows: [] }
    const used = { bytes: 0 }
    for await (const row of rows()) if (!admit(out, row, used)) break
    out.columns = columns.length ? columns : reader.schema.fields.map((f) => f.name)
    return out
  } finally {
    await statement.close()
  }
}
