import { describe, expect, it, vi } from 'vitest'
import type { SyncUpstreamModelsResult } from '@/api/admin/accounts'

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

import {
  isSameModelKeySet,
  isSameUpstreamCatalogMapping,
  normalizeUpstreamCatalogMapping,
  resolveUpstreamCatalogProjection,
  upstreamCatalogMappingRecord,
  type UpstreamCatalogResult
} from '../upstreamModelCatalog'

function catalogResult(overrides: Partial<UpstreamCatalogResult> = {}): UpstreamCatalogResult {
  return {
    models: ['gemini-3.8-flash', 'gemini-3.8-flash-high'],
    source: 'upstream',
    live: true,
    model_mapping: {
      'gemini-3.8-flash': 'gemini-3.8-flash-{effort}',
      'gemini-3.6-flash': 'gemini-3.6-flash-{effort:low}'
    },
    ...overrides
  }
}

describe('normalizeUpstreamCatalogMapping', () => {
  it('keeps formal catalog pairs including template targets and wildcard keys', () => {
    expect(
      normalizeUpstreamCatalogMapping({
        'gemini-3.8-flash': 'gemini-3.8-flash-{effort:low}',
        'grok-latest': 'grok-4.5',
        'claude-*': 'claude-4.6',
        'claude-sonnet-4-5': 'claude-sonnet-4-5'
      })
    ).toEqual([
      { from: 'gemini-3.8-flash', to: 'gemini-3.8-flash-{effort:low}' },
      { from: 'grok-latest', to: 'grok-4.5' },
      { from: 'claude-*', to: 'claude-4.6' },
      { from: 'claude-sonnet-4-5', to: 'claude-sonnet-4-5' }
    ])
  })

  it('rejects the whole mapping when any single entry is unusable', () => {
    const valid = { 'gemini-3.8-flash': 'gemini-3.8-flash-{effort}' }

    expect(normalizeUpstreamCatalogMapping({ ...valid, ' ': 'x' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': '' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': '   ' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': 5 })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': null })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a*b': 'x' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, '*a': 'x' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': 'x*' })).toEqual([])
    // Whitespace or control characters are not formal model IDs.
    expect(normalizeUpstreamCatalogMapping({ ...valid, ' gemini-3.8-flash': 'x' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'gemini 3.8 flash': 'x' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': ' gemini-3.8-flash' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': 'gemini\n-3.8' })).toEqual([])
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': 'gemini\u00003.8' })).toEqual([])
    // Two raw keys that normalise to the same request model are ambiguous.
    expect(normalizeUpstreamCatalogMapping({ ...valid, 'a': 'x', ' a ': 'x' })).toEqual([])
  })

  it('returns no entries for empty or non-object payloads', () => {
    expect(normalizeUpstreamCatalogMapping(null)).toEqual([])
    expect(normalizeUpstreamCatalogMapping('a=b')).toEqual([])
    expect(normalizeUpstreamCatalogMapping(['a'])).toEqual([])
    expect(normalizeUpstreamCatalogMapping({})).toEqual([])
  })
})

