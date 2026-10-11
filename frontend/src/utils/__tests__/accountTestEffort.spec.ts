import { describe, expect, it } from 'vitest'
import {
  ACCOUNT_TEST_DEFAULT_EFFORT_VALUE,
  effortTemplates,
  intersectReasoningLevels,
  normalizeReasoningDefaultLevel,
  normalizeReasoningLevels,
  parseEffortTemplate,
  readModelCapability,
  readModelMapping,
  readUpstreamCatalogMetadata,
  resolveDefaultEffortLevel,
  resolveEffectiveEffort,
  resolveModelEffortFamily,
  resolvePublishedDefaultLevel
} from '@/utils/accountTestEffort'

const AGENT_LEVELS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']

/** The projection an Antigravity account stores for one effort family. */
const AG_MAPPING = { 'gemini-3.6-flash': 'gemini-3.6-flash-{effort:high}' }

function catalogMetadata(extra: Record<string, unknown>) {
  return { upstream_model_metadata: { models: extra } }
}

describe('normalizeReasoningLevels', () => {
  it('reads string lists and drops blanks and duplicates', () => {
    expect(normalizeReasoningLevels([' low ', 'low', '', '  ', 'high'])).toEqual(['low', 'high'])
  })

  it('reads object lists keyed by effort', () => {
    expect(
      normalizeReasoningLevels([{ effort: 'max', description: 'max' }, { effort: 'xhigh' }, {}])
    ).toEqual(['max', 'xhigh'])
  })

  it('ignores payloads that are not lists', () => {
    expect(normalizeReasoningLevels('high')).toEqual([])
    expect(normalizeReasoningLevels(undefined)).toEqual([])
  })
})

describe('normalizeReasoningDefaultLevel', () => {
  it('keeps a level the model publishes', () => {
    expect(normalizeReasoningDefaultLevel('high', AGENT_LEVELS)).toBe('high')
  })

  it('drops a level the published list does not contain', () => {
    expect(normalizeReasoningDefaultLevel('ultra', AGENT_LEVELS)).toBe('')
  })

  it('accepts any non-empty level when no published list is known', () => {
    expect(normalizeReasoningDefaultLevel(' medium ')).toBe('medium')
    expect(normalizeReasoningDefaultLevel(undefined)).toBe('')
  })
})

describe('parseEffortTemplate', () => {
  it('reads a template that defers to the global default', () => {
    expect(parseEffortTemplate('gemini-3.6-flash-{effort}')).toEqual({ defaultEffort: '' })
  })

  it('reads a template that names its own default level', () => {
    expect(parseEffortTemplate('gemini-3.6-flash-{effort:high}')).toEqual({ defaultEffort: 'high' })
    expect(parseEffortTemplate('custom-{effort:extra-low}')).toEqual({ defaultEffort: 'extra-low' })
  })

  it('rejects targets that are not a single formal template', () => {
    expect(parseEffortTemplate('gemini-3.6-flash')).toBeNull()
    expect(parseEffortTemplate('gemini-3.6-flash-high')).toBeNull()
    expect(parseEffortTemplate('{effort}')).toBeNull()
    expect(parseEffortTemplate('a-{effort:x}-{effort:y}')).toBeNull()
    expect(parseEffortTemplate('a-{effort:HIGH}')).toBeNull()
    expect(parseEffortTemplate('a-{effort:}')).toBeNull()
    expect(parseEffortTemplate('a b-{effort}')).toBeNull()
    expect(parseEffortTemplate('a-{effort}-suffix')).toBeNull()
    expect(parseEffortTemplate(undefined)).toBeNull()
  })
})

describe('readModelMapping', () => {
  it('reads credentials.model_mapping as a trimmed string map', () => {
    expect(
      readModelMapping({
        model_mapping: { ' gemini-3.6-flash ': 'gemini-3.6-flash-{effort:high}', '': 'x', b: '' }
      })
    ).toEqual({ 'gemini-3.6-flash': 'gemini-3.6-flash-{effort:high}' })
  })

  it('ignores missing or malformed mappings', () => {
    expect(readModelMapping(undefined)).toEqual({})
    expect(readModelMapping({})).toEqual({})
    expect(readModelMapping({ model_mapping: [] })).toEqual({})
    expect(readModelMapping({ model_mapping: 'gemini-3.6-flash' })).toEqual({})
    expect(readModelMapping({ model_mapping: { a: { nested: true } } })).toEqual({})
  })
})

describe('effortTemplates', () => {
  it('keeps only concrete bases that carry a formal template', () => {
    const templates = effortTemplates({
      ...AG_MAPPING,
      'gemini-3.6-flash-medium': 'gemini-3.6-flash-medium',
      '*-flash': 'anything-{effort}',
      'gemini-3.6-pro': 'gemini-3.6-pro-{effort}'
    })
    expect([...templates.keys()].sort()).toEqual(['gemini-3.6-flash', 'gemini-3.6-pro'])
    expect(templates.get('gemini-3.6-flash')).toEqual({ defaultEffort: 'high' })
    expect(templates.get('gemini-3.6-pro')).toEqual({ defaultEffort: '' })
  })
})

