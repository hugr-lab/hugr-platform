import { describe, expect, it } from 'vitest'
import { bytes, fill, home, sentence, share, traceOf } from './format'

const link = { kind: 'telemetry' as const, name: 'Grafana', url: 'https://grafana.example.org/explore?node={node}&s={session}' }

describe('the links out', () => {
  it('fills a template, every value escaped', () => {
    expect(fill(link, { node: 'hugr-node-etl-a', session: 'A&B=1 x' }))
      .toBe('https://grafana.example.org/explore?node=hugr-node-etl-a&s=A%26B%3D1%20x')
  })
  it('gives no link when a value it names is missing, or the template is no https', () => {
    expect(fill(link, { node: 'n' })).toBeUndefined()
    expect(fill({ ...link, url: 'javascript:alert({node})' }, { node: 'n' })).toBeUndefined()
  })
  it('goes home for a page that names nothing', () => {
    expect(home(link)).toBe('https://grafana.example.org')
    expect(home({ ...link, url: 'http://grafana.example.org/{node}' })).toBeUndefined()
  })
  it('reads the trace of a traceparent only', () => {
    expect(traceOf('00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01')).toBe('4bf92f3577b34da6a3ce929d0e0e4736')
    expect(traceOf('garbage')).toBeUndefined()
    expect(traceOf(null)).toBeUndefined()
  })
})

describe('numbers a person reads', () => {
  it('bytes', () => {
    expect(bytes(0)).toBe('0 B')
    expect(bytes(9.8 * 1024 ** 3)).toBe('9.8 GiB')
    expect(bytes(16 * 1024 ** 3)).toBe('16 GiB')
    expect(bytes(null)).toBe('-')
  })
  it('a share, bounded', () => {
    expect(share(5, 10)).toBe(50)
    expect(share(20, 10)).toBe(100)
    expect(share(1, 0)).toBe(0)
  })
})

describe('a reason as a sentence', () => {
  it('trims, capitalises, ends once', () => {
    expect(sentence('a standalone node keeps no cluster profile')).toBe('A standalone node keeps no cluster profile.')
    expect(sentence('Catalog "x" does not exist!\n')).toBe('Catalog "x" does not exist!')
    expect(sentence('  ')).toBe('')
  })
})
