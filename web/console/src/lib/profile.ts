// The cluster's profile changed from a form (spec 010 stage 2): one CLUSTER statement per form, the console's server
// builds its text. Here: which fields each change takes, what the form lacks, and how the node's answer reads.
import type { Place, Policy, ProfileChange, ProfileWritten } from './types'

export type Action = ProfileChange['action']

export const actions: { value: Action; label: string }[] = [
  { value: 'install', label: 'Install an extension' },
  { value: 'update', label: 'Update an extension' },
  { value: 'remove', label: 'Remove an extension' },
  { value: 'attach', label: 'Attach a source' },
  { value: 'detach', label: 'Detach a source' },
  { value: 'set', label: 'Set a setting' },
  { value: 'reset', label: 'Reset a setting' },
  { value: 'add-script', label: 'Add a boot script' },
  { value: 'alter-script', label: 'Change a boot script' },
  { value: 'drop-script', label: 'Drop a boot script' },
]

/** where a boot script runs (hugr-node spec 047) */
export const places: { value: Place; label: string }[] = [
  { value: 'before_sources', label: 'Before the sources' },
  { value: 'after_source', label: 'After one source' },
  { value: 'after_sources', label: 'After the sources' },
]

const policyLabels: Record<Policy, string> = {
  fail_node: 'Fail the node: it does not serve',
  continue: 'Continue: the node logs it and goes on',
  detach_source: 'Detach the source: the node serves without it',
  retry: 'Retry: the node serves and tries again later',
}

/** the policies a place takes, its default first (hugr-node spec 047's table; the node judges again) */
export function policiesOf(place: Place | undefined): { value: Policy; label: string }[] {
  const of = (p: Policy[]) => p.map((value) => ({ value, label: policyLabels[value] }))
  switch (place) {
    case 'before_sources': return of(['fail_node', 'continue'])
    case 'after_source': return of(['detach_source', 'retry', 'continue', 'fail_node'])
    case 'after_sources': return of(['fail_node', 'retry', 'continue'])
    default: return of(['fail_node', 'continue', 'detach_source', 'retry'])
  }
}

/** a script's name: letters, digits, _ - it may start with a digit (10_vars), the name orders it among its place's */
const scriptName = /^[A-Za-z0-9_]{1,63}$/
/** the longest script the form writes */
export const maxScriptBytes = 64 << 10
/** a script's SQL: text over lines, tabs kept, no other control character */
const scriptControl = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/

/** a name the CLUSTER grammar takes bare: an extension, a repository, a type, a secret, a setting */
const bare = /^[A-Za-z_][A-Za-z0-9_]{0,62}$/
const control = /[\u0000-\u001f\u007f-\u009f]/
const line = (v: string | undefined) => !!v && v.length <= 1024 && !control.test(v)
/** a source's name as the server writes it: one line of at most 256 bytes, no comma (hugr-node joins a dependency list
 * by commas), no space at its ends */
const sourceName = (v: string | undefined) => !!v && new TextEncoder().encode(v).length <= 256 && !control.test(v) &&
  !v.includes(',') && v.trim() === v

/** the change as the form stands, only the fields its action takes */
export function changeOf(action: Action, f: Partial<ProfileChange>, group?: string): ProfileChange {
  const out: ProfileChange = { action }
  const keep = (keys: (keyof ProfileChange)[]) => {
    for (const k of keys) {
      const v = f[k]
      if (v !== undefined && v !== '' && v !== false && !(Array.isArray(v) && v.length === 0)) (out as unknown as Record<string, unknown>)[k] = v
    }
  }
  if (action === 'install') keep(['extension', 'version', 'repository', 'comment'])
  if (action === 'update') keep(['extension', 'version'])
  if (action === 'remove') keep(['extension'])
  if (action === 'attach') keep(['path', 'alias', 'type', 'secret', 'readOnly', 'lazy', 'idle', 'lineage', 'dependsOn', 'comment'])
  if (action === 'detach') keep(['alias', 'cascade', 'force'])
  if (action === 'set') keep(['setting', 'value'])
  if (action === 'reset') keep(['setting'])
  if (action === 'sources') keep(f.all ? ['all', 'lazy', 'idle'] : ['sources', 'lazy', 'idle'])
  if (action === 'add-script' || action === 'alter-script')
    keep(['script', 'place', ...(f.place === 'after_source' ? ['source' as const] : []), 'sql', 'onError', 'comment'])
  if (action === 'drop-script') keep(['script'])
  if (group) out.group = group
  return out
}

