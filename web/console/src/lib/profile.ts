// The cluster's profile changed from a form (spec 010 stage 2): one CLUSTER statement per form, the console's server
// builds its text. Here: which fields each change takes, what the form lacks, and how the node's answer reads.
import type { ProfileChange, ProfileWritten } from './types'

export type Action = ProfileChange['action']

export const actions: { value: Action; label: string }[] = [
  { value: 'install', label: 'Install an extension' },
  { value: 'update', label: 'Update an extension' },
  { value: 'remove', label: 'Remove an extension' },
  { value: 'attach', label: 'Attach a source' },
  { value: 'detach', label: 'Detach a source' },
  { value: 'set', label: 'Set a setting' },
  { value: 'reset', label: 'Reset a setting' },
]

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
  if (action === 'attach') keep(['path', 'alias', 'type', 'secret', 'readOnly', 'lineage', 'dependsOn', 'comment'])
  if (action === 'detach') keep(['alias', 'cascade', 'force'])
  if (action === 'set') keep(['setting', 'value'])
  if (action === 'reset') keep(['setting'])
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
      if (!line(c.path)) return 'Give the source\'s path.'
      if (!sourceName(c.alias)) return 'Name the source (its alias): no comma, no space at its ends.'
      if (!bare.test(c.type ?? '')) return 'Give its type (duckdb, postgres, mssql, ...).'
      if (c.secret && !bare.test(c.secret)) return 'A secret is a plain name.'
      if ((c.dependsOn ?? []).some((d) => !sourceName(d))) return 'A dependency names a source (its alias).'
      return undefined
    case 'detach':
      return sourceName(c.alias) ? undefined : 'Name the source (its alias): no comma, no space at its ends.'
    case 'set':
      if (!bare.test(c.setting ?? '')) return 'Name the setting.'
      return line(c.value) ? undefined : 'Give its value.'
    case 'reset':
      return bare.test(c.setting ?? '') ? undefined : 'Name the setting.'
  }
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
      return `Profile version ${w.version}: the nodes take it at a restart - the orchestrator replaces them one at a time.`
    default:
      return `Profile version ${w.version}: ${w.class || 'written'}.`
  }
}
