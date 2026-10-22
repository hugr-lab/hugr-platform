// The policy's pages (spec 007): what they read, offered by what the person reads (console_info's my_views and
// my_functions), as the DBeaver client offers them.
import type { Rights } from './types'

/** the policy's views - the server reads no other on these pages */
export const policyViews = ['catalogs', 'relations', 'relation_columns', 'schemas', 'functions', 'function_columns',
  'references', 'keys', 'grants', 'schema_grants', 'object_grants', 'grant_columns', 'roles', 'role_claims', 'admins',
  'platform_grants', 'issuers', 'clients', 'role_mappings', 'function_categories', 'function_category_members',
  'function_grants', 'resource_groups', 'role_resource_groups']

export const reads = (rights: Rights, view: string) => (rights.views ?? []).includes(view)
export const calls = (rights: Rights, fn: string) => (rights.functions ?? []).includes(fn)

/** the policy's parts, each offered by any of the views (or the function) its page reads */
export const policyParts: { path: string; label: string; offered: (r: Rights) => boolean }[] = [
  { path: 'catalogs', label: 'Catalogs', offered: (r) => reads(r, 'catalogs') },
  { path: 'roles', label: 'Roles', offered: (r) => reads(r, 'roles') },
  { path: 'identity', label: 'Identity', offered: (r) => ['issuers', 'clients', 'role_mappings'].some((v) => reads(r, v)) },
  { path: 'functions', label: 'Functions',
    offered: (r) => ['function_categories', 'function_category_members', 'function_grants'].some((v) => reads(r, v)) },
  { path: 'resource-groups', label: 'Resource groups',
    offered: (r) => ['resource_groups', 'role_resource_groups'].some((v) => reads(r, v)) },
  { path: 'sources', label: 'Sources', offered: (r) => calls(r, 'exposures') },
]

/** whether the person reads any part of the policy: the navigation's Policy */
export const readsPolicy = (rights: Rights) => policyParts.some((p) => p.offered(rights))

/** a capabilities cell: NULL is the view's own meaning (a catalog grant's every data capability, an object grant's
 * inheritance), an empty set is none - never a dash, which would read as none */
export function caps(value: unknown, unstated: string): string {
  if (value === null || value === undefined) return unstated
  if (Array.isArray(value) && value.length === 0) return 'none'
  return cell(value)
}

/** a value as a cell shows it: a list joined, a structure as its true keys or compact JSON, nothing as a dash */
export function cell(value: unknown): string {
  if (value === null || value === undefined) return '-'
  if (typeof value === 'boolean') return value ? 'yes' : 'no'
  if (Array.isArray(value)) return value.length ? value.map(cell).join(', ') : '-'
  if (typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>)
    if (entries.length && entries.every(([, v]) => typeof v === 'boolean'))
      return entries.filter(([, v]) => v).map(([k]) => k).join(', ') || 'none'
    return JSON.stringify(value)
  }
  return String(value)
}
