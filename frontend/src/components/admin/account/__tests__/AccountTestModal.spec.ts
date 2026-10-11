import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import AccountTestModal from '../AccountTestModal.vue'

const {
  getTestAvailableModels,
  getModelTestPromptSettings,
  getAntigravityModelEffortSettings,
  copyToClipboard
} = vi.hoisted(() => ({
  getTestAvailableModels: vi.fn(),
  getModelTestPromptSettings: vi.fn(),
  getAntigravityModelEffortSettings: vi.fn(),
  copyToClipboard: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getTestAvailableModels
    },
    settings: {
      getModelTestPromptSettings,
      getAntigravityModelEffortSettings
    }
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard
  })
}))

const MESSAGES: Record<string, string> = {
  'admin.accounts.imagePromptDefault': 'Generate a cute orange cat astronaut sticker on a clean pastel background.',
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
  'admin.accounts.testMetadata.promptReported': 'reported by the test run',
  'admin.accounts.testMetadata.effortFromSuffix': 'from model ID suffix',
  'admin.accounts.testMetadata.responseConflictHint': 'upstream model mismatch'
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        if (key === 'admin.accounts.imageReceived' && params?.count) {
          return `received-${params.count}`
        }
        if (key === 'admin.accounts.imagePreviewAlt' && params?.index) {
          return `test-image-${params.index}`
        }
        const template = MESSAGES[key] ?? key
        return params
          ? template.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ''))
          : template
      }
    })
  }
})

/**
 * The projection `projectUpstreamModelMapping` stores for the live Antigravity
 * catalog: variant IDs collapse into one base entry per effort family, whose
 * target is a formal effort template, and the capability snapshot is keyed by
 * that base. The global default effort is `medium`, so a family whose own
 * default is `medium` keeps the bare `{effort}` template.
 */
const AG_MAPPING = {
  'gemini-3.6-flash': 'gemini-3.6-flash-{effort}',
  'gemini-3.1-pro': 'gemini-3.1-pro-{effort:high}',
  'gemini-3.5-flash': 'gemini-3.5-flash-{effort:extra-low}',
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
  'gemini-3.1-pro': {
    id: 'gemini-3.1-pro',
    source: 'upstream',
    reasoning_levels_source: 'catalog_suffix',
    supported_reasoning_levels: ['high', 'low'],
    default_reasoning_level: 'high',
    display_name: 'Gemini 3.1 Pro High'
  },
  'gemini-3.5-flash': {
    id: 'gemini-3.5-flash',
    source: 'upstream',
    reasoning_levels_source: 'catalog_suffix',
    supported_reasoning_levels: ['extra-low', 'low'],
    default_reasoning_level: 'extra-low',
    display_name: 'Gemini 3.5 Flash Low'
  },
  'gpt-oss-120b-medium': {
    id: 'gpt-oss-120b-medium',
    source: 'upstream',
    display_name: 'GPT OSS 120B Medium'
  },
  // A literal request name whose own native thinking levels the provider declares.
  'gemini-pro-agent': {
    id: 'gemini-pro-agent',
    source: 'upstream',
    reasoning_levels_source: 'upstream',
    supported_reasoning_levels: ['minimal', 'low'],
    default_reasoning_level: 'minimal',
    display_name: 'Gemini Pro Agent'
  }
}

function buildAntigravityAccount(overrides: Record<string, unknown> = {}) {
  return {
    id: 7,
    name: 'Antigravity OAuth',
    platform: 'antigravity',
    type: 'oauth',
    status: 'active',
    credentials: { model_mapping: { ...AG_MAPPING } },
    extra: { upstream_model_metadata: { models: { ...AG_CATALOG } } },
    ...overrides
  }
}

function modelOptionIds(wrapper: VueWrapper): string[] {
  const options = wrapper.findAllComponents(SelectStub)[0].props('options') as Array<{ id: string }>
  return options.map((option) => option.id)
}

