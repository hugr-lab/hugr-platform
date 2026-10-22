// The policy, read (spec 007 stage 1): the pages follow the DBeaver client's tree - catalogs, roles, the identity, the
// function categories, the resource groups, the sources. Each part is offered by what the person reads.
import { useState, type ReactNode } from 'react'
import { Link, NavLink, Navigate, useParams } from 'react-router-dom'
import { Crumbs, Header, Problem } from '../components/ui'
import { Rows, Section, ViewTable } from '../components/ViewTable'
import { useApp, useLoad } from '../lib/context'
import { calls, caps, policyParts, reads } from '../lib/policy'
import type { ViewPage } from '../lib/types'

const enc = encodeURIComponent

/** a catalog grant's caps: NULL is every data capability; an object grant's: NULL inherits the catalog grant's */
const grantCaps = (column: string, value: unknown) =>
  column === 'caps' ? caps(value, 'unstated: every data capability') : undefined
const objectCaps = (column: string, value: unknown) =>
  column === 'caps' ? caps(value, "inherited: the catalog grant's") : undefined
const answerCaps = (column: string, value: unknown) => (column === 'caps' ? caps(value, '-') : undefined)

function PartsNav() {
  const { rights } = useApp()
  return (
    <nav aria-label="The policy's parts" className="flex flex-wrap gap-1 border-b border-line">
      {policyParts.filter((p) => p.offered(rights)).map((p) => (
        <NavLink key={p.path} to={`/policy/${p.path}`}
          className={({ isActive }) => `px-4 py-2 text-sm font-semibold ${isActive ? 'border-b-2 border-ink' : 'text-ink-muted'}`}>
          {p.label}
        </NavLink>
      ))}
    </nav>
  )
}

function Frame({ title, lead, crumbs, children }: { title: string; lead?: string; crumbs?: { label: string; to?: string }[];
  children: ReactNode }) {
  return (
    <>
      <Crumbs items={crumbs ?? [{ label: 'Policy' }]} />
      <Header title={title} lead={lead} />
      <PartsNav />
      {children}
    </>
  )
}

/** /policy: the first part the person reads */
export function PolicyHome() {
  const { rights } = useApp()
  const first = policyParts.find((p) => p.offered(rights))
  return first ? <Navigate to={`/policy/${first.path}`} replace /> : <Problem error={new Error('your rights read none of the policy')} />
}

/** a part's page, only when the person reads it: otherwise the first part they do */
export function Part({ path, children }: { path: string; children: ReactNode }) {
  const { rights } = useApp()
  const part = policyParts.find((p) => p.path === path)
  return part?.offered(rights) ? <>{children}</> : <PolicyHome />
}

/** a page of one object (a catalog, a role): drawn anew for another, so nothing picked on one shows on the next */
export function Keyed({ page: Page }: { page: (props: { name: string }) => ReactNode }) {
  const { name = '' } = useParams()
  return <Page key={name} name={name} />
}

export function Catalogs() {
  return (
    <Frame title="Catalogs" lead="The access-control policy, read on a node under your sign-in. What you see is what your rights read.">
      <ViewTable view="catalogs" title="Virtual catalogs"
        link={(c, row) => c === 'catalog' ? <Link className="link" to={`/policy/catalogs/${enc(String(row.catalog))}`}>{String(row.catalog)}</Link> : undefined} />
    </Frame>
  )
}

/** a button that picks one of a list: pressed while picked */
function Pick({ name, picked, onPick }: { name: string; picked: boolean; onPick: () => void }) {
  return <button type="button" className="link text-left" aria-pressed={picked} onClick={onPick}>{name}</button>
}

