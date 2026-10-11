import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent } from 'vue'
import AccountTestModal from '../AccountTestModal.vue'

const {
  getTestAvailableModelsMock,
  getModelTestPromptSettingsMock,
  getAntigravityModelEffortSettingsMock
} = vi.hoisted(() => ({
  getTestAvailableModelsMock: vi.fn(),
  getModelTestPromptSettingsMock: vi.fn(),
  getAntigravityModelEffortSettingsMock: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getTestAvailableModels: getTestAvailableModelsMock
    },
    settings: {
      getModelTestPromptSettings: getModelTestPromptSettingsMock,
      getAntigravityModelEffortSettings: getAntigravityModelEffortSettingsMock
    }
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: vi.fn()
  })
}))

const MESSAGES: Record<string, string> = {
  'admin.accounts.imagePromptDefault': 'Generate a cute orange cat astronaut sticker.',
  'admin.accounts.testEffort.useDefault': 'Use default',
  'admin.accounts.testEffort.useDefaultWithLevel': 'Use default ({level})',
  'admin.accounts.testEffort.hint': 'Sent as reasoning_effort together with the selected model.',
  'admin.accounts.testEffort.levelsUnavailable':
    'The published reasoning levels of this account are empty for the selected model, so only its default can be used.',
  'admin.accounts.testEffort.levelsGloballyDisabled':
    'This account publishes levels for the selected model, but none of them is enabled in the global effort settings, so none can be chosen here.',
  'admin.accounts.testEffort.levelsUnproven':
    'The known reasoning levels come from the model registry fallback rather than from this account, so no level is offered here.',
  'admin.accounts.testEffort.suffixOverrides':
    'This account maps the model ID to an effort template ending in "-{level}", so the suffix sets the reasoning effort and overrides this selection.',
  'admin.accounts.testMetadata.requestedModel': 'Requested model',
  'admin.accounts.testMetadata.upstreamSentModel': 'Upstream sent model',
  'admin.accounts.testMetadata.upstreamResponseModel': 'Upstream response model',
  'admin.accounts.testMetadata.effectiveEffort': 'Reasoning effort',
  'admin.accounts.testMetadata.effectivePrompt': 'Effective prompt',
  'admin.accounts.testMetadata.streamModeLabel': 'Response mode',
  'admin.accounts.testMetadata.streamMode.stream': 'stream',
  'admin.accounts.testMetadata.streamMode.native_non_stream': 'native non-stream',
  'admin.accounts.testMetadata.notProvided': 'not provided',
  'admin.accounts.testMetadata.promptGlobal': 'global setting',
  'admin.accounts.testMetadata.promptBuiltin': 'built-in default',
  'admin.accounts.testMetadata.responseConflictHint': 'upstream model mismatch'
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        const template = MESSAGES[key] ?? key
        return params
          ? template.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ''))
          : template
      }
    })
  }
})

/**
 * What the projection stores for the live Antigravity catalog: the mapping key
 * is the testable model, its target is the formal effort template, and the
 * capability snapshot is keyed by the same base. `gpt-oss-120b-medium` is a
 * literal variant the catalog never collapses into a family.
 */
const AG_MAPPING = {
  'gemini-3.6-flash': 'gemini-3.6-flash-{effort}',
  'gpt-oss-120b-medium': 'gpt-oss-120b-medium',
  'gemini-pro-agent': 'gemini-pro-agent'
}

/** Globally enabled names and shared default, as the settings endpoint reports them. */
const GLOBAL_EFFORT = {
  levels: ['none', 'minimal', 'extra-low', 'low', 'medium', 'high', 'xhigh', 'max'],
  default_effort: 'medium'
}