function effortOptionValues(wrapper: VueWrapper): Array<string | undefined> {
  return wrapper
    .get('[data-testid="account-test-effort"]')
    .findAll('option')
    .map((option) => option.attributes('value'))
}

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: { type: [String, Number, Boolean, null], default: '' },
    options: { type: Array, default: () => [] },
    valueKey: { type: String, default: 'value' },
    labelKey: { type: String, default: 'label' },
    disabled: { type: Boolean, default: false }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      :disabled="disabled"
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
  name: 'TextAreaStub',
  props: { modelValue: { type: String, default: '' } },
  emits: ['update:modelValue'],
  template: '<textarea class="textarea-stub" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />'
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialogStub',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

function streamResponse(chunks: string[]) {
  const encoder = new TextEncoder()
  const encoded = chunks.map((chunk) => encoder.encode(chunk))
  let index = 0

  return {
    ok: true,
    body: {
      getReader: () => ({
        read: vi.fn().mockImplementation(async () => {
          if (index < encoded.length) {
            return { done: false, value: encoded[index++] }
          }
          return { done: true, value: undefined }
        })
      })
    }
  } as unknown as Response
}

function mountModal(account: Record<string, unknown> = {
  id: 42,
  name: 'Gemini Image Test',
  platform: 'gemini',
  type: 'apikey',
  status: 'active'
}): VueWrapper {
  return mount(AccountTestModal, {
    props: {
      show: false,
      account
    } as any,
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        TextArea: TextAreaStub,
        Icon: true
      }
    }
  })
}

function startButton(wrapper: VueWrapper) {
  const button = wrapper
    .findAll('button')
    .find((candidate) => candidate.text().includes('admin.accounts.startTest'))
  if (!button) throw new Error('start button not found')
  return button
}

function cellText(wrapper: VueWrapper, key: string): string {
  return wrapper.find(`[data-testid="account-test-metadata-${key}"]`).text()
}

function sentBody(call = 0): Record<string, unknown> {
  const [, request] = (global.fetch as any).mock.calls[call]
  return JSON.parse(request.body)
}

