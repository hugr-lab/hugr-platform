import { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Problem, Segmented } from '../../components/ui'
import { useApp, useLoad } from '../../lib/context'
import { NodeFrame } from './Frame'

export function Logs() {
  const { label = '' } = useParams()
  const { api, rights } = useApp()
  const [previous, setPrevious] = useState<'current' | 'previous'>('current')
  const [lines, setLines] = useState<'100' | '500'>('100')
  const log = useLoad(() => (rights.infrastructure ? api.logs(label, previous === 'previous', Number(lines)) : Promise.resolve(null)),
    [api, label, previous, lines, rights.infrastructure])
  const pod = useLoad(() => api.pod(label), [api, label])
  return (
    <NodeFrame label={label} alsoReload={() => { log.reload(); pod.reload() }}>
      {() => (
        <>
          <section aria-labelledby="log-h" className="flex flex-col gap-4">
            <div className="flex flex-wrap items-end justify-between gap-4">
              <div className="max-w-[640px]">
                <h2 id="log-h" className="text-[22px] font-semibold leading-[30px]">The pod's log</h2>
                <p className="text-sm text-ink-muted">From Kubernetes, as the node wrote it: its bootstrap, its ticks and heartbeats. The previous container's log says why a node that restarted stopped.</p>
              </div>
              {rights.infrastructure && (
                <div className="flex flex-wrap gap-4">
                  <Segmented legend="Container" name="container" value={previous} onChange={setPrevious}
                    options={[{ value: 'current', label: 'Current' }, { value: 'previous', label: 'Previous' }]} />
                  <Segmented legend="Lines" name="lines" value={lines} onChange={setLines}
                    options={[{ value: '100', label: 'Last 100' }, { value: '500', label: 'Last 500' }]} />
                </div>
              )}
            </div>
            {!rights.infrastructure && (
              <p role="note" className="rounded-md border border-line px-6 py-4 text-sm">A node's log names hosts and sources; reading it takes the cluster right. The pod's events below are yours.</p>
            )}
            {log.error !== undefined && <Problem error={log.error} />}
            {log.data && (
              <pre className="max-h-[32rem] overflow-auto whitespace-pre rounded-md bg-surface-soft p-6 font-mono text-[13px] leading-[22px]"><code>{log.data.text || '(empty)'}</code></pre>
            )}
            {rights.infrastructure && <p className="text-sm text-ink-muted">At most 1 MiB, read when the page opens or on Refresh; never streamed. A line is shown as text.</p>}
          </section>
          <section aria-labelledby="ev-h" className="flex flex-col gap-4">
            <h2 id="ev-h" className="text-[22px] font-semibold leading-[30px]">The pod's events</h2>
            {pod.error !== undefined && <Problem error={pod.error} />}
            <ol className="flex flex-col rounded-md border border-line text-sm">
              {(pod.data?.events ?? []).map((e, i) => (
                <li key={i} className="grid grid-cols-[minmax(150px,0.6fr)_minmax(110px,0.5fr)_minmax(0,2fr)] items-baseline gap-4 border-b border-line px-6 py-3.5 last:border-b-0">
                  <span className="tabular-nums text-ink-muted">{e.last ? new Date(e.last).toLocaleString() : '-'}</span>
                  <span className="font-semibold">{e.reason}{e.type === 'Warning' ? ' (warning)' : ''}</span>
                  <span className="break-words">{e.message}</span>
                </li>
              ))}
              {pod.data && pod.data.events.length === 0 && <li className="px-6 py-3.5 text-ink-muted">No events kept for this pod.</li>}
            </ol>
          </section>
        </>
      )}
    </NodeFrame>
  )
}
