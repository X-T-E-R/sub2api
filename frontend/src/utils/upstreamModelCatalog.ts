import type { SyncUpstreamModelsResult } from '@/api/admin/accounts'
import { isValidWildcardPattern, type ModelMappingEntry } from '@/composables/useModelWhitelist'

/**
 * Fields the backend catalog result must expose for the frontend to replace
 * editing state. `models` may hold the configured fallback list, so the model
 * list alone is never enough to project a mapping.
 */
export type UpstreamCatalogResult = Pick<
  SyncUpstreamModelsResult,
  'models' | 'source' | 'live' | 'model_mapping'
>

export type UpstreamCatalogOutcome = 'applied' | 'empty' | 'unavailable'

export interface UpstreamCatalogProjection {
  /**
   * applied: live upstream list with a usable request→upstream mapping.
   * empty: live upstream list returned no model IDs.
   * unavailable: not live (configured fallback) or no valid mapping — keep state.
   */
  outcome: UpstreamCatalogOutcome
  applied: boolean
  live: boolean
  source: SyncUpstreamModelsResult['source'] | null
  /** Live raw upstream model IDs, trimmed and deduplicated. */
  modelIds: string[]
  entries: ModelMappingEntry[]
  /** Request-side model names, taken from the mapping keys. */
  keys: string[]
}

/** Live catalog plus its mapping, emitted once editing state was replaced. */
export interface UpstreamCatalogApplied {
  /** Request-side model names now selected. */
  models: string[]
  /** Full request→upstream mapping so callers keep real upstream targets. */
  modelMapping: Record<string, string>
}

function trim(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

/**
 * A formal catalog model ID: no leading/trailing or interior whitespace and no
 * control characters. The save path tolerates looser strings, but a catalog
 * projection must carry real model IDs, so anything else fails closed.
 */
function hasControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code <= 0x1f || code === 0x7f) return true
  }
  return false
}

function isFormalModelId(value: string): boolean {
  return value.length > 0 && value === value.trim() && !/\s/.test(value) && !hasControlCharacter(value)
}

function modelIdsOf(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return Array.from(new Set(value.map(trim).filter(Boolean)))
}

function unavailableProjection(
  live: boolean,
  source: SyncUpstreamModelsResult['source'] | null,
  modelIds: string[] = []
): UpstreamCatalogProjection {
  return { outcome: 'unavailable', applied: false, live, source, modelIds, entries: [], keys: [] }
}

/**
 * Validate the single formal catalog projection. The backend projection is
 * all-or-nothing: one unusable entry (non-string target, empty side, whitespace
 * or control characters, misplaced wildcard, wildcard target, or two raw keys
 * that normalise to the same request model) rejects the whole mapping so editing
 * state can never be replaced by a partial projection.
 */
export function normalizeUpstreamCatalogMapping(raw: unknown): ModelMappingEntry[] {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return []

  const entries: ModelMappingEntry[] = []
  const seen = new Set<string>()
  for (const [rawFrom, rawTo] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof rawTo !== 'string') return []
    if (!isFormalModelId(rawFrom) || !isFormalModelId(rawTo)) return []
    if (!isValidWildcardPattern(rawFrom)) return []
    if (rawTo.includes('*')) return []
    if (seen.has(rawFrom)) return []
    seen.add(rawFrom)
    entries.push({ from: rawFrom, to: rawTo })
  }
  return entries
}

export function upstreamCatalogMappingRecord(entries: ModelMappingEntry[]): Record<string, string> {
  return Object.fromEntries(entries.map((entry) => [entry.from, entry.to]))
}

/** True when both lists hold the same model names, regardless of order. */
export function isSameModelKeySet(current: string[], next: string[]): boolean {
  const currentKeys = new Set(current)
  const nextKeys = new Set(next)
  if (currentKeys.size !== nextKeys.size) return false
  return Array.from(currentKeys).every((key) => nextKeys.has(key))
}

/** True when both mapping row sets describe the same request→upstream pairs. */
export function isSameUpstreamCatalogMapping(
  current: ModelMappingEntry[],
  next: ModelMappingEntry[]
): boolean {
  if (current.length !== next.length) return false
  const nextTargets = new Map(next.map((entry) => [entry.from, entry.to]))
  return current.every((entry) => nextTargets.get(entry.from) === entry.to)
}

/**
 * Decide what a sync response may do to editing state. Only a live upstream
 * list carrying a non-empty valid mapping is projectable; configured fallback
 * lists and empty/invalid mappings must leave state untouched.
 */
export function resolveUpstreamCatalogProjection(
  result: UpstreamCatalogResult | null | undefined
): UpstreamCatalogProjection {
  const live = result?.live === true
  const source = result?.source ?? null
  if (!live || source !== 'upstream') {
    return unavailableProjection(live, source)
  }

  const modelIds = modelIdsOf(result?.models)
  if (modelIds.length === 0) {
    return { outcome: 'empty', applied: false, live, source, modelIds, entries: [], keys: [] }
  }

  const entries = normalizeUpstreamCatalogMapping(result?.model_mapping)
  if (entries.length === 0) {
    return unavailableProjection(live, source, modelIds)
  }

  return {
    outcome: 'applied',
    applied: true,
    live,
    source,
    modelIds,
    entries,
    keys: entries.map((entry) => entry.from)
  }
}
