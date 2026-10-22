// The policy, read (spec 007 stage 1): the pages follow the DBeaver client's tree - catalogs, roles, the identity, the
// function categories, the resource groups, the sources. Each part is offered by what the person reads.
import { useState } from 'react'
import { Link, NavLink, Navigate, useParams } from 'react-router-dom'
import { Crumbs, Header, Problem } from '../components/ui'
import { Rows, ViewTable } from '../components/ViewTable'
import { useApp, useLoad } from '../lib/context'
import { calls, reads } from '../lib/policy'
import type { Rights } from '../lib/types'

const enc = encodeURIComponent

/** the policy's parts, each with the view (or function) that offers it */
const parts: { path: string; label: string; offered: (r: Rights) => boolean }[] = [
  { path: 'catalogs', label: 'Catalogs', offered: (r) => reads(r, 'catalogs') },
  { path: 'roles', label: 'Roles', offered: (r) => reads(r, 'roles') },
  { path: 'identity', label: 'Identity', offered: (r) => reads(r, 'issuers') },
  { path: 'functions', label: 'Functions', offered: (r) => reads(r, 'function_categories') },
  { path: 'resource-groups', label: 'Resource groups', offered: (r) => reads(r, 'resource_groups') },
  { path: 'sources', label: 'Sources', offered: (r) => calls(r, 'exposures') },
]

function PartsNav() {
  const { rights } = useApp()
  return (
    <nav aria-label="The policy's parts" className="flex flex-wrap gap-1 border-b border-line">
      {parts.filter((p) => p.offered(rights)).map((p) => (
        <NavLink key={p.path} to={`/policy/${p.path}`}
          className={({ isActive }) => `px-4 py-2 text-sm font-semibold ${isActive ? 'border-b-2 border-ink' : 'text-ink-muted'}`}>
          {p.label}
        </NavLink>
      ))}
    </nav>
  )
}

function Frame({ title, lead, crumbs, children }: { title: string; lead?: string; crumbs?: { label: string; to?: string }[];
  children: React.ReactNode }) {
  return (
    <>
      {crumbs && <Crumbs items={crumbs} />}
      <Header title={title} lead={lead} />
      <PartsNav />
      {children}
    </>
  )
}

/** /policy: the first part the person reads */
export function PolicyHome() {
  const { rights } = useApp()
  const first = parts.find((p) => p.offered(rights))
  return first ? <Navigate to={`/policy/${first.path}`} replace /> : <Problem error={new Error('your rights read none of the policy')} />
}

export function Catalogs() {
  return (
    <Frame title="Policy" lead="The access-control policy, read on a node under your sign-in. What you see is what your rights read.">
      <ViewTable view="catalogs" title="Virtual catalogs"
        link={(c, row) => c === 'catalog' ? <Link className="link" to={`/policy/catalogs/${enc(String(row.catalog))}`}>{String(row.catalog)}</Link> : undefined} />
    </Frame>
  )
}

export function Catalog() {
  const { name = '' } = useParams()
  const { rights, api } = useApp()
  const [object, setObject] = useState<string>()
  const [checking, setChecking] = useState(false)
  const at = { catalog: name }
  return (
    <Frame title={name} crumbs={[{ label: 'Policy', to: '/policy/catalogs' }, { label: name }]}>
      {calls(rights, 'check_catalog') && (
        <div>
          <button type="button" className="btn-outline" onClick={() => setChecking(true)}>Check the catalog against its sources</button>
        </div>
      )}
      {checking && <CallTable title="What the check found" load={() => api.policyCall('check_catalog', { catalog: name })} />}
      {reads(rights, 'relations') && (
        <ViewTable view="relations" filters={at} title="Tables and views" columns={['name', 'form', 'phys', 'rls', 'comment']}
          link={(c, row) => c === 'name'
            ? <button type="button" className="link text-left" onClick={() => setObject(String(row.name))}>{String(row.name)}</button>
            : undefined} />
      )}
      {object && reads(rights, 'relation_columns') && (
        <>
          <ViewTable view="relation_columns" filters={{ ...at, relation: object }} title={`Declared columns of ${object}`}
            columns={['pos', 'name', 'expr', 'nullable']} />
          <p className="text-sm text-ink-muted">An object that declares no list exposes its source's columns, narrowed by each role's grant.</p>
        </>
      )}
      {reads(rights, 'schemas') && <ViewTable view="schemas" filters={at} title="Schemas" columns={['path', 'phys_path', 'origin', 'comment']} />}
      {reads(rights, 'functions') && <ViewTable view="functions" filters={at} title="Functions" columns={['name', 'kind', 'form', 'target', 'params', 'comment']} />}
      {reads(rights, 'references') && <ViewTable view="references" filters={at} title="References" columns={['name', 'from_object', 'to_object', 'cardinality', 'optional', 'comment']} />}
      {reads(rights, 'grants') && <ViewTable view="grants" filters={at} title="Catalog grants" columns={['role', 'is_main', 'caps', 'rls', 'columns']} />}
      {reads(rights, 'schema_grants') && <ViewTable view="schema_grants" filters={at} title="Schema grants" columns={['role', 'schema_path', 'caps', 'inherited', 'into', 'virtual_only']} />}
      {reads(rights, 'object_grants') && <ViewTable view="object_grants" filters={at} title="Object grants" columns={['role', 'object', 'caps', 'rls', 'columns']} />}
    </Frame>
  )
}

