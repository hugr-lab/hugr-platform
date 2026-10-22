// The console's API (spec 003), as internal/console answers it.

export interface Link {
  kind: 'telemetry' | 'lineage'
  name: string
  url: string
}

export interface Mount {
  base: string
  audience?: string
  tenant?: string
}

export interface Config {
  platform: string
  issuer: string
  clientID: string
  scopes: string[]
  audiences: { nodes: string; tresor: string; kista?: string }
  mounts: Record<string, Mount>
  links: Link[]
}

export interface Rights {
  fleet: boolean
  sessions: boolean
  endSession: boolean
  drain: boolean
  resume: boolean
  tracing: boolean
  scale: boolean
  /** a group's place in the policy (spec 006): the policy bundle's, the node judges each write again */
  policy: boolean
  infrastructure: boolean
  events: boolean
  /** a node against the cluster's profile, the profile's items, a node's items: a cluster node's views only */
  drift: boolean
  profile: boolean
  effective: boolean
  /** "cluster" (hugr-node's) or "standalone", as the node said */
  deployment: string
  bundles: string[]
  /** the platform views and functions the person reads and calls (console_info's my_views, my_functions): spec 007 */
  views: string[]
  functions: string[]
}

/** a page of a policy view's rows, or a read function's answer (spec 007) */
export interface ViewPage {
  columns: string[]
  rows: Record<string, unknown>[]
  total: number
  /** the view held more than the console reads: total counts what it read */
  truncated?: boolean
}

export interface NodeInfo {
  label: string
  ready: boolean
  serving: boolean
  leaseAge: number | null
  current: boolean
  sessions: number
  seatsLeft: number
  admitting: boolean
  draining: boolean
  target: number
  applied: number
  needsRestart: boolean
  usage: Usage | null
  /** how far its pod has got; null when the pods were not read */
  pod: PodState | null
}

/** a node's pod as Kubernetes says it: made and not serving yet, serving, going away, or not starting as it is */
export interface PodState {
  state: 'starting' | 'running' | 'stopping' | 'failing' | 'ended'
  phase: string
  reason?: string
  message?: string
  restarts: number
  created?: string
}

export interface Usage {
  cpuMilli: number
  memoryBytes: number
}

export interface Identity {
  serviceAccount: string
  subject: string
  clientID: string
  tresor: string
  tlsName: string
}

export interface Condition {
  type: string
  status: string
  reason: string
  message: string
}

export interface Group {
  name: string
  size: string
  replicas: number
  readyReplicas: number
  resourceGroup: string
  resourceGroupKnown: boolean | null
  profile: { target: number; applied: number; needsRestart: string[] }
  lost: string[]
  restartedAt?: string
  conditions: Condition[]
  scaling: { mode: 'manual' | 'auto'; min?: number; max?: number }
  identity: Identity
  memoryLimitBytes: number
  /** the quack door beside Flight, and a stopping node's wait for its sessions (spec 006) */
  quack: boolean
  drainSeconds: number
  /** what the group's Deployment says stops its pods */
  problem?: string
  nodes: NodeInfo[]
}

export interface Fleet {
  plan: { maxNodes: number; sizes: string[] }
  groups: Group[]
  /** whether the console makes a group, and the subject a new group's identity trusts (its prefix) */
  newGroup: { available: boolean; subjectPrefix: string }
}

/** `POST groups` (spec 006): the identity is given - an existing one's client id */
export interface NewGroup {
  name: string
  size: string
  replicas: number
  clientID: string
  quack: boolean
  drainSeconds?: number
  resourceGroup?: string
}

/** one field of a group a `PATCH groups/{g}` changes */
export type GroupPatch = { replicas: number } | { restart: true } | { size: string } | { quack: boolean } |
  { drainSeconds: number }

/** a group's place in the policy, as a node answers it for the person */
export interface GroupPolicy {
  resourceGroup: string
  inPolicy: boolean
  default: boolean
  roles: string[]
  bound: string[]
}

export interface NodePage {
  kubernetes: NodeInfo
  group: string
  load: Record<string, unknown> | null
  loadError?: string
}

export interface SessionRow {
  id: string
  subject: string
  roles: string[] | null
  door: string
  idle_seconds: number | null
  expires_at: string | null
  audit_level: string | null
  audit_level_source: string | null
  profile_level: string | null
  profile_source: string | null
  groups: string[] | null
  charged_group: string | null
}

/** one read of a page made of several: its rows, or what the node said instead */
export interface Part {
  rows: Record<string, unknown>[]
  error?: string
}

export interface PodPage {
  phase: string
  startedAt: string | null
  nodeName: string
  podIP: string
  image: string
  imageID: string
  restarts: number
  lastTermination: { reason: string; exitCode: number; finishedAt: string } | null
  requests: Usage
  limits: Usage
  usage: Usage | null
  usageError?: string
  /** the metrics API has not sampled the pod yet: asked again shortly */
  usagePending?: boolean
  certificateExpiry: string | null
  events: { type: string; reason: string; message: string; count: number; last: string }[]
}

export interface MemoryPage {
  tags: Part
  spillBytes: number | null
  limits: Part
  error?: string
}

/** what a node of a group is handed at its entry (spec 010): the operator's document, as the node receives it */
export interface BootstrapDocument {
  version: number
  node_group: string
  node_label: string
  tresor: { host: string; identity: { issuer: string; client_id: string; scope?: string } }
  repository: { name: string; prefix: string }
  catalog: { secret: string; schema: string; migrate?: boolean }
  heartbeat: { interval_seconds: number }
  first_policy?: {
    issuer: { name: string; url: string; audiences: string[]; roles_claim: string }
    clients: { name: string; client_id: string }[]
    administrators: string
    resource_groups: { name: string; default?: boolean }[]
  }
}

/** one change of the cluster's profile (spec 010 stage 2): a CLUSTER statement's fields; group scopes it to a resource group */
export interface ProfileChange {
  action: 'install' | 'update' | 'remove' | 'attach' | 'detach' | 'set' | 'reset'
  extension?: string
  version?: string
  repository?: string
  path?: string
  alias?: string
  type?: string
  secret?: string
  readOnly?: boolean
  lineage?: string
  dependsOn?: string[]
  cascade?: boolean
  force?: boolean
  setting?: string
  value?: string
  group?: string
  comment?: string
}

/** what the node did: the profile's version after it, how the change reaches the nodes, the writing node */
export interface ProfileWritten {
  version: number
  class: string
  appliedHere: boolean
  note: string
  node: string
}

export interface BootstrapPage {
  document: BootstrapDocument
  /** false: the operator refuses it now, and a node of the group does not start; refused says why */
  served: boolean
  refused?: string
}

export interface InventoryPage {
  extensions: Part
  drift: Part
  effective: Part
}

export interface AuditEvent {
  ts: string
  seq: number
  kind: string
  level: string | null
  door: string | null
  session: string | null
  subject: string | null
  roles: string[] | null
  statement: string | null
  verdict: string | null
  reason_code: string | null
  rows: number | null
  duration_us: number | null
  correlation_id: string | null
  traceparent: string | null
  detail: string | null
}

export interface ClusterPage {
  profile: Part
  lineage: { node: string; row?: Record<string, unknown>; error?: string }[]
  links: Link[]
}
