import { useState } from 'react'
import { ArrowRight, RefreshCw, LogOut } from 'lucide-react'
import logo from '../assets/hugr-logo.svg'
import logoOnDark from '../assets/hugr-logo-on-dark.svg'
import type { Session } from '../lib/auth'
import type { Config } from '../lib/types'

/** the sign-in: the IdP signs the person in; the console keeps the token in this tab only */
export function SignIn({ config, session }: { config: Config; session: Session }) {
  const [failed, setFailed] = useState<string>()
  const issuer = (() => {
    try {
      return new URL(config.issuer).host
    } catch {
      return config.issuer
    }
  })()
  return (
    <div className="relative flex min-h-screen flex-col items-center justify-center gap-8 overflow-hidden bg-[var(--surface-soft)] px-4 py-16">
      <main className="relative flex w-full max-w-[440px] flex-col gap-8 rounded-lg bg-surface px-10 py-12">
        <div className="flex flex-col gap-4">
          <img src={logo} alt="hugr" width={144} className="h-auto w-36" />
          <div className="eyebrow">Console</div>
        </div>
        <div className="flex flex-col gap-2">
          <h1 className="text-[32px] font-semibold leading-10 tracking-[-0.01em]">Sign in to {config.platform}</h1>
          <p className="text-ink-muted">You sign in at your organisation's identity provider. The console keeps your token in this tab only; closing it signs you out here.</p>
        </div>
        <div className="flex flex-col items-start gap-4">
          <button type="button" className="btn-primary min-h-12 px-6 text-base"
            onClick={() => {
              setFailed(undefined)
              session.signIn(window.location.pathname).catch((e: unknown) => setFailed(unreachable(e)))
            }}>
            Sign in <ArrowRight size={20} aria-hidden="true" />
          </button>
          <span className="text-sm text-ink-muted">Identity provider <span className="mono text-ink">{issuer}</span></span>
          {failed && <p role="alert" className="text-sm">{failed}</p>}
        </div>
        <p className="border-t border-line pt-6 text-sm text-ink-muted">What you see and may do here comes from the platform's policy. The console decides nothing by itself.</p>
      </main>
      <p className="relative text-sm text-ink-muted">hugr · Hugr Lab</p>
    </div>
  )
}

/** fail closed (spec 003 Q2): no node answers, so the console cannot learn the person's rights */
export function NoNode({ config, session, onRetry }: { config: Config; session: Session; onRetry: () => void }) {
  return (
    <div className="flex min-h-screen flex-wrap items-stretch">
      <nav aria-label="Console" className="flex flex-[1_1_248px] flex-col gap-8 bg-navy px-4 py-8 text-[#e8f1f2]">
        <div className="flex flex-col gap-2 px-4">
          <img src={logoOnDark} alt="hugr" width={112} className="h-auto w-28" />
          <div className="text-[13px] font-semibold uppercase leading-5 tracking-[0.08em] text-navy-muted">Console · {config.platform}</div>
        </div>
        <p className="px-4 text-sm text-navy-muted">No pages: your rights are unknown until a node answers.</p>
        <div className="mt-auto rounded-md border border-navy-line p-4">
          <button type="button" className="btn-dark w-full" onClick={() => session.signOut()}>
            <LogOut size={18} aria-hidden="true" /> Sign out
          </button>
        </div>
      </nav>
      <main className="flex min-w-0 flex-[999_1_560px] flex-col items-start gap-8 px-[clamp(16px,7vw,128px)] pb-16 pt-24">
        <div role="alert" className="flex max-w-[640px] flex-col gap-4">
          <h1 className="text-[44px] font-bold leading-[50px] tracking-[-0.015em]">No node answers</h1>
          <p className="text-[19px] leading-[30px] text-ink-muted">The console learns what you may see from a node, and none answers now. Until one does, it shows nothing else.</p>
        </div>
        <button type="button" className="btn-primary" onClick={onRetry}>
          <RefreshCw size={18} aria-hidden="true" /> Try again
        </button>
        <section aria-labelledby="k8s-h" className="panel flex w-full max-w-[720px] flex-col gap-4 p-8">
          <h2 id="k8s-h" className="text-[22px] font-semibold leading-[30px]">The cluster's own state is in Kubernetes</h2>
          <p className="text-ink-muted">Whoever runs the cluster can read the groups and their nodes there:</p>
          <pre className="overflow-x-auto rounded-md border border-line bg-surface px-6 py-4 font-mono text-sm"><code>{`kubectl get hugrnodegroups -n <namespace>
kubectl describe hugrnodegroup <group> -n <namespace>
kubectl get leases -n <namespace>`}</code></pre>
        </section>
      </main>
    </div>
  )
}

/** what a sign-in that did not leave for the IdP says */
export function unreachable(e: unknown): string {
  return `The identity provider does not answer: ${e instanceof Error ? e.message : 'unknown'}. Try again in a moment.`
}

/** the nodes refuse the person's token right after a sign-in: another sign-in would not mend it, the policy would */
export function Refused({ config, session }: { config: Config; session: Session }) {
  return (
    <main className="mx-auto flex max-w-[640px] flex-col gap-4 px-4 py-24">
      <img src={logo} alt="hugr" width={144} className="h-auto w-36" />
      <h1 className="text-[32px] font-semibold leading-10">The nodes refuse your sign-in</h1>
      <p role="alert" className="text-ink-muted">
        You signed in at the identity provider, and the platform's nodes do not accept the token it gave the console:
        the policy names no client for it, or your account is not one the client takes. The administrator of{' '}
        {config.platform}'s policy can tell which.
      </p>
      <div className="flex flex-wrap gap-4">
        <button type="button" className="btn-primary" onClick={() => void session.signIn(window.location.pathname)}>
          <RefreshCw size={18} aria-hidden="true" /> Sign in again
        </button>
        <button type="button" className="btn-outline" onClick={() => void session.signOut()}>
          <LogOut size={18} aria-hidden="true" /> Sign out
        </button>
      </div>
    </main>
  )
}

/** the console cannot start: no config (no console configured), or the sign-in failed */
export function Broken({ message }: { message: string }) {
  return (
    <main className="mx-auto flex max-w-[640px] flex-col gap-4 px-4 py-24">
      <img src={logo} alt="hugr" width={144} className="h-auto w-36" />
      <h1 className="text-[32px] font-semibold leading-10">The console cannot start</h1>
      <p role="alert" className="text-ink-muted">{message}</p>
    </main>
  )
}