describe('resolveUpstreamCatalogProjection', () => {
  it('applies a live upstream catalog and exposes keys plus real targets', () => {
    const projection = resolveUpstreamCatalogProjection(catalogResult())

    expect(projection.outcome).toBe('applied')
    expect(projection.applied).toBe(true)
    expect(projection.live).toBe(true)
    expect(projection.source).toBe('upstream')
    expect(projection.keys).toEqual(['gemini-3.8-flash', 'gemini-3.6-flash'])
    expect(projection.entries).toEqual([
      { from: 'gemini-3.8-flash', to: 'gemini-3.8-flash-{effort}' },
      { from: 'gemini-3.6-flash', to: 'gemini-3.6-flash-{effort:low}' }
    ])
    expect(projection.modelIds).toEqual(['gemini-3.8-flash', 'gemini-3.8-flash-high'])
  })

  it('trims and deduplicates the reported upstream IDs', () => {
    const projection = resolveUpstreamCatalogProjection(
      catalogResult({ models: [' x ', 'x', '', 'y'] })
    )

    expect(projection.modelIds).toEqual(['x', 'y'])
  })

  it('never projects the configured fallback list', () => {
    const projection = resolveUpstreamCatalogProjection(
      catalogResult({ source: 'configured', live: false })
    )

    expect(projection.outcome).toBe('unavailable')
    expect(projection.applied).toBe(false)
    expect(projection.live).toBe(false)
    expect(projection.source).toBe('configured')
    expect(projection.entries).toEqual([])
    expect(projection.keys).toEqual([])
  })

  it('never projects a live list that is not sourced from upstream', () => {
    const projection = resolveUpstreamCatalogProjection(catalogResult({ source: 'configured' }))

    expect(projection.outcome).toBe('unavailable')
    expect(projection.applied).toBe(false)
    expect(projection.modelIds).toEqual([])
  })

  it('reports an empty live list', () => {
    const projection = resolveUpstreamCatalogProjection(
      catalogResult({ models: [], model_mapping: {} })
    )

    expect(projection.outcome).toBe('empty')
    expect(projection.applied).toBe(false)
    expect(projection.entries).toEqual([])
  })

  it('keeps no entries when any live mapping entry is unusable', () => {
    const projection = resolveUpstreamCatalogProjection(
      catalogResult({ model_mapping: { 'gemini-3.8-flash': 'x*' } })
    )

    expect(projection.outcome).toBe('unavailable')
    expect(projection.applied).toBe(false)
    expect(projection.entries).toEqual([])
    expect(projection.modelIds).toEqual(['gemini-3.8-flash', 'gemini-3.8-flash-high'])
  })

  it('never partially applies a mixed valid/invalid live mapping', () => {
    const projection = resolveUpstreamCatalogProjection(
      catalogResult({
        model_mapping: {
          'gemini-3.8-flash': 'gemini-3.8-flash-{effort}',
          'claude-sonnet-4-5': 'bad target'
        }
      })
    )

    expect(projection.outcome).toBe('unavailable')
    expect(projection.applied).toBe(false)
    expect(projection.entries).toEqual([])
    expect(projection.keys).toEqual([])
  })

  it('treats a missing result as unavailable', () => {
    expect(resolveUpstreamCatalogProjection(null).outcome).toBe('unavailable')
    expect(resolveUpstreamCatalogProjection(undefined).outcome).toBe('unavailable')
    expect(resolveUpstreamCatalogProjection({} as UpstreamCatalogResult).outcome).toBe('unavailable')
  })

  it('matches the formal backend result contract', () => {
    const documented: SyncUpstreamModelsResult = {
      models: ['gemini-3.8-flash'],
      metadata: { 'gemini-3.8-flash': { id: 'gemini-3.8-flash', source: 'upstream' } },
      warnings: [],
      source: 'upstream',
      live: true,
      model_mapping: { 'gemini-3.8-flash': 'gemini-3.8-flash-{effort}' }
    }

    expect(resolveUpstreamCatalogProjection(documented).keys).toEqual(['gemini-3.8-flash'])
  })
})

describe('mapping comparison helpers', () => {
  it('compares key sets without order', () => {
    expect(isSameModelKeySet(['a', 'b'], ['b', 'a'])).toBe(true)
    expect(isSameModelKeySet(['a'], ['a', 'b'])).toBe(false)
    expect(isSameModelKeySet(['a'], ['b'])).toBe(false)
    expect(isSameModelKeySet(['a', 'a'], ['a'])).toBe(true)
  })

  it('compares mapping pairs including the upstream targets', () => {
    const current = [{ from: 'a', to: 'a-{effort}' }, { from: 'b', to: 'b' }]

    expect(isSameUpstreamCatalogMapping(current, [{ from: 'b', to: 'b' }, { from: 'a', to: 'a-{effort}' }])).toBe(true)
    expect(isSameUpstreamCatalogMapping(current, [{ from: 'a', to: 'a' }, { from: 'b', to: 'b' }])).toBe(false)
    expect(isSameUpstreamCatalogMapping(current, [{ from: 'a', to: 'a-{effort}' }])).toBe(false)
  })

  it('builds a mapping record from entries', () => {
    expect(upstreamCatalogMappingRecord([{ from: 'a', to: 'a-{effort}' }])).toEqual({ a: 'a-{effort}' })
  })
})
