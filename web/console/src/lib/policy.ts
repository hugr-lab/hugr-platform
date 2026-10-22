// The policy's pages (spec 007): what they read, offered by what the person reads (console_info's my_views and
// my_functions), as the DBeaver client offers them.
import type { Rights } from './types'

/** the policy's views - the server reads no other on these pages */
export const policyViews = ['catalogs', 'relations', 'relation_columns', 'schemas', 'functions', 'function_columns',
  'references', 'reference_columns', 'keys', 'grants', 'schema_grants', 'object_grants', 'grant_columns', 'roles',
  'role_claims', 'admins', 'platform_grants', 'issuers', 'clients', 'role_mappings', 'function_categories',
  'function_category_members', 'function_grants', 'resource_groups', 'role_resource_groups', 'catalog_schema']

export const reads = (rights: Rights, view: string) => (rights.views ?? []).includes(view)
export const calls = (rights: Rights, fn: string) => (rights.functions ?? []).includes(fn)

/** whether the person reads any of the policy: the navigation's Policy */
export const readsPolicy = (rights: Rights) => policyViews.some((v) => reads(rights, v))

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
