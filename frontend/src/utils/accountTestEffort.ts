/**
 * Reasoning-effort and model-capability helpers for the account connectivity
 * test modal.
 *
 * Two independent facts drive the effort UI, and neither is guessed:
 *
 * 1. *Authorization* — the account's own `model_mapping`. Only an entry whose
 *    target is a formal effort template (`base-{effort}` / `base-{effort:name}`)
 *    is an effort family; a literal variant such as `claude-opus-5-5-high` or
 *    `gpt-oss-120b-medium` stays a raw model ID with no effort semantics.
 * 2. *Capability* — the metadata the backend projects for that same mapping
 *    (`supported_reasoning_levels` / `default_reasoning_level`). Levels that are
 *    not published for the account are never fabricated, and a registry
 *    fallback (`reasoning_levels_source: models.dev`) is not account proof.
 */

/** Dropdown value that keeps the provider default (no `reasoning_effort` sent). */
export const ACCOUNT_TEST_DEFAULT_EFFORT_VALUE = ''

/** Where the effective effort came from. */
export type AccountTestEffortSource = 'model' | 'selection' | 'default'

export interface AccountTestEffectiveEffort {
  /** Value to send as `reasoning_effort`; empty means "omit the field". */
  effort: string
  source: AccountTestEffortSource
}

/** Mirrors the backend effort-name grammar. */
const EFFORT_NAME_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/

/** A formal `base-{effort}` / `base-{effort:name}` mapping target. */
export interface AccountTestEffortTemplate {
  /** Its own default effort, or `''` when it defers to the global default. */
  defaultEffort: string
}

/** Effort family a requested model ID belongs to, and the level its ID names. */
export interface AccountTestEffortFamily {
  template: AccountTestEffortTemplate
  /** Level named by the model ID; `''` for a base/default form. */
  level: string
}

/** Projected capability for one selectable model. */
export interface AccountTestModelCapability {
  id: string
  displayName: string
  /** Levels this account can actually address; `[]` when none is published. */
  levels: string[]
  /** Level a run without `reasoning_effort` resolves to; `''` when unresolved. */
  defaultLevel: string
  /** False when the levels are a registry fallback rather than account proof. */
  accountProven: boolean
  /**
   * Where the published levels came from (`upstream` / `catalog_suffix` /
   * `models.dev`), or `''` when the entry declares no provenance.
   */
  levelsSource: string
  /**
   * True when the levels are the model's own native thinking levels rather than
   * an effort-family projection or a registry fallback. The backend accepts an
   * explicit effort for a literal model only in this case.
   */
  nativeLevels: boolean
}

function effortFromEntry(entry: unknown): string {
  if (typeof entry === 'string') return entry.trim()
  if (typeof entry !== 'object' || entry === null) return ''
  const value = (entry as { effort?: unknown }).effort
  return typeof value === 'string' ? value.trim() : ''
}

function textOf(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

/**
 * Reads published reasoning levels from either `string[]` or
 * `{ effort: string }[]` payloads, dropping blanks and duplicates.
 */
export function normalizeReasoningLevels(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  const levels: string[] = []
  for (const entry of value) {
    const level = effortFromEntry(entry)
    if (!level || levels.includes(level)) continue
    levels.push(level)
  }
  return levels
}

/** Reads a published default level and ignores one that is not selectable. */
export function normalizeReasoningDefaultLevel(value: unknown, levels: string[] = []): string {
  const level = textOf(value)
  if (!level) return ''
  if (levels.length > 0 && !levels.includes(level)) return ''
  return level
}

/** Longest names first so `extra-low` is never read as `low`. */
function longestFirst(values: string[]): string[] {
  return [...values].sort((a, b) => b.length - a.length || a.localeCompare(b))
}

/**
 * Levels both sides publish, in the account's own order.
 *
 * The backend resolver only recognizes a level that is enabled in the global
 * effort settings, so a level the account once published but the global
 * settings no longer enable must not be offered.
 */
export function intersectReasoningLevels(levels: string[], enabled: string[]): string[] {
  const allowed = new Set(normalizeReasoningLevels(enabled))
  if (allowed.size === 0) return []
  return normalizeReasoningLevels(levels).filter((level) => allowed.has(level))
}

/**
 * First candidate the offered levels actually publish, or `''` when none does.
 *
 * Nothing is invented: an unresolved default stays unresolved, the selector
 * reads "Use default" without naming a level and the run sends no effort, so
 * the provider - not the UI - decides.
 */
export function resolvePublishedDefaultLevel(
  levels: string[],
  candidates: Array<string | undefined>
): string {
  if (levels.length === 0) return ''
  for (const candidate of candidates) {
    const level = textOf(candidate)
    if (level && levels.includes(level)) return level
  }
  return ''
}

/**
 * Parses a mapping target exactly like the backend effort resolver:
 * `prefix-{effort}` or `prefix-{effort:name}` with a single marker.
 */
export function parseEffortTemplate(target: unknown): AccountTestEffortTemplate | null {
  if (typeof target !== 'string') return null
  const marker = target.indexOf('-{effort')
  if (marker <= 0) return null
  if (target.split('{effort').length - 1 !== 1) return null
  if (/[{} \t\r\n]/.test(target.slice(0, marker))) return null

  const tail = target.slice(marker)
  if (tail === '-{effort}') return { defaultEffort: '' }
  if (tail.startsWith('-{effort:') && tail.endsWith('}')) {
    const name = tail.slice('-{effort:'.length, -1)
    if (EFFORT_NAME_PATTERN.test(name)) return { defaultEffort: name }
  }
  return null
}

/** Reads `credentials.model_mapping` as a plain string map. */
export function readModelMapping(credentials: unknown): Record<string, string> {
  if (typeof credentials !== 'object' || credentials === null) return {}
  const raw = (credentials as { model_mapping?: unknown }).model_mapping
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return {}

  const mapping: Record<string, string> = {}
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    const source = key.trim()
    const target = textOf(value)
    if (!source || !target) continue
    mapping[source] = target
  }
  return mapping
}

