# Spec 003: the console - one place to run the platform

- **Status**: accepted (the owner, 2026-10-19: Q1-Q5 as recommended); the look is made with Claude Design on the Hugr Lab
  design system (hugr-lab.com)
- **Date**: 2026-10-19

## Summary

The console is the platform's one web interface. The operator serves it. It is a shell: it owns the sign-in, the
navigation and the theme, and it mounts tresor-server's and kista's consoles through their micro-frontend contracts.
Its own pages are the fleet: the groups and their nodes, what each node holds, and what a person may do to them -
scale or restart a group, drain or resume a node, end a session. Every node-side action carries the person's own
token, so the ACL judges it, as spec 001 decided. The pages a person sees are the ones their ACL rights admit (design/082
C3); the fleet's state comes from Kubernetes (C1). The policy is not edited here: that is SQL and the DBeaver plugin.

## Problem

- **No interface.** An administrator runs the platform with `kubectl`, the nodes' SQL and two separate consoles
  (tresor-server's, kista's), each with its own sign-in and address.
- **The fleet's state is in pieces**: a group's status in its `HugrNodeGroup`, a node's load in its Lease, its
  sessions behind its door. Nothing shows them together.
- **A person's actions on a node** (kill a session, drain) need a Flight SQL client and the person's token. A browser
  speaks neither gRPC nor Flight.
- **Who may do what** is the ACL's (bundles `observe`, `operate`, `cluster`, `policy` - duckdb-acl spec 117/118). A
  console that decided by itself would be a second judge (spec 001).

## Design

### Where it runs

- **The operator serves it**, on every replica (the leader only reconciles): a listener of its own (`:8444`), its own
  public certificate (the Secret `hugr-console`, the administrator's or a cert-manager Certificate's - as the front's,
  spec 002), and a Service `hugr-platform-console` in `config/manager`. Clients outside the cluster reach it through
  the installation's Ingress or load balancer (the packaging spec); the node API's port and certificate stay the
  nodes' alone.
- **The web build is embedded** in the operator's binary (`//go:embed`), built by a Node stage in the image's
  Dockerfile. A binary built without Node serves a placeholder page that says so (kista's rule, its spec 0015).
- **The stack is tresor-server's and kista's**: React 18, TypeScript, Vite, Tailwind 3, react-router-dom 7,
  lucide-react, oidc-client-ts; Manrope and JetBrains Mono self-hosted; no component library (native `<dialog>`s); a
  strict CSP (no inline script, no `data:` fonts); a licence gate (MIT, ISC, Apache-2.0, BSD, OFL). Source in
  `web/console/`.

### Sign-in and tokens

- **The browser signs the person in** at the organisation's IdP (`spec.idp.issuer`): authorization code with PKCE,
  oidc-client-ts, the console's own public client (`spec.console.clientID`). The console keeps tokens in memory only;
  a reload signs in again silently (the IdP's session).
- **A token per audience**, as tresor's and kista's contracts ask (`getToken(audience, {renew})`):
  - **the nodes'** (`spec.console.audiences.nodes`, the audience of the policy's client for the console, duckdb-acl
    spec 095): the console's own API and every node-side action;
  - **tresor-server's** and **kista's** for their mounts.
  - Entra: one token per resource, by the refresh token (`acquireTokenSilent` semantics: scope `<api>/.default`).
    Keycloak: audience mappers put the audiences in one token, or token exchange.
- **No second sign-in**, no token stored by the operator, none logged, none in a URL.

### Who may see and do what (C3)

- **The ACL decides, once per person, cached.** The operator asks a ready node `SELECT * FROM platform.console_info()`
  with the person's token: the answer's `bundles` and `my_views` / `my_functions` say what this person holds. Cached
  60 s by the token's hash (never the token); a refused token is the sign-in's problem (401).
- **A page shows when its reads are the person's**, an action when its call is:

  | page or action | needs |
  | --- | --- |
  | the fleet: groups, nodes, their Leases and status | `observe` (`platform.node_load` readable) |
  | a node's sessions | `observe` (`platform.sessions`) |
  | end a session, drain or resume a node | `operate` (`platform.kill_session`, `drain`, `resume`) |
  | scale a group, restart a group | `cluster` (the infrastructure's right, duckdb-acl spec 118) |
  | secrets | tresor-server's own `whoami` (`permissions.create`) |
  | extensions | kista's own `whoami` (`administrator`, or `holds_admin` on the tenant) |

- **The API checks the same table** on every call: hiding a button is a convenience, the check is the operator's (for
  the Kubernetes writes) and the node's (for its own actions, which it judges again).
- **No node answers**: the console cannot learn anyone's rights, so it shows nothing but that, and says the cluster's
  state is `kubectl`'s (fail closed).

### Pages (C1)

- **Fleet** - one row per group: its size, replicas wanted / ready, the resource group it serves and whether the
  policy knows it, the profile's target and applied version, a restart pending, draining. Actions: scale (within the
  plan: the webhook refuses past it, with its reason), restart (the group's `restartedAt`).
- **Group** - one row per node: label, ready, the Lease's age, sessions, seats left, admitting, the image's version.
- **Node** - its load report (`platform.node_load`, read with the person's token), its sessions (`platform.sessions`:
  principal, door, opened, idle, groups) with "end session"; drain / resume.
- **Secrets** - tresor-server's console, mounted. **Extensions** - kista's, mounted with the tenant the repository's
  prefix names.
- A page reads when it opens and on the person's refresh; nothing polls in the background (tresor's rule).