export function Catalog({ name }: { name: string }) {
  const { rights, api } = useApp()
  const [object, setObject] = useState<string>()
  const [fn, setFn] = useState<string>()
  const [checks, setChecks] = useState(0)
  const at = { catalog: name }
  return (
    <Frame title={name} crumbs={[{ label: 'Policy', to: '/policy/catalogs' }, { label: name }]}>
      {calls(rights, 'check_catalog') && reads(rights, 'catalogs') && reads(rights, 'grants') && (
        <div>
          <button type="button" className="btn-outline" onClick={() => setChecks((n) => n + 1)}>
            {checks ? 'Check again' : 'Check the catalog against its sources'}
          </button>
        </div>
      )}
      {checks > 0 && <CallTable key={checks} title="What the check found" load={() => api.policyCall('check_catalog', { catalog: name })} />}
      {reads(rights, 'relations') && (
        <ViewTable view="relations" filters={at} title="Tables and views" columns={['name', 'form', 'phys', 'rls', 'comment']}
          link={(c, row) => c === 'name' ? <Pick name={String(row.name)} picked={object === row.name} onPick={() => setObject(String(row.name))} /> : undefined} />
      )}
      {object && reads(rights, 'relation_columns') && (
        <>
          <ViewTable key={object} view="relation_columns" filters={{ ...at, relation: object }} title={`Declared columns of ${object}`}
            columns={['pos', 'name', 'expr', 'nullable']} />
          <p className="text-sm text-ink-muted">An object that declares no list exposes its source's columns, narrowed by each role's grant.</p>
        </>
      )}
      {reads(rights, 'keys') && <ViewTable view="keys" filters={at} title="Keys" columns={['object', 'kind', 'pos', 'column']} />}
      {reads(rights, 'schemas') && <ViewTable view="schemas" filters={at} title="Schemas" columns={['path', 'phys_path', 'origin', 'comment']} />}
      {reads(rights, 'functions') && (
        <ViewTable view="functions" filters={at} title="Functions" columns={['name', 'kind', 'form', 'target', 'params', 'comment']}
          link={(c, row) => c === 'name' && reads(rights, 'function_columns')
            ? <Pick name={String(row.name)} picked={fn === row.name} onPick={() => setFn(String(row.name))} /> : undefined} />
      )}
      {fn && <ViewTable key={fn} view="function_columns" filters={{ ...at, function: fn }} title={`Parameters and columns of ${fn}`}
        columns={['kind', 'pos', 'name', 'type', 'nullable', 'comment']} />}
      {reads(rights, 'references') && <ViewTable view="references" filters={at} title="References" columns={['name', 'from_object', 'to_object', 'cardinality', 'optional', 'comment']} />}
      {reads(rights, 'grants') && <ViewTable view="grants" filters={at} title="Catalog grants" columns={['role', 'is_main', 'caps', 'rls', 'columns']} format={grantCaps} />}
      {reads(rights, 'schema_grants') && <ViewTable view="schema_grants" filters={at} title="Schema grants" columns={['role', 'schema_path', 'caps', 'inherited', 'into', 'virtual_only']} format={objectCaps} />}
      {reads(rights, 'object_grants') && <ViewTable view="object_grants" filters={at} title="Object grants" columns={['role', 'object', 'caps', 'rls', 'columns']} format={objectCaps} />}
      {reads(rights, 'grant_columns') && <ViewTable view="grant_columns" filters={at} title="What each grant's projection produces" columns={['role', 'object', 'pos', 'name', 'type']} />}
    </Frame>
  )
}

/** a read function's answer, when asked: all of it up to the server's page, the cut said */
function CallTable({ title, load }: { title: string; load: () => Promise<ViewPage> }) {
  const answer = useLoad(load, [])
  return (
    <Section title={title}>
      {answer.error !== undefined && <Problem error={answer.error} />}
      {answer.loading && !answer.data && <p className="text-sm text-ink-muted">Reading on a node…</p>}
      {answer.data && <Rows page={answer.data} format={answerCaps} />}
      {answer.data && answer.data.total > answer.data.rows.length && (
        <p className="text-sm text-ink-muted">The first {answer.data.rows.length} of {answer.data.total} rows.</p>
      )}
    </Section>
  )
}

export function Roles() {
  return (
    <Frame title="Roles">
      <ViewTable view="roles" title="Roles"
        link={(c, row) => c === 'role' ? <Link className="link" to={`/policy/roles/${enc(String(row.role))}`}>{String(row.role)}</Link> : undefined} />
    </Frame>
  )
}

