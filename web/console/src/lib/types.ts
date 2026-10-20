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
  audiences: { nodes: string; tresor?: string; kista?: string }
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
  infrastructure: boolean
  events: boolean
  /** a node against the cluster's profile, the profile's items, a node's items: a cluster node's views only */
  drift: boolean
  profile: boolean
  effective: boolean
  /** "cluster" (hugr-node's) or "standalone", as the node said */
  deployment: string
  bundles: string[]
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
  /** what the group's Deployment says stops its pods */
  problem?: string
  nodes: NodeInfo[]
}

export interface Fleet {
  plan: { maxNodes: number; sizes: string[] }
  groups: Group[]
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