describe('resolveModelEffortFamily', () => {
  it('anchors the family on the account template key', () => {
    expect(resolveModelEffortFamily('gemini-3.6-flash', ['high', 'low'], AG_MAPPING)).toEqual({
      template: { defaultEffort: 'high' },
      level: ''
    })
  })

  it('reads the level named by a complete model ID suffix', () => {
    expect(
      resolveModelEffortFamily('gemini-3.6-flash-high', ['high', 'low'], AG_MAPPING)
    ).toEqual({ template: { defaultEffort: 'high' }, level: 'high' })
  })

  it('never splits a literal variant into a fake level', () => {
    // The mapping entry is literal, so the ID is forwarded exactly as requested.
    expect(
      resolveModelEffortFamily(
        'gemini-3.6-flash-medium',
        ['high', 'low', 'medium'],
        { 'gemini-3.6-flash-medium': 'gemini-3.6-flash-medium' }
      )
    ).toBeNull()
    // A suffix no template family publishes is not a level either.
    expect(resolveModelEffortFamily('gemini-3.6-flash-ultra', ['high', 'low'], AG_MAPPING)).toBeNull()
    expect(resolveModelEffortFamily('gpt-oss-120b-medium', ['medium'], AG_MAPPING)).toBeNull()
  })

  it('needs an enabled level, not just a level-shaped suffix', () => {
    expect(resolveModelEffortFamily('gemini-3.6-flash-high', [], AG_MAPPING)).toBeNull()
    expect(resolveModelEffortFamily('gemini-3.6-flash-high', ['low'], AG_MAPPING)).toBeNull()
  })

  it('prefers the longest level so extra-low never reads as low', () => {
    expect(
      resolveModelEffortFamily(
        'gemini-3.6-flash-extra-low',
        ['low', 'extra-low'],
        { 'gemini-3.6-flash': 'gemini-3.6-flash-{effort}' }
      )
    ).toEqual({ template: { defaultEffort: '' }, level: 'extra-low' })
  })

  it('declares no family without an account template', () => {
    expect(resolveModelEffortFamily('gemini-3.6-flash-high', AGENT_LEVELS, {})).toBeNull()
    expect(resolveModelEffortFamily('gemini-3.6-flash-high', AGENT_LEVELS, undefined)).toBeNull()
    expect(resolveModelEffortFamily('', AGENT_LEVELS, AG_MAPPING)).toBeNull()
  })
})

describe('intersectReasoningLevels', () => {
  it('keeps the account order and drops globally disabled names', () => {
    expect(
      intersectReasoningLevels(
        ['extra-low', 'low', 'medium', 'high'],
        ['none', 'low', 'medium', 'high', 'max']
      )
    ).toEqual(['low', 'medium', 'high'])
  })

  it('lets the account decide that a globally enabled name is not offered', () => {
    // `max` is enabled globally but this family never publishes it.
    expect(intersectReasoningLevels(['low', 'medium', 'high'], ['low', 'medium', 'high', 'max'])).toEqual([
      'low',
      'medium',
      'high'
    ])
  })

  it('offers nothing when no name is enabled', () => {
    expect(intersectReasoningLevels(['low', 'high'], [])).toEqual([])
    expect(intersectReasoningLevels([], ['low'])).toEqual([])
  })
})

describe('resolvePublishedDefaultLevel', () => {
  it('takes the first candidate the offered levels publish', () => {
    expect(resolvePublishedDefaultLevel(['low', 'medium'], ['high', 'low'])).toBe('low')
  })

  it('stays unresolved instead of naming an unpublished level', () => {
    expect(resolvePublishedDefaultLevel(['low'], ['extra-low', 'medium'])).toBe('')
    expect(resolvePublishedDefaultLevel([], ['medium'])).toBe('')
    expect(resolvePublishedDefaultLevel(['low'], ['', undefined])).toBe('')
  })
})

describe('resolveDefaultEffortLevel', () => {
  it('reports the level the account metadata declares', () => {
    expect(
      resolveDefaultEffortLevel({
        levels: ['high', 'low'],
        metadataDefaultLevel: 'low',
        templateDefaultEffort: 'high'
      })
    ).toBe('low')
  })

  it('falls back to the template default when metadata names none', () => {
    expect(
      resolveDefaultEffortLevel({ levels: ['high', 'low'], templateDefaultEffort: 'high' })
    ).toBe('high')
  })

  it('invents nothing when the account publishes no levels', () => {
    expect(
      resolveDefaultEffortLevel({ levels: [], metadataDefaultLevel: 'medium', templateDefaultEffort: 'medium' })
    ).toBe('')
    expect(resolveDefaultEffortLevel({ levels: [] })).toBe('')
  })

  it('ignores a default the published levels do not contain', () => {
    expect(
      resolveDefaultEffortLevel({
        levels: ['high'],
        metadataDefaultLevel: 'medium',
        templateDefaultEffort: 'medium'
      })
    ).toBe('')
  })
})