describe('AccountTestModal', () => {
  beforeEach(() => {
    getTestAvailableModels.mockReset()
    getModelTestPromptSettings.mockReset()
    getAntigravityModelEffortSettings.mockReset()
    copyToClipboard.mockReset()
    getTestAvailableModels.mockResolvedValue([
      { id: 'gemini-2.0-flash', display_name: 'Gemini 2.0 Flash' },
      { id: 'gemini-2.5-flash-image', display_name: 'Gemini 2.5 Flash Image' },
      { id: 'gemini-3.1-flash-image', display_name: 'Gemini 3.1 Flash Image' }
    ])
    getModelTestPromptSettings.mockResolvedValue({ prompt: '', default_prompt: 'hi' })
    getAntigravityModelEffortSettings.mockResolvedValue({ ...GLOBAL_EFFORT })
    Object.defineProperty(globalThis, 'localStorage', {
      value: {
        getItem: vi.fn((key: string) => (key === 'auth_token' ? 'test-token' : null)),
        setItem: vi.fn(),
        removeItem: vi.fn(),
        clear: vi.fn()
      },
      configurable: true
    })
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse([
        'data: {"type":"test_start","model":"gemini-2.5-flash-image"}\n',
        'data: {"type":"image","image_url":"data:image/png;base64,QUJD","mime_type":"image/png"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('gemini 图片模型测试会携带提示词并渲染图片预览', async () => {
    const wrapper = mountModal()
    await wrapper.setProps({ show: true })
    await flushPromises()

    const promptInput = wrapper.find('textarea.textarea-stub')
    expect(promptInput.exists()).toBe(true)
    await promptInput.setValue('draw a tiny orange cat astronaut')

    await startButton(wrapper).trigger('click')
    await flushPromises()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    expect(sentBody()).toEqual({
      model_id: 'gemini-3.1-flash-image',
      prompt: 'draw a tiny orange cat astronaut'
    })

    const preview = wrapper.find('img[alt="test-image-1"]')
    expect(preview.exists()).toBe(true)
    expect(preview.attributes('src')).toBe('data:image/png;base64,QUJD')
  })

  it('grok 账号测试默认选择 Grok 模型', async () => {
    getTestAvailableModels.mockResolvedValue([
      { id: 'grok-4.3', display_name: 'Grok 4.3' },
      { id: 'grok-build-0.1', display_name: 'Grok Build 0.1' }
    ])
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse([
        'data: {"type":"test_start","model":"grok-4.3"}\n',
        'data: {"type":"content","text":"ok"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = mountModal({
      id: 13,
      name: 'Grok Account',
      platform: 'grok',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    expect(sentBody()).toEqual({
      model_id: 'grok-4.3',
      prompt: '',
      mode: 'text'
    })
  })

  it('OpenAI Compact 探测会携带 compact 测试模式', async () => {
    getTestAvailableModels.mockResolvedValue([{ id: 'gpt-5.4', display_name: 'GPT-5.4' }])
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    ;(wrapper.vm as any).testMode = 'compact'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    expect(sentBody()).toMatchObject({
      model_id: 'gpt-5.4',
      prompt: '',
      mode: 'compact'
    })
  })

  it('每次 content 事件到达时立即渲染增量文本，而不是等到结束', async () => {
    const encoder = new TextEncoder()
    const chunks = [
      'data: {"type":"content","text":"hel"}\n\n',
      'data: {"type":"content","text":"lo"}\n\n',
      'data: {"type":"test_complete","success":true}\n\n'
    ]
    let releaseStream!: () => void
    let reads = 0
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            reads += 1
            if (reads <= chunks.length) {
              return Promise.resolve({
                done: false,
                value: encoder.encode(chunks[reads - 1])
              })
            }
            return new Promise((resolve) => {
              releaseStream = () => resolve({ done: true, value: undefined })
            })
          }
        })
      }
    } as any)

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    const pending = (wrapper.vm as any).startTest()
    // Each delta is rendered while the connection is still open.
    await vi.waitFor(() => expect(wrapper.text()).toContain('hel'))
    await vi.waitFor(() => expect(wrapper.text()).toContain('hello'))

    releaseStream()
    await pending
    await flushPromises()

    expect(wrapper.text()).toContain('hello')
    expect(wrapper.text()).toContain('admin.accounts.testCompleted')
  })

  it('展示 test_start 与 test_metadata 上报的模型元数据，并如实标注冲突', async () => {
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse([
        'data: {"type":"test_start","model":"gemini-3.6-flash-medium","requested_model":"gemini-3.6-flash","upstream_sent_model":"gemini-3.6-flash-medium","effective_prompt":"hi","effective_effort":"medium","stream_mode":"native_non_stream"}\n',
        'data: {"type":"test_metadata","upstream_response_model":"gemini-3.6-flash-high","upstream_response_model_conflict":true}\n',
        'data: {"type":"content","text":"pong"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(cellText(wrapper, 'requested-model')).toContain('gemini-3.6-flash')
    expect(cellText(wrapper, 'upstream-sent-model')).toContain('gemini-3.6-flash-medium')
    expect(cellText(wrapper, 'upstream-response-model')).toContain('gemini-3.6-flash-high')
    expect(cellText(wrapper, 'effective-effort')).toContain('medium')
    expect(cellText(wrapper, 'effective-prompt')).toContain('hi')
    expect(cellText(wrapper, 'stream-mode')).toContain('native non-stream')
    expect(
      wrapper.find('[data-testid="account-test-terminal-mode"]').text()
    ).toContain('native non-stream')
    expect(wrapper.find('[data-testid="account-test-metadata-conflict"]').exists()).toBe(true)
  })

  it('未上报的响应模型如实显示为 not provided', async () => {
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse([
        'data: {"type":"test_start","requested_model":"gpt-5.4"}\n',
        'data: {"type":"test_complete","success":true}\n'
      ])
    ) as any

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(cellText(wrapper, 'requested-model')).toContain('gpt-5.4')
    expect(cellText(wrapper, 'upstream-sent-model')).toContain('not provided')
    expect(cellText(wrapper, 'upstream-response-model')).toContain('not provided')
    expect(wrapper.find('[data-testid="account-test-metadata-conflict"]').exists()).toBe(false)
  })

  it('写明全局提示词在普通文本测试中生效', async () => {
    getModelTestPromptSettings.mockResolvedValue({
      prompt: 'Reply with the single word: hi',
      default_prompt: 'hi'
    })

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(cellText(wrapper, 'effective-prompt')).toContain('Reply with the single word: hi')
    expect(cellText(wrapper, 'effective-prompt')).toContain('global setting')
    expect(wrapper.find('textarea.textarea-stub').exists()).toBe(false)
  })

  it('档位来自账号 model_mapping 与投影元数据，选择后随请求发送', async () => {
    // The account's own model list still carries the raw variant IDs; the
    // mapping keys are what this account can be tested with.
    getTestAvailableModels.mockResolvedValue([
      { id: 'gemini-3.6-flash-high', display_name: 'Gemini 3.6 Flash High' },
      { id: 'gemini-3.6-flash-low', display_name: 'Gemini 3.6 Flash Low' }
    ])
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any

    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(modelOptionIds(wrapper)).toEqual([
      'gemini-3.6-flash',
      'gemini-3.1-pro',
      'gemini-3.5-flash',
      'gpt-oss-120b-medium',
      'gemini-pro-agent'
    ])
    expect(effortOptionValues(wrapper)).toEqual(['', 'low', 'medium', 'high'])
    // The account's own family default is shown; the global default is not invented.
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain('Use default (medium)')

    await wrapper.get('[data-testid="account-test-effort"]').setValue('high')
    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(sentBody()).toMatchObject({ model_id: 'gemini-3.6-flash', reasoning_effort: 'high' })
  })

  it('每个家族的已公布档位与默认档位相互独立', async () => {
    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    const modelSelect = wrapper.findAllComponents(SelectStub)[0]
    await modelSelect.setValue('gemini-3.1-pro')
    await flushPromises()

    expect(effortOptionValues(wrapper)).toEqual(['', 'high', 'low'])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain('Use default (high)')

    await modelSelect.setValue('gemini-3.5-flash')
    await flushPromises()

    // Router tokens, not display labels: the upstream display name is "Low".
    expect(effortOptionValues(wrapper)).toEqual(['', 'extra-low', 'low'])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain(
      'Use default (extra-low)'
    )
  })

  it('字面量 gpt-oss-120b-medium 不提供档位也不发送 effort', async () => {
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any

    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAllComponents(SelectStub)[0].setValue('gpt-oss-120b-medium')
    await flushPromises()

    expect(wrapper.find('[data-testid="account-test-effort"]').exists()).toBe(false)
    expect(cellText(wrapper, 'effective-effort')).toContain('not provided')

    await startButton(wrapper).trigger('click')
    await flushPromises()

    const body = sentBody()
    expect(body.model_id).toBe('gpt-oss-120b-medium')
    expect(body).not.toHaveProperty('reasoning_effort')
  })

  it('字面量模型的原生档位不套用全局档位名策略', async () => {
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any

    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAllComponents(SelectStub)[0].setValue('gemini-pro-agent')
    await flushPromises()

    // `minimal` is a native level the global suffix names do not even list.
    expect(effortOptionValues(wrapper)).toEqual(['', 'minimal', 'low'])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain(
      'Use default (minimal)'
    )

    await wrapper.get('[data-testid="account-test-effort"]').setValue('minimal')
    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(sentBody()).toMatchObject({
      model_id: 'gemini-pro-agent',
      reasoning_effort: 'minimal'
    })
  })

  it('全局档位名过滤账号档位，共享 {effort} 默认随全局设置更新', async () => {
    getAntigravityModelEffortSettings.mockResolvedValue({
      levels: ['low', 'medium', 'high', 'max'],
      default_effort: 'high'
    })
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any

    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    // The account publishes low/medium/high: `max` is globally enabled but this
    // family never publishes it, so it is not offered.
    expect(effortOptionValues(wrapper)).toEqual(['', 'low', 'medium', 'high'])
    // The bare {effort} template resolves the shared global default, which moved
    // after the snapshot was written (the stale metadata default was `medium`).
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain('Use default (high)')

    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(sentBody()).not.toHaveProperty('reasoning_effort')
  })

  it('模板自带的默认档位不被全局默认覆盖', async () => {
    getAntigravityModelEffortSettings.mockResolvedValue({
      levels: ['low', 'medium', 'high'],
      default_effort: 'low'
    })
    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAllComponents(SelectStub)[0].setValue('gemini-3.1-pro')
    await flushPromises()

    expect(effortOptionValues(wrapper)).toEqual(['', 'high', 'low'])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).toContain('Use default (high)')
  })

  it('账号公布但全局未启用的档位不提供，默认无法解析时不显示档位', async () => {
    getAntigravityModelEffortSettings.mockResolvedValue({
      levels: ['low', 'high'],
      default_effort: 'high'
    })
    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAllComponents(SelectStub)[0].setValue('gemini-3.5-flash')
    await flushPromises()

    // `extra-low` is published for this family but no longer globally enabled.
    expect(effortOptionValues(wrapper)).toEqual(['', 'low'])
    const effort = wrapper.get('[data-testid="account-test-effort"]')
    expect(effort.text()).toContain('Use default')
    expect(effort.text()).not.toContain('Use default (')
    expect(cellText(wrapper, 'effective-effort')).toContain('Use default')
  })

  it('全局档位设置不可用时只提供 Use default，并说明原因', async () => {
    getAntigravityModelEffortSettings.mockRejectedValue(new Error('settings unavailable'))
    const wrapper = mountModal(buildAntigravityAccount())
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(effortOptionValues(wrapper)).toEqual([''])
    expect(wrapper.get('[data-testid="account-test-effort-hint"]').text()).toContain(
      'none of them is enabled in the global effort settings'
    )
  })

  it('只提供 Use default，当账号没有公布该模型的档位时', async () => {
    const wrapper = mountModal(
      buildAntigravityAccount({
        extra: {
          upstream_model_metadata: {
            models: {
              ...AG_CATALOG,
              'gemini-3.6-flash': {
                id: 'gemini-3.6-flash',
                source: 'upstream',
                reasoning_levels_source: 'upstream',
                supported_reasoning_levels: []
              }
            }
          }
        }
      })
    )
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(effortOptionValues(wrapper)).toEqual([''])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).not.toContain('Use default (')
    expect(wrapper.get('[data-testid="account-test-effort-hint"]').text()).toContain(
      'published reasoning levels of this account are empty'
    )
    expect(cellText(wrapper, 'effective-effort')).toContain('Use default')
  })

  it('不把模型目录兜底的档位当作账号能力', async () => {
    const wrapper = mountModal(
      buildAntigravityAccount({
        extra: {
          upstream_model_metadata: {
            models: {
              ...AG_CATALOG,
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
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(effortOptionValues(wrapper)).toEqual([''])
    expect(wrapper.get('[data-testid="account-test-effort"]').text()).not.toContain('Use default (')
    expect(wrapper.get('[data-testid="account-test-effort-hint"]').text()).toContain(
      'registry fallback'
    )
  })

  it('其它提供方的档位来自模型能力元数据', async () => {
    getTestAvailableModels.mockResolvedValue([
      {
        id: 'gpt-5.4',
        display_name: 'GPT-5.4',
        supported_reasoning_levels: [{ effort: 'low' }, { effort: 'xhigh' }],
        default_reasoning_level: 'low'
      }
    ])

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    const effort = wrapper.get('[data-testid="account-test-effort"]')
    expect(effort.findAll('option').map((option) => option.attributes('value'))).toEqual([
      '',
      'low',
      'xhigh'
    ])
    expect(effort.text()).toContain('Use default (low)')
  })

  it('Use default 不发送 reasoning_effort 字段', async () => {
    getTestAvailableModels.mockResolvedValue([
      {
        id: 'gpt-5.4',
        display_name: 'GPT-5.4',
        supported_reasoning_levels: ['low', 'high']
      }
    ])
    global.fetch = vi.fn().mockResolvedValue(
      streamResponse(['data: {"type":"test_complete","success":true}\n'])
    ) as any

    const wrapper = mountModal({
      id: 42,
      name: 'OpenAI OAuth',
      platform: 'openai',
      type: 'oauth',
      status: 'active'
    })
    await wrapper.setProps({ show: true })
    await flushPromises()

    await startButton(wrapper).trigger('click')
    await flushPromises()

    expect(sentBody()).not.toHaveProperty('reasoning_effort')
  })
})