const AG_CATALOG = {
  'gemini-3.6-flash': {
    id: 'gemini-3.6-flash',
    source: 'upstream',
    reasoning_levels_source: 'catalog_suffix',
    supported_reasoning_levels: ['low', 'medium', 'high'],
    default_reasoning_level: 'medium',
    display_name: 'Gemini 3.6 Flash'
  },
  'gpt-oss-120b-medium': {
    id: 'gpt-oss-120b-medium',
    source: 'upstream',
    display_name: 'GPT OSS 120B Medium'
  },
  'gemini-pro-agent': {
    id: 'gemini-pro-agent',
    source: 'upstream',
    reasoning_levels_source: 'upstream',
    supported_reasoning_levels: ['minimal', 'low'],
    default_reasoning_level: 'minimal',
    display_name: 'Gemini Pro Agent'
  }
}

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: { type: [String, Number, Boolean, null], default: '' },
    options: { type: Array, default: () => [] },
    valueKey: { type: String, default: 'value' },
    labelKey: { type: String, default: 'label' }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option
        v-for="option in options"
        :key="option[valueKey]"
        :value="option[valueKey]"
      >
        {{ option[labelKey] }}
      </option>
    </select>
  `
})

const TextAreaStub = defineComponent({
  name: 'TextArea',
  props: {
    modelValue: { type: String, default: '' }
  },
  emits: ['update:modelValue'],
  template: `
    <textarea
      v-bind="$attrs"
      :value="modelValue"
      @input="$emit('update:modelValue', $event.target.value)"
    />
  `
})

function buildAntigravityAccount(overrides: Record<string, unknown> = {}) {
  return buildAccount({
    id: 7,
    name: 'Antigravity OAuth',
    platform: 'antigravity',
    type: 'oauth',
    credentials: { model_mapping: { ...AG_MAPPING } },
    extra: { upstream_model_metadata: { models: { ...AG_CATALOG } } },
    ...overrides
  })
}

function modelOptionIds(wrapper: VueWrapper): string[] {
  const options = wrapper.findAllComponents(SelectStub)[0].props('options') as Array<{ id: string }>
  return options.map((option) => option.id)
}

function buildAccount(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    name: 'OpenAI OAuth',
    platform: 'openai',
    type: 'oauth',
    status: 'active',
    credentials: {},
    extra: {},
    concurrency: 1,
    priority: 1,
    proxy_id: null,
    auto_pause_on_expired: false,
    ...overrides
  } as any
}

function streamResponse(chunks: string[]) {
  const encoder = new TextEncoder()
  const encoded = chunks.map((chunk) => encoder.encode(chunk))
  let index = 0
  return {
    ok: true,
    body: {
      getReader: () => ({
        read: vi.fn().mockImplementation(async () =>
          index < encoded.length
            ? { done: false, value: encoded[index++] }
            : { done: true, value: undefined }
        )
      })
    }
  } as unknown as Response
}

async function mountModal(account = buildAccount()): Promise<VueWrapper> {
  const wrapper = mount(AccountTestModal, {
    props: {
      show: false,
      account
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        TextArea: TextAreaStub,
        Icon: true
      }
    }
  })
  // The modal loads models, the global prompt and effort capability on open.
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

function cellText(wrapper: VueWrapper, key: string): string {
  return wrapper.find(`[data-testid="account-test-metadata-${key}"]`).text()
}

function sentBody(call = 0): Record<string, unknown> {
  const [, options] = (global.fetch as any).mock.calls[call]
  return JSON.parse(options.body)
}

describe('AccountTestModal', () => {
  const originalFetch = global.fetch

  beforeEach(() => {
    getTestAvailableModelsMock.mockReset()
    getModelTestPromptSettingsMock.mockReset()
    getAntigravityModelEffortSettingsMock.mockReset()
    getTestAvailableModelsMock.mockResolvedValue([{ id: 'gpt-5.4', display_name: 'GPT-5.4' }])
    getModelTestPromptSettingsMock.mockResolvedValue({ prompt: '', default_prompt: 'hi' })
    getAntigravityModelEffortSettingsMock.mockResolvedValue({ ...GLOBAL_EFFORT })
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: vi.fn().mockResolvedValue({ done: true, value: undefined })
        })
      }
    } as any)
    localStorage.setItem('auth_token', 'test-token')
  })

  afterEach(() => {
    global.fetch = originalFetch
    localStorage.clear()
  })

  it('posts compact mode for OpenAI compact probe', async () => {
    const wrapper = await mountModal()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    ;(wrapper.vm as any).testMode = 'compact'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    expect(sentBody()).toMatchObject({
      model_id: 'gpt-5.4',
      mode: 'compact'
    })
  })

  it('renders Chat Completions path status from test SSE', async () => {
    const encoder = new TextEncoder()
    const chunks = [
      encoder.encode('data: {"type":"status","text":"已通过 /v1/chat/completions 验证"}\n\n'),
      encoder.encode('data: {"type":"test_complete","success":true}\n\n')
    ]
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: vi.fn().mockImplementation(() =>
            Promise.resolve(
              chunks.length > 0
                ? { done: false, value: chunks.shift() }
                : { done: true, value: undefined }
            )
          )
        })
      }
    } as any)

    const wrapper = await mountModal()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(wrapper.text()).toContain('已通过 /v1/chat/completions 验证')
  })

  it('renders every content delta while the stream is still open', async () => {
    const encoder = new TextEncoder()
    const chunks = [
      'data: {"type":"content","text":"hel"}\n\n',
      'data: {"type":"content","text":"lo"}\n\n',
      'data: {"type":"test_complete","success":true}\n\n'
    ]
    let releaseNext!: () => void
    let delivered = 0
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            if (delivered === 0) {
              delivered = 1
              return Promise.resolve({ done: false, value: encoder.encode(chunks[0]) })
            }
            return new Promise((resolve) => {
              releaseNext = () => {
                delivered += 1
                resolve(
                  delivered <= chunks.length
                    ? { done: false, value: encoder.encode(chunks[delivered - 1]) }
                    : { done: true, value: undefined }
                )
              }
            })
          }
        })
      }
    } as any)

    const wrapper = await mountModal()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    const pending = (wrapper.vm as any).startTest()

    // The first delta is on screen while the connection stays open.
    await vi.waitFor(() => expect(wrapper.text()).toContain('hel'))

    await vi.waitFor(() => expect(releaseNext).toBeTypeOf('function'))
    releaseNext()
    await vi.waitFor(() => expect(wrapper.text()).toContain('hello'))
    expect(wrapper.text()).not.toContain('admin.accounts.testCompleted')

    await vi.waitFor(() => expect(releaseNext).toBeTypeOf('function'))
    releaseNext()
    await vi.waitFor(() => expect(wrapper.text()).toContain('admin.accounts.testCompleted'))

    releaseNext()
    await pending
    await flushPromises()

    expect(wrapper.text()).toContain('hello')
    expect(wrapper.text()).toContain('admin.accounts.testCompleted')
  })

  it('shows the reported models and keeps an unprovided response model honest', async () => {
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse([
        'data: {"type":"test_start","model":"gpt-5.4","requested_model":"gpt-5.4","upstream_sent_model":"gpt-5.4-terra","effective_prompt":"Reply with the single word: hi","effective_effort":"high","stream_mode":"stream"}\n',
        'data: {"type":"test_metadata","upstream_response_model":"gpt-5.4-luna","upstream_response_model_conflict":true}\n',
        'data: {"type":"content","text":"pong"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = await mountModal()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(cellText(wrapper, 'requested-model')).toContain('gpt-5.4')
    expect(cellText(wrapper, 'upstream-sent-model')).toContain('gpt-5.4-terra')
    expect(cellText(wrapper, 'upstream-response-model')).toContain('gpt-5.4-luna')
    expect(cellText(wrapper, 'effective-effort')).toContain('high')
    expect(cellText(wrapper, 'effective-prompt')).toContain('Reply with the single word: hi')
    expect(cellText(wrapper, 'stream-mode')).toContain('stream')
    expect(wrapper.find('[data-testid="account-test-metadata-conflict"]').text()).toContain(
      'upstream model mismatch'
    )
  })

  it('marks a missing upstream response model as not provided', async () => {
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse([
        'data: {"type":"test_start","requested_model":"gpt-5.4"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = await mountModal()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(cellText(wrapper, 'upstream-sent-model')).toContain('not provided')
    expect(cellText(wrapper, 'upstream-response-model')).toContain('not provided')
    expect(wrapper.find('[data-testid="account-test-metadata-conflict"]').exists()).toBe(false)
  })

  it('shows the global test prompt that an ordinary text test will send', async () => {
    getModelTestPromptSettingsMock.mockResolvedValue({
      prompt: 'Reply with the single word: hi',
      default_prompt: 'hi'
    })

    const wrapper = await mountModal()
    await flushPromises()

    expect(cellText(wrapper, 'effective-prompt')).toContain('Reply with the single word: hi')
    expect(cellText(wrapper, 'effective-prompt')).toContain('global setting')
    expect(wrapper.find('textarea').exists()).toBe(false)

    await (wrapper.vm as any).startTest()
    await flushPromises()

    // The prompt travels through the global setting, not a per-test override.
    expect(sentBody()).toMatchObject({ model_id: 'gpt-5.4', prompt: '' })
  })

  it('falls back to the built-in default prompt label when nothing is configured', async () => {
    const wrapper = await mountModal()
    await flushPromises()

    expect(cellText(wrapper, 'effective-prompt')).toContain('hi')
    expect(cellText(wrapper, 'effective-prompt')).toContain('built-in default')
  })

  it('keeps media tests on their own specific prompt', async () => {
    getTestAvailableModelsMock.mockResolvedValue([
      { id: 'gemini-3.1-flash-image', display_name: 'Gemini 3.1 Flash Image' }
    ])
    getModelTestPromptSettingsMock.mockResolvedValue({
      prompt: 'Reply with the single word: hi',
      default_prompt: 'hi'
    })

    const wrapper = await mountModal(
      buildAccount({ id: 9, platform: 'gemini', type: 'apikey', name: 'Gemini API Key' })
    )
    await flushPromises()

    const promptInput = wrapper.find('textarea')
    expect(promptInput.exists()).toBe(true)
    await promptInput.setValue('draw a tiny orange cat astronaut')
    await flushPromises()

    expect(cellText(wrapper, 'effective-prompt')).toContain('draw a tiny orange cat astronaut')
    expect(cellText(wrapper, 'effective-prompt')).not.toContain('global setting')

    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(sentBody()).toMatchObject({
      model_id: 'gemini-3.1-flash-image',
      prompt: 'draw a tiny orange cat astronaut'
    })
  })

  it('offers the account mapping entries and sends the level the account publishes', async () => {
    // The account's model list still carries raw variant IDs; the mapping keys
    // are what this account can be tested with.
    getTestAvailableModelsMock.mockResolvedValue([
      { id: 'gemini-3.6-flash-high', display_name: 'Gemini 3.6 Flash High' },
      { id: 'gemini-3.6-flash-low', display_name: 'Gemini 3.6 Flash Low' }
    ])
    const wrapper = await mountModal(buildAntigravityAccount())
    await flushPromises()

    expect(modelOptionIds(wrapper)).toEqual([
      'gemini-3.6-flash',
      'gpt-oss-120b-medium',
      'gemini-pro-agent'
    ])

    const effort = wrapper.get('[data-testid="account-test-effort"]')
    expect(effort.findAll('option').map((option) => option.attributes('value'))).toEqual([
      '',
      'low',
      'medium',
      'high'
    ])
    // The account's own family default is shown; the global default is not invented.
    expect(effort.text()).toContain('Use default (medium)')

    await effort.setValue('high')
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(sentBody()).toMatchObject({ model_id: 'gemini-3.6-flash', reasoning_effort: 'high' })
  })

  it('sends no effort at all for Use default', async () => {
    const wrapper = await mountModal(buildAntigravityAccount())
    await flushPromises()

    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(sentBody()).not.toHaveProperty('reasoning_effort')
  })

  it('keeps a literal mapping entry as a raw model ID without effort', async () => {
    const wrapper = await mountModal(buildAntigravityAccount())
    ;(wrapper.vm as any).selectedModelId = 'gpt-oss-120b-medium'
    await flushPromises()

    // The mapping target is literal, so no effort family is authorized for it.
    expect(wrapper.find('[data-testid="account-test-effort"]').exists()).toBe(false)
    expect(cellText(wrapper, 'effective-effort')).toContain('not provided')

    await (wrapper.vm as any).startTest()
    await flushPromises()

    const body = sentBody()
    expect(body.model_id).toBe('gpt-oss-120b-medium')
    expect(body).not.toHaveProperty('reasoning_effort')
  })

  it('offers a literal model its own native levels and sends the chosen one', async () => {
    const wrapper = await mountModal(buildAntigravityAccount())
    ;(wrapper.vm as any).selectedModelId = 'gemini-pro-agent'
    await flushPromises()

    // `minimal` is not a globally enabled suffix name, because native thinking
    // levels are not subject to the Antigravity suffix policy.
    const effort = wrapper.get('[data-testid="account-test-effort"]')
    expect(effort.findAll('option').map((option) => option.attributes('value'))).toEqual([
      '',
      'minimal',
      'low'
    ])
    expect(effort.text()).toContain('Use default (minimal)')

    await effort.setValue('minimal')
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(sentBody()).toMatchObject({
      model_id: 'gemini-pro-agent',
      reasoning_effort: 'minimal'
    })
  })

  it('shows the shared global default of a bare {effort} template', async () => {
    getAntigravityModelEffortSettingsMock.mockResolvedValue({
      levels: ['low', 'medium', 'high', 'max'],
      default_effort: 'high'
    })
    const wrapper = await mountModal(buildAntigravityAccount())
    await flushPromises()

    expect(
      wrapper
        .get('[data-testid="account-test-effort"]')
        .findAll('option')
        .map((option) => option.attributes('value'))
    ).toEqual(['', 'low', 'medium', 'high'])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain('Use default (high)')

    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(sentBody()).not.toHaveProperty('reasoning_effort')
  })

  it('offers only Use default when the account publishes no levels', async () => {
    const wrapper = await mountModal(
      buildAntigravityAccount({
        extra: {
          upstream_model_metadata: {
            models: {
              'gemini-3.6-flash': {
                id: 'gemini-3.6-flash',
                source: 'upstream',
                reasoning_levels_source: 'upstream',
                supported_reasoning_levels: [],
                default_reasoning_level: 'medium'
              }
            }
          }
        }
      })
    )
    await flushPromises()

    const effort = wrapper.get('[data-testid="account-test-effort"]')
    expect(effort.findAll('option').map((option) => option.attributes('value'))).toEqual([''])
    // "Use default" without a level: nothing is invented from the registry.
    expect(effort.text()).toContain('Use default')
    expect(effort.text()).not.toContain('Use default (')
    expect(wrapper.get('[data-testid="account-test-effort-hint"]').text()).toContain(
      'published reasoning levels of this account are empty'
    )
    expect(cellText(wrapper, 'effective-effort')).toContain('Use default')
  })

  it('does not offer a registry fallback as if the account supported it', async () => {
    const wrapper = await mountModal(
      buildAntigravityAccount({
        extra: {
          upstream_model_metadata: {
            models: {
              'gemini-3.6-flash': {
                id: 'gemini-3.6-flash',
                source: 'models.dev',
                reasoning_levels_source: 'models.dev',
                supported_reasoning_levels: ['low', 'medium', 'high'],
                default_reasoning_level: 'medium'
              }
            }
          }
        }
      })
    )
    await flushPromises()

    const effort = wrapper.get('[data-testid="account-test-effort"]')
    expect(effort.findAll('option').map((option) => option.attributes('value'))).toEqual([''])
    expect(effort.text()).not.toContain('Use default (')
    expect(wrapper.get('[data-testid="account-test-effort-hint"]').text()).toContain(
      'registry fallback'
    )
  })

  it('does not offer an effort selector without published capability levels', async () => {
    const wrapper = await mountModal()
    await flushPromises()

    expect(wrapper.find('[data-testid="account-test-effort"]').exists()).toBe(false)
  })
})
