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
  rows: unknown[][]
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
  await db.open({ path: ':memory:', query: { castBigIntToDouble: true }, allowUnsignedExtensions: true })
  const conn = await db.connect()
  // nothing fetched from elsewhere: the page reaches its own origin only
  await conn.query('SET autoinstall_known_extensions = false')
  for (const extension of [httpfs, quack]) {
    await conn.query(`LOAD ${literal(new URL(extension, window.location.href).href)}`)
  }
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

/** rows of the answer up to the page's bounds: whole rows only, the cut said */
export function bounded(columns: string[], rows: unknown[][]): Answer {
  const out: Answer = { columns, rows: [] }
  let bytes = 0
  for (const row of rows) {
    if (out.rows.length === ROWS_MAX) {
      out.cut = `the first ${ROWS_MAX} rows`
      break
    }
    bytes += JSON.stringify(row, (_k, v) => (typeof v === 'bigint' ? v.toString() : v)).length
    if (bytes > BYTES_MAX) {
      out.cut = `the first ${out.rows.length} rows, ${BYTES_MAX >> 20} MiB`
      break
    }
    out.rows.push(row)
  }
  return out
}

/** runs the statement through the platform's door, as the person: the token goes into the page's quack secret for the
 * door and as the console's header (the console forwards only a signed-in person's requests) */
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
  const statement = await conn.prepare(`FROM quack_query(?, ?, disable_ssl := ?) LIMIT ${ROWS_MAX + 1}`)
  try {
    const table = await statement.query(uri, statementOf(sql, native), window.location.protocol !== 'https:')
    const columns = table.schema.fields.map((f) => f.name)
    const rows = table.toArray().map((r) => columns.map((c) => (r as Record<string, unknown>)[c]))
    return bounded(columns, rows)
  } finally {
    await statement.close()
  }
}