/** a read function's answer, when asked */
function CallTable({ title, load }: { title: string; load: () => ReturnType<ReturnType<typeof useApp>['api']['policyCall']> }) {
  const answer = useLoad(load, [])
  return (
    <section aria-label={title} className="flex flex-col gap-3">
      <h2 className="text-[18px] font-semibold leading-7">{title}</h2>
      {answer.error !== undefined && <Problem error={answer.error} />}
      {answer.loading && !answer.data && <p className="text-sm text-ink-muted">Reading on a node…</p>}
      {answer.data && <Rows page={answer.data} />}
    </section>
  )
}

export function Roles() {
  return (
    <Frame title="Policy">
      <ViewTable view="roles" title="Roles"
        link={(c, row) => c === 'role' ? <Link className="link" to={`/policy/roles/${enc(String(row.role))}`}>{String(row.role)}</Link> : undefined} />
    </Frame>
  )
}

export function Role() {
  const { name = '' } = useParams()
  const { rights, api } = useApp()
  const [catalog, setCatalog] = useState<string>()
  const of = { role: name }
  return (
    <Frame title={name} crumbs={[{ label: 'Policy', to: '/policy/roles' }, { label: name }]}>
      {reads(rights, 'grants') && (
        <ViewTable view="grants" filters={of} title="Catalog grants" columns={['catalog', 'is_main', 'caps', 'rls', 'columns']}
          link={(c, row) => c === 'catalog' && calls(rights, 'effective_rights')
            ? <button type="button" className="link text-left" onClick={() => setCatalog(String(row.catalog))}>{String(row.catalog)}</button>
            : undefined} />
      )}
      {catalog && (
        <CallTable key={catalog} title={`What ${name} reads in ${catalog}`}
          load={() => api.policyCall('effective_rights', { role: name, catalog })} />
      )}
      {calls(rights, 'effective_rights') && !catalog && reads(rights, 'grants') && (
        <p className="text-sm text-ink-muted">Pick a catalog to see what the role reads there, and which grant decides it.</p>
      )}
      {reads(rights, 'role_claims') && <ViewTable view="role_claims" filters={of} title="Default claims" columns={['claim', 'value']} />}
      {reads(rights, 'schema_grants') && <ViewTable view="schema_grants" filters={of} title="Schema grants" columns={['catalog', 'schema_path', 'caps', 'inherited']} />}
      {reads(rights, 'object_grants') && <ViewTable view="object_grants" filters={of} title="Object grants" columns={['catalog', 'object', 'caps', 'rls', 'columns']} />}
      {reads(rights, 'admins') && <ViewTable view="admins" filters={of} title="Administration" columns={['scope', 'catalog']} />}
      {reads(rights, 'platform_grants') && <ViewTable view="platform_grants" filters={of} title="Platform grants" columns={['object', 'kind', 'allowed']} />}
      {reads(rights, 'role_resource_groups') && <ViewTable view="role_resource_groups" filters={of} title="Resource groups" columns={['group']} />}
    </Frame>
  )
}

export function Identity() {
  const { rights } = useApp()
  return (
    <Frame title="Policy">
      <ViewTable view="issuers" title="Issuers" />
      {reads(rights, 'clients') && <ViewTable view="clients" title="Clients" />}
      {reads(rights, 'role_mappings') && <ViewTable view="role_mappings" title="Role mappings" />}
    </Frame>
  )
}

export function FunctionCategories() {
  const { rights } = useApp()
  return (
    <Frame title="Policy">
      <ViewTable view="function_categories" title="Function categories" />
      {reads(rights, 'function_category_members') && <ViewTable view="function_category_members" title="Members" />}
      {reads(rights, 'function_grants') && <ViewTable view="function_grants" title="Function grants" />}
    </Frame>
  )
}

export function ResourceGroups() {
  const { rights } = useApp()
  return (
    <Frame title="Policy">
      <ViewTable view="resource_groups" title="Resource groups" />
      {reads(rights, 'role_resource_groups') && <ViewTable view="role_resource_groups" title="Roles in them" />}
    </Frame>
  )
}

export function Sources() {
  const { api } = useApp()
  const sources = useLoad(() => api.policySources(), [api])
  const [source, setSource] = useState<string>()
  return (
    <Frame title="Policy" lead="The node's attached sources, and what of the policy exposes each: relations, aliases and every stored body that names it, per role.">
      {sources.error !== undefined && <Problem error={sources.error} />}
      {sources.loading && !sources.data && <p className="text-sm text-ink-muted">Reading on a node…</p>}
      {sources.data && (
        <ul className="flex flex-wrap gap-2">
          {sources.data.sources.map((s) => (
            <li key={s}><button type="button" className={source === s ? 'btn-primary' : 'btn-outline'} onClick={() => setSource(s)}>{s}</button></li>
          ))}
          {sources.data.sources.length === 0 && <li className="text-sm text-ink-muted">The node has no source attached.</li>}
        </ul>
      )}
      {source && <CallTable key={source} title={`What exposes ${source}`} load={() => api.policyCall('exposures', { source })} />}
    </Frame>
  )
}