describe('readModelCapability', () => {
  it('reads the levels the account metadata declares', () => {
    expect(
      readModelCapability({
        id: 'gemini-3.6-flash',
        display_name: 'Gemini 3.6 Flash',
        source: 'upstream',
        reasoning_levels_source: 'catalog_suffix',
        supported_reasoning_levels: ['high', 'low'],
        default_reasoning_level: 'high'
      })
    ).toEqual({
      id: 'gemini-3.6-flash',
      displayName: 'Gemini 3.6 Flash',
      levels: ['high', 'low'],
      defaultLevel: 'high',
      accountProven: true,
      levelsSource: 'catalog_suffix',
      nativeLevels: false
    })
  })

  it('reads object-shaped levels from a model list entry', () => {
    expect(
      readModelCapability({
        id: 'gpt-5.4',
        supported_reasoning_levels: [{ effort: 'low' }, { effort: 'xhigh' }],
        default_reasoning_level: 'low'
      })
    ).toMatchObject({ levels: ['low', 'xhigh'], defaultLevel: 'low', displayName: 'gpt-5.4' })
  })

  it('marks the model native thinking levels as its own semantics', () => {
    // Provider-declared levels are not an Antigravity suffix projection.
    expect(
      readModelCapability({
        id: 'gemini-pro-agent',
        source: 'upstream',
        reasoning_levels_source: 'upstream',
        supported_reasoning_levels: ['minimal', 'low'],
        default_reasoning_level: 'minimal'
      })
    ).toMatchObject({ levelsSource: 'upstream', nativeLevels: true, accountProven: true })
  })

  it('keeps a suffix projection distinct from native levels', () => {
    expect(
      readModelCapability({
        id: 'gemini-3.6-flash',
        reasoning_levels_source: 'catalog_suffix',
        supported_reasoning_levels: ['high', 'low']
      })
    ).toMatchObject({ levelsSource: 'catalog_suffix', nativeLevels: false })
  })

  it('never offers a registry fallback as account capability', () => {
    const capability = readModelCapability({
      id: 'mystery-model',
      source: 'models.dev',
      reasoning_levels_source: 'models.dev',
      supported_reasoning_levels: ['low', 'medium', 'high'],
      default_reasoning_level: 'medium'
    })
    expect(capability.levels).toEqual([])
    expect(capability.defaultLevel).toBe('')
    expect(capability.accountProven).toBe(false)
    expect(capability.levelsSource).toBe('models.dev')
    expect(capability.nativeLevels).toBe(false)
  })

  it('drops a default that is not published', () => {
    expect(
      readModelCapability({ id: 'm', supported_reasoning_levels: ['high'], default_reasoning_level: 'max' })
    ).toMatchObject({ levels: ['high'], defaultLevel: '' })
  })

  it('survives a missing entry', () => {
    expect(readModelCapability(undefined)).toEqual({
      id: '',
      displayName: '',
      levels: [],
      defaultLevel: '',
      accountProven: true,
      levelsSource: '',
      nativeLevels: false
    })
  })
})

describe('readUpstreamCatalogMetadata', () => {
  it('reads the account snapshot keyed like the mapping', () => {
    const metadata = readUpstreamCatalogMetadata(
      catalogMetadata({
        'gemini-3.6-flash': {
          id: 'gemini-3.6-flash',
          reasoning_levels_source: 'catalog_suffix',
          supported_reasoning_levels: ['high', 'low'],
          default_reasoning_level: 'high',
          display_name: 'Gemini 3.6 Flash'
        }
      })
    )
    expect(metadata['gemini-3.6-flash']).toMatchObject({
      levels: ['high', 'low'],
      defaultLevel: 'high',
      accountProven: true
    })
  })

  it('returns nothing when the account carries no snapshot', () => {
    expect(readUpstreamCatalogMetadata(undefined)).toEqual({})
    expect(readUpstreamCatalogMetadata({})).toEqual({})
    expect(readUpstreamCatalogMetadata({ upstream_model_metadata: { models: [] } })).toEqual({})
    expect(readUpstreamCatalogMetadata({ upstream_model_metadata: 'nope' })).toEqual({})
  })
})

describe('resolveEffectiveEffort', () => {
  it('lets a model ID suffix override the dropdown selection', () => {
    expect(resolveEffectiveEffort({ selection: 'low', modelSuffix: 'high' })).toEqual({
      effort: 'high',
      source: 'model'
    })
  })

  it('uses the dropdown selection when the model ID carries no suffix', () => {
    expect(resolveEffectiveEffort({ selection: 'xhigh', modelSuffix: '' })).toEqual({
      effort: 'xhigh',
      source: 'selection'
    })
  })

  it('sends no effort at all for the provider default', () => {
    expect(
      resolveEffectiveEffort({ selection: ACCOUNT_TEST_DEFAULT_EFFORT_VALUE, modelSuffix: '' })
    ).toEqual({ effort: '', source: 'default' })
  })
})