/** Formal effort templates declared by an account model mapping, by base ID. */
export function effortTemplates(modelMapping: unknown): Map<string, AccountTestEffortTemplate> {
  const templates = new Map<string, AccountTestEffortTemplate>()
  if (typeof modelMapping !== 'object' || modelMapping === null) return templates
  for (const [base, target] of Object.entries(modelMapping as Record<string, unknown>)) {
    const template = parseEffortTemplate(target)
    // Wildcard sources cannot carry a template (rejected by the backend), and
    // an effort family needs a concrete base ID.
    if (!template || base.includes('*') || !base.trim()) continue
    templates.set(base.trim(), template)
  }
  return templates
}

/**
 * Effort family the requested model ID belongs to, or `null` when the account
 * mapping declares no formal template for it.
 *
 * `level` is `''` for a template's base/default form, so only an ID that
 * literally names an enabled level of a template family locks the selector.
 * An exact mapping key always wins, exactly like the backend resolver: a
 * literal variant is forwarded as-is and no effort is applied to it.
 */
export function resolveModelEffortFamily(
  modelId: string,
  levels: string[],
  modelMapping: unknown
): AccountTestEffortFamily | null {
  const requested = modelId.trim()
  if (!requested) return null

  const templates = effortTemplates(modelMapping)
  if (templates.size === 0) return null

  const mapping = (modelMapping ?? {}) as Record<string, unknown>
  if (Object.prototype.hasOwnProperty.call(mapping, requested)) {
    const template = parseEffortTemplate(mapping[requested])
    return template ? { template, level: '' } : null
  }

  const direct = templates.get(requested)
  if (direct) return { template: direct, level: '' }

  const bases = longestFirst([...templates.keys()])
  for (const level of longestFirst(levels)) {
    for (const base of bases) {
      if (requested === `${base}-${level}`) {
        return { template: templates.get(base)!, level }
      }
    }
  }
  return null
}

/**
 * Resolves the effort a test will use: a level named by a formal model-ID
 * suffix wins over the dropdown, the dropdown wins over the capability default,
 * and the default itself is expressed by sending no effort at all.
 */
export function resolveEffectiveEffort(input: {
  selection: string
  modelSuffix: string
}): AccountTestEffectiveEffort {
  const suffix = input.modelSuffix.trim()
  if (suffix) return { effort: suffix, source: 'model' }

  const selection = input.selection.trim()
  if (selection) return { effort: selection, source: 'selection' }

  return { effort: '', source: 'default' }
}

/**
 * What "use default" resolves to, or `''` when it cannot be resolved.
 *
 * Only published levels are ever reported: the template's own `-{effort:name}`
 * default is applied by the backend before any global default, so it wins when
 * the capability publishes it. With no levels there is nothing to resolve and
 * the UI must say "use default" without naming a level.
 */
export function resolveDefaultEffortLevel(input: {
  levels: string[]
  metadataDefaultLevel?: string
  templateDefaultEffort?: string
}): string {
  return resolvePublishedDefaultLevel(input.levels, [
    input.metadataDefaultLevel,
    input.templateDefaultEffort
  ])
}

/**
 * Reads one projected capability entry.
 *
 * `reasoning_levels_source: 'models.dev'` marks a registry fallback that is not
 * proof for this account, so those levels are not offered at all. The source is
 * kept as well, because a `catalog_suffix` projection only means something
 * together with the effort family it was projected from.
 */
export function readModelCapability(entry: unknown): AccountTestModelCapability {
  const record = (typeof entry === 'object' && entry !== null ? entry : {}) as Record<
    string,
    unknown
  >
  const id = textOf(record.id)
  const levelsSource = textOf(record.reasoning_levels_source)
  const accountProven = levelsSource !== 'models.dev'
  const levels = accountProven ? normalizeReasoningLevels(record.supported_reasoning_levels) : []

  return {
    id,
    displayName: textOf(record.display_name) || id,
    levels,
    // A default is only nameable next to the levels that publish it; without
    // them the UI must say "use default" and not borrow the registry's answer.
    defaultLevel:
      levels.length > 0 ? normalizeReasoningDefaultLevel(record.default_reasoning_level, levels) : '',
    accountProven,
    levelsSource,
    nativeLevels:
      accountProven && levelsSource !== 'catalog_suffix' && levels.length > 0
  }
}

/**
 * Reads the account's stored upstream catalog snapshot
 * (`extra.upstream_model_metadata.models`), keyed the same way as the mapping
 * the backend projects for it.
 */
export function readUpstreamCatalogMetadata(
  extra: unknown
): Record<string, AccountTestModelCapability> {
  const result: Record<string, AccountTestModelCapability> = {}
  if (typeof extra !== 'object' || extra === null) return result

  const snapshot = (extra as { upstream_model_metadata?: unknown }).upstream_model_metadata
  if (typeof snapshot !== 'object' || snapshot === null) return result

  const models = (snapshot as { models?: unknown }).models
  if (typeof models !== 'object' || models === null || Array.isArray(models)) return result

  for (const [key, entry] of Object.entries(models as Record<string, unknown>)) {
    const id = key.trim()
    if (!id) continue
    result[id] = readModelCapability(entry)
  }
  return result
}