### The console's API (the operator)

Under `/api/v1/console/`, the person's nodes-audience token as the bearer on every call:

| method, path | does | right |
| --- | --- | --- |
| `GET me` | the person's rights (the table above), the platform's name, the mounts' bases and audiences | any signed-in |
| `GET groups` | the groups with their status and nodes (Kubernetes: the groups, their Leases) | observe |
| `PATCH groups/{g}` `{replicas}` / `{restart:true}` | writes the `HugrNodeGroup` as the operator (SSA, its field owner) | cluster |
| `GET nodes/{label}` | the node's load report, read on that node with the person's token | observe |
| `GET nodes/{label}/sessions` | `platform.sessions` on that node | observe |
| `POST nodes/{label}/sessions/{id}/end` | `KILL SESSION '<id>'` on that node | operate |
| `POST nodes/{label}/drain` / `resume` | `DRAIN NODE` / `RESUME NODE` on that node | operate |
| `POST sign-out` | `CloseSession` on every node holding one of this person's sessions | any signed-in |

- An answer carries no token, no session handle (`platform.sessions` shows the ops id, never the handle), no
  connection id, and a node's refusal only as its message.
- A Kubernetes write names the person in its audit: an annotation `platform.hugr.io/changed-by` (the person's `sub` at
  the IdP, never a token) on the group, and the operator's log line.

### Talking to the nodes (spec 001's pool, now built)

- **Flight SQL, directly to the node** a page names (the console's calls are about one node), over the group's TLS
  name and the platform's CA bundle (the front's dialing, spec 002).
- **One gRPC channel per node; one session cookie per node and person**: the first call opens the person's session,
  later ones reuse it (duckdb-acl spec 050). Each call carries the person's bearer, and `traceparent`.