/** what the form lacks before it can be sent; nothing when it can (the server and the node judge again) */
export function changeProblem(c: ProfileChange): string | undefined {
  switch (c.action) {
    case 'install':
    case 'update':
      if (!bare.test(c.extension ?? '')) return 'Name the extension (letters, digits, _).'
      if (!line(c.version)) return 'Give its version.'
      if (c.repository && !bare.test(c.repository)) return 'A repository is a plain name.'
      return undefined
    case 'remove':
      return bare.test(c.extension ?? '') ? undefined : 'Name the extension (letters, digits, _).'
    case 'attach':
      // a source its secret names has no path (a postgres source: ATTACH '' … SECRET s)
      if (!(c.path === undefined && c.secret) && !line(c.path)) return 'Give the source\'s path, or the secret that names it.'
      if (!sourceName(c.alias)) return 'Name the source (its alias): no comma, no space at its ends.'
      if (!bare.test(c.type ?? '')) return 'Give its type (duckdb, postgres, mssql, ...).'
      if (c.secret && !bare.test(c.secret)) return 'A secret is a plain name.'
      if ((c.dependsOn ?? []).some((d) => !sourceName(d))) return 'A dependency names a source (its alias).'
      return lazyProblem(c)
    case 'detach':
      return sourceName(c.alias) ? undefined : 'Name the source (its alias): no comma, no space at its ends.'
    case 'set':
      if (!bare.test(c.setting ?? '')) return 'Name the setting.'
      return line(c.value) ? undefined : 'Give its value.'
    case 'reset':
      return bare.test(c.setting ?? '') ? undefined : 'Name the setting.'
    case 'sources':
      if (!c.group) return 'A source set is a group\'s.'
      if (c.all) return c.lazy ? lazyProblem(c) : 'Every source attached at once is no set: reset the group\'s set instead.'
      if ((c.sources ?? []).length === 0) return 'Pick the sources the group\'s nodes attach.'
      if ((c.sources ?? []).some((d) => !sourceName(d))) return 'A source is named by its alias.'
      return lazyProblem(c)
    case 'reset-sources':
      return c.group ? undefined : 'A source set is a group\'s.'
    case 'add-script':
    case 'alter-script':
    case 'drop-script':
      return scriptProblem(c)
  }
}

/** what a boot script's form lacks */
function scriptProblem(c: ProfileChange): string | undefined {
  if (!scriptName.test(c.script ?? '')) return 'Name the script (letters, digits, _): the name orders it among its place\'s.'
  if (c.action === 'drop-script') return undefined
  if (c.action === 'add-script' && !c.place) return 'Pick where it runs.'
  if (c.place === 'after_source' && !sourceName(c.source)) return 'Name the source it follows (its alias).'
  if (c.action === 'add-script' && !c.sql?.trim()) return 'Write its SQL.'
  if (c.sql !== undefined) {
    if (!c.sql.trim()) return 'Write its SQL, or leave it to keep the one it has.'
    if (new TextEncoder().encode(c.sql).length > maxScriptBytes) return 'A script is at most 64 KiB.'
    if (scriptControl.test(c.sql)) return 'A script holds no control character but a line break and a tab.'
  }
  if (c.onError && !policiesOf(c.place).some((p) => p.value === c.onError)) return 'Pick a policy its place takes.'
  if (c.action === 'alter-script' && !c.place && c.sql === undefined && !c.onError && !c.comment)
    return 'Change its place, its SQL, its policy or its comment.'
  if (c.comment && !line(c.comment)) return 'A comment is one line.'
  return undefined
}

/** an idle time is a lazy source's, one line ('30 min'); the node reads the interval and refuses one under a minute */
function lazyProblem(c: ProfileChange): string | undefined {
  if (c.idle && !c.lazy) return 'An idle time is a lazy source\'s: mark it lazy.'
  if (c.idle && !line(c.idle)) return 'Give the idle time as an interval, such as 30 min.'
  return undefined
}

/** how a set's laziness reads: attached on the first read, detached after an idle time (or never) */
export function lazyText(lazy: boolean, idleSeconds: number): string {
  if (!lazy) return 'attached at once'
  if (idleSeconds <= 0) return 'attached on its first read, kept until the node stops (or its own idle time)'
  return `attached on its first read, detached after ${duration(idleSeconds)} unused`
}

function duration(s: number): string {
  if (s % 3600 === 0) return s === 3600 ? '1 hour' : `${s / 3600} hours`
  if (s % 60 === 0) return s === 60 ? '1 minute' : `${s / 60} minutes`
  return `${s} s`
}

/** how the change reaches the nodes, as the node classed it */
export function reachOf(w: ProfileWritten): string {
  const here = w.appliedHere ? ` ${w.node} applied it at once.` : ''
  switch (w.class) {
    case 'hot':
      return `Profile version ${w.version}: applied live -${here} the other nodes take it on their next tick.`
    case 'drain':
      return `Profile version ${w.version}: the nodes take it as the orchestrator drains and replaces them, one at a time.`
    case 'restart':
      return `Profile version ${w.version}: the nodes take it at a restart - restarting the group from the fleet replaces them one at a time.`
    default:
      return `Profile version ${w.version}: ${w.class || 'written'}.`
  }
}
