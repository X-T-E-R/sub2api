import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import type { SyncUpstreamModelsResult } from '@/api/admin/accounts'

const {
  updateAccountMock,
  checkMixedChannelRiskMock,
  syncUpstreamModelsMock,
  selectorSyncUpstreamModelsMock,
  showError,
  showSuccess,
  showInfo,
  showWarning,
  authIsSimpleMode
} = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  selectorSyncUpstreamModelsMock: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
  showWarning: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showInfo,
    showWarning
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock,
      syncUpstreamModels: syncUpstreamModelsMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn(),
  accountsAPI: {
    syncUpstreamModels: selectorSyncUpstreamModelsMock,
    syncUpstreamModelsPreview: selectorSyncUpstreamModelsMock
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import EditAccountModal from '../EditAccountModal.vue'
import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: {
      type: [String, Number, Boolean, null],
      default: ''
    },
    options: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
  `
})

/**
 * Live catalog as the backend returns it: real raw IDs plus the
 * request→upstream projection (AG effort families collapse into templates).
 */
const LIVE_AG_CATALOG: SyncUpstreamModelsResult = {
  models: ['claude-sonnet-4-5', 'gemini-3.8-flash-low', 'gemini-3.8-flash-high'],
  metadata: { 'gemini-3.8-flash': { id: 'gemini-3.8-flash', source: 'upstream' } },
  source: 'upstream',
  live: true,
  model_mapping: {
    'claude-sonnet-4-5': 'claude-sonnet-4-5',
    'gemini-3.8-flash': 'gemini-3.8-flash-{effort}'
  }
}

const LIVE_OPENAI_CATALOG: SyncUpstreamModelsResult = {
  models: ['gpt-5.2', 'gpt-5.2-2025-12-11'],
  source: 'upstream',
  live: true,
  model_mapping: {
    'gpt-5.2': 'gpt-5.2',
    'gpt-5.2-2025-12-11': 'gpt-5.2-2025-12-11'
  }
}

function buildAntigravityAPIKeyAccount(
  id: number,
  modelMapping: Record<string, string> = {}
) {
  return {
    id,
    name: `Antigravity Upstream ${id}`,
    notes: '',
    platform: 'antigravity',
    type: 'apikey',
    credentials: {
      api_key: 'sk-ag',
      base_url: 'https://cloudcode-pa.googleapis.com',
      model_mapping: modelMapping
    },
    credentials_status: { has_api_key: true },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function buildOpenAIAPIKeyAccount() {
  return {
    id: 9,
    name: 'OpenAI Key',
    notes: '',
    platform: 'openai',
    type: 'apikey',
    credentials: { api_key: 'sk-test', base_url: 'https://api.openai.com' },
    credentials_status: { has_api_key: true },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

function mountModal(account: any) {
  return mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true
      }
    }
  })
}

function findSyncButtons(wrapper: ReturnType<typeof mountModal>) {
  return wrapper
    .findAll('button')
    .filter((button) => button.text() === 'admin.accounts.syncUpstreamModels')
}

function readAntigravityRows(wrapper: ReturnType<typeof mountModal>) {
  const fromInputs = wrapper.findAll('input[placeholder="admin.accounts.requestModel"]')
  const toInputs = wrapper.findAll('input[placeholder="admin.accounts.actualModel"]')
  return fromInputs.map((input, index) => ({
    from: (input.element as HTMLInputElement).value,
    to: (toInputs[index]!.element as HTMLInputElement).value
  }))
}

async function triggerAgSync(wrapper: ReturnType<typeof mountModal>) {
  const buttons = findSyncButtons(wrapper)
  expect(buttons.length).toBe(1)
  await buttons[0]!.trigger('click')
  await flushPromises()
}

async function submit(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('form#edit-account-form').trigger('submit.prevent')
  await flushPromises()
}

describe('EditAccountModal upstream model catalog', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    syncUpstreamModelsMock.mockReset()
    selectorSyncUpstreamModelsMock.mockReset()
    showError.mockReset()
    showSuccess.mockReset()
    showInfo.mockReset()
    showWarning.mockReset()
  })

  it('persists the live AG mapping targets only when the ordinary Save runs', async () => {
    const account = buildAntigravityAPIKeyAccount(3)
    updateAccountMock.mockResolvedValue(account)
    syncUpstreamModelsMock.mockResolvedValue(LIVE_AG_CATALOG)

    const wrapper = mountModal(account)
    expect(readAntigravityRows(wrapper)).toEqual([])

    await triggerAgSync(wrapper)

    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(3)
    // No update API call until the user saves.
    expect(updateAccountMock).not.toHaveBeenCalled()

    // Rows are the backend projection, including the {effort} template target.
    expect(readAntigravityRows(wrapper)).toEqual([
      { from: 'claude-sonnet-4-5', to: 'claude-sonnet-4-5' },
      { from: 'gemini-3.8-flash', to: 'gemini-3.8-flash-{effort}' }
    ])
    expect(showSuccess).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsSuccess')

    await submit(wrapper)

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'claude-sonnet-4-5': 'claude-sonnet-4-5',
      'gemini-3.8-flash': 'gemini-3.8-flash-{effort}'
    })
  })

  it('replaces the AG mapping table instead of appending self mappings', async () => {
    const account = buildAntigravityAPIKeyAccount(3, { 'legacy-model': 'legacy-target' })
    updateAccountMock.mockResolvedValue(account)
    syncUpstreamModelsMock.mockResolvedValue(LIVE_AG_CATALOG)

    const wrapper = mountModal(account)
    expect(readAntigravityRows(wrapper)).toEqual([{ from: 'legacy-model', to: 'legacy-target' }])

    await triggerAgSync(wrapper)

    expect(readAntigravityRows(wrapper)).toEqual([
      { from: 'claude-sonnet-4-5', to: 'claude-sonnet-4-5' },
      { from: 'gemini-3.8-flash', to: 'gemini-3.8-flash-{effort}' }
    ])
    // Raw IDs the backend did not project as keys never become rows.
    expect(readAntigravityRows(wrapper).some((row) => row.from === 'gemini-3.8-flash-low')).toBe(false)
    expect(updateAccountMock).not.toHaveBeenCalled()

    await submit(wrapper)

    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'claude-sonnet-4-5': 'claude-sonnet-4-5',
      'gemini-3.8-flash': 'gemini-3.8-flash-{effort}'
    })
  })

  it('keeps the AG mapping table when the upstream falls back to configured models', async () => {
    const account = buildAntigravityAPIKeyAccount(3, { 'manual-model': 'manual-target' })
    updateAccountMock.mockResolvedValue(account)
    syncUpstreamModelsMock.mockResolvedValue({
      models: ['manual-target'],
      source: 'configured',
      live: false,
      model_mapping: {}
    })

    const wrapper = mountModal(account)
    await triggerAgSync(wrapper)

    expect(readAntigravityRows(wrapper)).toEqual([{ from: 'manual-model', to: 'manual-target' }])
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsConfiguredFallback')
    expect(showSuccess).not.toHaveBeenCalled()
    expect(updateAccountMock).not.toHaveBeenCalled()
  })

  it('keeps the AG mapping table when the live mapping mixes valid and unusable entries', async () => {
    const account = buildAntigravityAPIKeyAccount(3, { 'manual-model': 'manual-target' })
    updateAccountMock.mockResolvedValue(account)
    syncUpstreamModelsMock.mockResolvedValue({
      ...LIVE_AG_CATALOG,
      model_mapping: {
        'claude-sonnet-4-5': 'claude-sonnet-4-5',
        'gemini-3.8-flash': 'bad target'
      }
    })

    const wrapper = mountModal(account)
    await triggerAgSync(wrapper)

    // Nothing is applied from a partially usable projection.
    expect(readAntigravityRows(wrapper)).toEqual([{ from: 'manual-model', to: 'manual-target' }])
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsConfiguredFallback')
    expect(showSuccess).not.toHaveBeenCalled()
    expect(updateAccountMock).not.toHaveBeenCalled()
  })

  it('does not let an in-flight catalog result overwrite another account editing state', async () => {
    const firstAccount = buildAntigravityAPIKeyAccount(3, { 'first-model': 'first-target' })
    const secondAccount = buildAntigravityAPIKeyAccount(4, { 'second-model': 'second-target' })
    updateAccountMock.mockResolvedValue(secondAccount)

    const pending = deferred<SyncUpstreamModelsResult>()
    syncUpstreamModelsMock.mockReturnValue(pending.promise)

    const wrapper = mountModal(firstAccount)
    const buttons = findSyncButtons(wrapper)
    expect(buttons.length).toBe(1)
    await buttons[0]!.trigger('click')
    await flushPromises()

    await wrapper.setProps({ account: secondAccount })
    await flushPromises()

    pending.resolve(LIVE_AG_CATALOG)
    await flushPromises()

    expect(readAntigravityRows(wrapper)).toEqual([{ from: 'second-model', to: 'second-target' }])
    expect(showSuccess).not.toHaveBeenCalled()
    expect(showInfo).not.toHaveBeenCalled()
  })

  it('replaces the whitelist selection with the live keys and saves identity mappings', async () => {
    const account = buildOpenAIAPIKeyAccount()
    updateAccountMock.mockResolvedValue(account)
    selectorSyncUpstreamModelsMock.mockResolvedValue(LIVE_OPENAI_CATALOG)

    const wrapper = mountModal(account)
    const selector = wrapper.findComponent(ModelWhitelistSelector)
    expect(selector.exists()).toBe(true)

    const syncButton = selector
      .findAll('button')
      .find((button) => button.text() === 'admin.accounts.syncUpstreamModels')
    expect(syncButton).toBeDefined()
    await syncButton!.trigger('click')
    await flushPromises()

    expect(selectorSyncUpstreamModelsMock).toHaveBeenCalledWith(9)
    expect(syncUpstreamModelsMock).not.toHaveBeenCalled()
    expect(updateAccountMock).not.toHaveBeenCalled()
    expect(selector.props('modelValue')).toEqual(['gpt-5.2', 'gpt-5.2-2025-12-11'])

    await submit(wrapper)

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.model_mapping).toEqual({
      'gpt-5.2': 'gpt-5.2',
      'gpt-5.2-2025-12-11': 'gpt-5.2-2025-12-11'
    })
  })
})