- **Sign-out** ends the person's sessions on every node holding one; otherwise the node's idle timeout does.
- Statements are fixed texts; an id from the browser (a session's ops id) is bound as a constant only after it
  matches the ops id's form - never SQL from the browser.

### tresor-server and kista

- **Same origin, proxied**: `/tresor/…` to `spec.tresor.host`, `/kista/…` to the repository prefix's host - their
  contracts' recommended delivery (no CORS; the console's CSP allows itself). The proxy forwards the request as it
  came (the person's bearer for that service included), adds nothing, follows no redirect.
- The modules are imported from their own servers (`/tresor/ui/mfe/tresor.js`, `/kista/ui/mfe/kista.js`); their
  `contract` must be 1, else the entry says the component is too new or too old.
- `onUnauthorized` from either: the console renews the token once, then signs the person in again by a popup.

### Configuration

`HugrPlatform.spec.console`: `clientID` (the IdP's public client), `audiences` (`nodes`, `tresor`, `kista`),
`scopes`; the issuer is `spec.idp.issuer`. The console's public certificate is the operator's Deployment's
(`hugr-console`).

## Security

- **The ACL is the only judge** of node-side rights and of the fleet's pages (C3); the operator executes the
  Kubernetes writes a `cluster` right asked for, and nothing else of its own initiative.
- **No credential at rest**: tokens live in the browser's memory; the operator keeps a token's hash for the rights
  cache and the session cookies the nodes gave (in memory, per replica), never a token.
- **Nothing from the browser becomes SQL**: fixed statements, ids checked by form.
- **The proxy** to tresor and kista forwards the person's bearer to the service it is meant for (the audience is the
  service's), never another's.
- **CSP**: `default-src 'self'`; the IdP in `connect-src` (token endpoint) and `frame-src` (silent renew), nothing
  else.

## Testing

- **Unit (Go)**: the rights table against `console_info` answers; the API's checks (each route refused without its
  right); the pool's per-person cookies; the proxy (no redirect, nothing added); the ops id's form.
- **Web**: Vitest for the token per audience and the pages' gating; Playwright against the operator with a fake IdP
  and fake nodes.
- **The demo** (spec 004): the console's API through the person's token - Fleet, a node's sessions, end a session,
  drain and resume, scale a group - and tresor's and kista's mounts shown.

## Alternatives considered

- **A console of its own Deployment**: one more thing to run and secure; the operator already holds the fleet's state
  and the nodes' dialing (owner, 2026-10-17: the operator serves it).
- **The operator's identity for every call plus a trace** - two judges (spec 001).
- **The browser talking to the nodes** through a gRPC-web gateway: a second public door to every node.
- **A backend session (the operator holding the person's tokens)**: tresor's and kista's contracts expect
  `getToken` in the browser; a server-side token store is a credential at rest.

## Decisions (owner, 2026-10-19): Q1-Q5 as recommended below

**Q1. How the console is reached.** *Recommendation:* its own listener, Service and public certificate on the operator;
the installation's Ingress or load balancer in front (packaging). Alternative: through the front, as a third door.

**Q2. Rights when no node answers.** *Recommendation:* fail closed - the console shows that no node answers and points
to `kubectl`. Alternative: an IdP role (`platform-admin`) checked by the operator itself gives the fleet's read-only
pages without a node - a second judge, but the one view that helps most when every node is down.

**Q3. Sign-in.** *Recommendation:* the browser (PKCE, oidc-client-ts), a token per audience in memory. Alternative:
the operator as a backend holding the tokens (rejected above: tresor's and kista's `getToken` contract, a credential at
rest).

**Q4. The pages of v1.** *Recommendation:* Fleet, Group, Node (load, sessions, end, drain / resume), scale and restart,
tresor's and kista's mounts. Later: the audit's history (C2), a client's workload tag (C4), the drift view
(`platform.drift`), the profile's pending items.

**Q5. Who may scale and restart.** *Recommendation:* the `cluster` bundle (the infrastructure's right, duckdb-acl spec
118). Alternative: `operate`.

## Follow-ups

- The packaging spec: the console's Ingress / load balancer, the IdP registrations (the console's client, the API
  permissions on tresor's and kista's resources), the Managed Application's own.
- C2 and C4 (design/082), when the audit's storage is decided.
- Languages: tresor's and kista's `locale`, once the console has more than English.
