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