export function Role({ name }: { name: string }) {
  const { rights, api } = useApp()
  const [catalog, setCatalog] = useState<string>()
  const of = { role: name }
  // the node checks the arguments against these lists: offered only to whom reads them
  const effective = calls(rights, 'effective_rights') && reads(rights, 'roles') && reads(rights, 'catalogs') &&
    reads(rights, 'grants')
  return (
    <Frame title={name} crumbs={[{ label: 'Policy', to: '/policy/roles' }, { label: name }]}>
      {reads(rights, 'grants') && (
        <ViewTable view="grants" filters={of} title="Catalog grants" columns={['catalog', 'is_main', 'caps', 'rls', 'columns']} format={grantCaps}
          link={(c, row) => c === 'catalog' && effective
            ? <Pick name={String(row.catalog)} picked={catalog === row.catalog} onPick={() => setCatalog(String(row.catalog))} />
            : undefined} />
      )}
      {catalog && (
        <CallTable key={catalog} title={`What ${name} reads in ${catalog}`}
          load={() => api.policyCall('effective_rights', { role: name, catalog })} />
      )}
      {effective && !catalog && reads(rights, 'grants') && (
        <p className="text-sm text-ink-muted">Pick a catalog to see what the role reads there, and which grant decides it.</p>
      )}
      {reads(rights, 'role_claims') && <ViewTable view="role_claims" filters={of} title="Default claims" columns={['claim', 'value']} />}
      {reads(rights, 'schema_grants') && <ViewTable view="schema_grants" filters={of} title="Schema grants" columns={['catalog', 'schema_path', 'caps', 'inherited']} format={objectCaps} />}
      {reads(rights, 'object_grants') && <ViewTable view="object_grants" filters={of} title="Object grants" columns={['catalog', 'object', 'caps', 'rls', 'columns']} format={objectCaps} />}
      {reads(rights, 'admins') && <ViewTable view="admins" filters={of} title="Administration" columns={['scope', 'catalog']} />}
      {reads(rights, 'platform_grants') && <ViewTable view="platform_grants" filters={of} title="Platform grants" columns={['object', 'kind', 'allowed']} />}
      {reads(rights, 'role_resource_groups') && <ViewTable view="role_resource_groups" filters={of} title="Resource groups" columns={['group']} />}
    </Frame>
  )
}

export function Identity() {
  const { rights } = useApp()
  return (
    <Frame title="Identity">
      {reads(rights, 'issuers') && <ViewTable view="issuers" title="Issuers" />}
      {reads(rights, 'clients') && <ViewTable view="clients" title="Clients" />}
      {reads(rights, 'role_mappings') && <ViewTable view="role_mappings" title="Role mappings" />}
    </Frame>
  )
}

export function FunctionCategories() {
  const { rights } = useApp()
  return (
    <Frame title="Functions">
      {reads(rights, 'function_categories') && <ViewTable view="function_categories" title="Function categories" />}
      {reads(rights, 'function_category_members') && <ViewTable view="function_category_members" title="Members" />}
      {reads(rights, 'function_grants') && <ViewTable view="function_grants" title="Function grants" />}
    </Frame>
  )
}

export function ResourceGroups() {
  const { rights } = useApp()
  return (
    <Frame title="Resource groups">
      {reads(rights, 'resource_groups') && <ViewTable view="resource_groups" title="Resource groups" />}
      {reads(rights, 'role_resource_groups') && <ViewTable view="role_resource_groups" title="Roles in them" />}
    </Frame>
  )
}

export function Sources() {
  const { api } = useApp()
  const sources = useLoad(() => api.policySources(), [api])
  const [source, setSource] = useState<string>()
  return (
    <Frame title="Sources" lead="The node's attached sources, and what of the policy exposes each: relations, aliases and every stored body that names it, per role.">
      {sources.error !== undefined && <Problem error={sources.error} />}
      {sources.loading && !sources.data && <p className="text-sm text-ink-muted">Reading on a node…</p>}
      {sources.data && (
        <ul className="flex flex-wrap gap-2">
          {sources.data.sources.map((s) => (
            <li key={s}>
              <button type="button" aria-pressed={source === s} className={source === s ? 'btn-primary' : 'btn-outline'}
                onClick={() => setSource(s)}>{s}</button>
            </li>
          ))}
          {sources.data.sources.length === 0 && <li className="text-sm text-ink-muted">The node has no source attached.</li>}
        </ul>
      )}
      {source && <CallTable key={source} title={`What exposes ${source}`} load={() => api.policyCall('exposures', { source })} />}
    </Frame>
  )
}
