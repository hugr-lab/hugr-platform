// A group's bootstrap document as a person reads it (spec 010): its parts, each a label and a value, in the order the
// node uses them - where it logs in, what it trusts, its catalog, how often it reports, the policy it may write.
import type { BootstrapPage, BootstrapDocument, NodeInfo } from './types'

export interface DocumentPart {
  title: string
  note?: string
  rows: [string, string][]
}

export function documentParts(d: BootstrapDocument): DocumentPart[] {
  const parts: DocumentPart[] = [
    { title: 'Secrets service', note: 'where the node logs in, and as whom: the group\'s workload identity', rows: [
      ['tresor', d.tresor.host],
      ['Identity provider', d.tresor.identity.issuer],
      ['Client id', d.tresor.identity.client_id],
      ...(d.tresor.identity.scope ? [['Scope', d.tresor.identity.scope] as [string, string]] : []),
    ] },
    { title: 'Extension repository', note: 'the node trusts it by its discovery document', rows: [
      ['Name', d.repository.name],
      ['Prefix', d.repository.prefix],
    ] },
    { title: 'Policy catalog', note: 'its connection is the secret of this name in tresor', rows: [
      ['Secret', d.catalog.secret],
      ['Schema', d.catalog.schema],
      ...(d.catalog.migrate ? [['Migration', 'pending: this group is the rollout\'s canary'] as [string, string]] : []),
    ] },
    { title: 'Heartbeat', rows: [['Every', `${d.heartbeat.interval_seconds} s`], ['Resource group', d.node_group]] },
  ]
  const p = d.first_policy
  if (p) {
    parts.push({
      title: 'First policy',
      note: 'written once, by the first node to attach a catalog nobody administers yet; a catalog with its policy keeps its own',
      rows: [
        ['Issuer', `${p.issuer.name}: ${p.issuer.url}`],
        ['Audiences', p.issuer.audiences.join(', ')],
        ['Roles claim', p.issuer.roles_claim],
        ['Clients', p.clients.map((c) => `${c.name} (${c.client_id})`).join(', ')],
        ['Administrators', p.administrators],
        ['Resource groups', p.resource_groups.map((g) => (g.default ? `${g.name} (default)` : g.name)).join(', ')],
      ],
    })
  }
  return parts
}

/** the JSON's title: what a node receives, or - refused - what the operator would hand one and does not */
export function documentTitle(page: BootstrapPage): string {
  return page.served ? 'The document as a node receives it' : 'The document the operator would hand a node - refused now'
}

/** the group's nodes that applied an older profile than the one written, each with the version it applied */
export function behind(nodes: NodeInfo[], target: number): string[] {
  return nodes.filter((n) => n.leaseAge !== null && n.applied < target).map((n) => `${n.label} (version ${n.applied})`)
}
