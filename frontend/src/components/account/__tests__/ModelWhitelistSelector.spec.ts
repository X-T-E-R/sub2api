import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { SyncUpstreamModelsResult } from '@/api/admin/accounts'

const {
  copyToClipboard,
  showError,
  showSuccess,
  showInfo,
  showWarning,
  syncUpstreamModels,
  syncUpstreamModelsPreview
} = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
  showWarning: vi.fn(),
  syncUpstreamModels: vi.fn(),
  syncUpstreamModelsPreview: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      // Keep the params visible so tests can assert the interpolated count.
      t: (key: string, params?: Record<string, unknown>) =>
        key === 'common.copy'
          ? '复制'
          : params
            ? `${key}:${JSON.stringify(params)}`
            : key
    })
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showInfo,
    showWarning
  })
}))

vi.mock('@/api/admin/accounts', () => ({
  accountsAPI: {
    syncUpstreamModels,
    syncUpstreamModelsPreview
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard
  })
}))

import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

type CatalogResult = SyncUpstreamModelsResult

function liveCatalogResult(overrides: Partial<CatalogResult> = {}): CatalogResult {
  return {
    models: ['gemini-3.8-flash', 'gemini-3.6-flash'],
    source: 'upstream',
    live: true,
    model_mapping: {
      'gemini-3.8-flash': 'gemini-3.8-flash-{effort}',
      'gemini-3.6-flash': 'gemini-3.6-flash-{effort}'
    },
    ...overrides
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function mountSelector(props: Record<string, unknown> = {}) {
  return mount(ModelWhitelistSelector, {
    props: {
      modelValue: [],
      platform: 'openai',
      ...props,
    },
    global: {
      stubs: {
        ModelIcon: true
      }
    }
  })
}

function findModelRow(wrapper: ReturnType<typeof mountSelector>, modelId: string) {
  const row = wrapper
    .findAll('[data-testid="model-option"]')
    .find(candidate => candidate.text().includes(modelId))

  if (!row) {
    throw new Error(`Model row not found: ${modelId}`)
  }

  return row
}

function findSyncButton(wrapper: ReturnType<typeof mountSelector>) {
  return wrapper
    .findAll('button')
    .find(button => button.text() === 'admin.accounts.syncUpstreamModels')
}

async function triggerSync(wrapper: ReturnType<typeof mountSelector>) {
  const syncButton = findSyncButton(wrapper)
  expect(syncButton).toBeDefined()
  await syncButton!.trigger('click')
  await flushPromises()
}

describe('ModelWhitelistSelector', () => {
  beforeEach(() => {
    copyToClipboard.mockClear()
    showError.mockReset()
    showSuccess.mockReset()
    showInfo.mockReset()
    showWarning.mockReset()
    syncUpstreamModels.mockReset()
    syncUpstreamModelsPreview.mockReset()
  })

  it('copies a model ID without selecting the model', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = findModelRow(wrapper, 'gpt-5.6-sol')

    const copyButton = row.get('[data-testid="copy-model-id"]')
    expect(copyButton.attributes('aria-label')).toBe('复制 gpt-5.6-sol')

    await copyButton.trigger('click')
    await flushPromises()

    expect(copyToClipboard).toHaveBeenCalledWith('gpt-5.6-sol')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('keeps the existing model selection behavior', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = findModelRow(wrapper, 'gpt-5.6-sol')
    await row.get('[data-testid="select-model"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.6-sol']]])
    expect(copyToClipboard).not.toHaveBeenCalled()
  })

  it('replaces the selection with the live projection keys and carries the real targets', async () => {
    syncUpstreamModels.mockResolvedValue(liveCatalogResult())

    const wrapper = mountSelector({ modelValue: ['stale-model'], accountId: 46 })
    await triggerSync(wrapper)

    expect(syncUpstreamModels).toHaveBeenCalledWith(46)
    expect(wrapper.emitted('update:modelValue')).toEqual([
      [['gemini-3.8-flash', 'gemini-3.6-flash']]
    ])
    expect(wrapper.emitted('upstream-synced')).toEqual([
      [
        {
          models: ['gemini-3.8-flash', 'gemini-3.6-flash'],
          modelMapping: {
            'gemini-3.8-flash': 'gemini-3.8-flash-{effort}',
            'gemini-3.6-flash': 'gemini-3.6-flash-{effort}'
          }
        }
      ]
    ])
    expect(showSuccess).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsSuccess:{"count":2,"total":2}'
    )
    expect(showInfo).not.toHaveBeenCalled()
    expect(showWarning).not.toHaveBeenCalled()
  })

  it('warns when model IDs sync but capability metadata is incomplete', async () => {
    syncUpstreamModels.mockResolvedValue(
      liveCatalogResult({
        models: ['x-preview-f-free'],
        model_mapping: { 'x-preview-f-free': 'x-preview-f-free-thinking' },
        warnings: [
          {
            code: 'upstream_model_metadata_incomplete',
            message: 'Model IDs were synced, but capability metadata could not be updated.'
          }
        ]
      })
    )
    const wrapper = mountSelector({ modelValue: [], accountId: 46 })
    await triggerSync(wrapper)

    expect(wrapper.emitted('update:modelValue')).toEqual([
      [['x-preview-f-free']]
    ])
    expect(showWarning).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsMetadataIncomplete')
    expect(showSuccess).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsSuccess:{"count":1,"total":1}'
    )
  })

  it('reports the editing list already matching the live catalog', async () => {
    syncUpstreamModels.mockResolvedValue(liveCatalogResult())

    const wrapper = mountSelector({
      modelValue: ['gemini-3.6-flash', 'gemini-3.8-flash'],
      accountId: 46
    })
    await triggerSync(wrapper)

    expect(wrapper.emitted('update:modelValue')).toEqual([
      [['gemini-3.8-flash', 'gemini-3.6-flash']]
    ])
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsNoChanges')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('reports a successful preview so account creation can persist metadata', async () => {
    syncUpstreamModelsPreview.mockResolvedValue(
      liveCatalogResult({
        models: ['x-preview-f-free'],
        model_mapping: { 'x-preview-f-free': 'x-preview-f-free' },
        metadata: {
          'x-preview-f-free': {
            id: 'x-preview-f-free',
            reasoning: true,
            supported_reasoning_levels: ['low', 'high', 'max'],
          },
        }
      })
    )
    const wrapper = mountSelector({
      syncCredentials: {
        platform: 'openai',
        type: 'apikey',
        base_url: 'https://opencode.ai/zen/v1',
        api_key: 'test-key',
      },
    })
    await triggerSync(wrapper)

    expect(syncUpstreamModelsPreview).toHaveBeenCalledOnce()
    expect(wrapper.emitted('upstream-synced')).toEqual([
      [{ models: ['x-preview-f-free'], modelMapping: { 'x-preview-f-free': 'x-preview-f-free' } }]
    ])
    expect(wrapper.emitted('update:modelValue')).toEqual([[['x-preview-f-free']]])
  })

  it('keeps the selection when the upstream falls back to configured models', async () => {
    syncUpstreamModels.mockResolvedValue({
      models: ['x-configured-fallback'],
      source: 'configured',
      live: false,
      model_mapping: {}
    })

    const wrapper = mountSelector({ modelValue: ['manual-model'], accountId: 46 })
    await triggerSync(wrapper)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsConfiguredFallback')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('keeps the selection when the live list carries no usable mapping', async () => {
    syncUpstreamModels.mockResolvedValue(
      liveCatalogResult({
        models: ['gemini-3.8-flash'],
        model_mapping: { 'gemini-3.8-flash': '' }
      })
    )

    const wrapper = mountSelector({ modelValue: ['manual-model'], accountId: 46 })
    await triggerSync(wrapper)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsConfiguredFallback')
  })

  it('keeps the selection when the live mapping mixes valid and unusable entries', async () => {
    syncUpstreamModels.mockResolvedValue(
      liveCatalogResult({
        model_mapping: {
          'gemini-3.8-flash': 'gemini-3.8-flash-{effort}',
          'gemini-3.6-flash': 'bad target'
        }
      })
    )

    const wrapper = mountSelector({ modelValue: ['manual-model'], accountId: 46 })
    await triggerSync(wrapper)

    // No partial replacement of the editing list.
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsConfiguredFallback')
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('reports an empty live list without touching the selection', async () => {
    syncUpstreamModels.mockResolvedValue(
      liveCatalogResult({ models: [], model_mapping: {} })
    )

    const wrapper = mountSelector({ modelValue: ['manual-model'], accountId: 46 })
    await triggerSync(wrapper)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsEmpty')
  })

  it('discards a preview result that arrives after the credentials changed', async () => {
    const pending = deferred<CatalogResult>()
    syncUpstreamModelsPreview.mockReturnValue(pending.promise)

    const wrapper = mountSelector({
      syncCredentials: {
        platform: 'openai',
        type: 'apikey',
        base_url: 'https://openaikey.example/v1',
        api_key: 'old-key'
      }
    })
    await triggerSync(wrapper)

    await wrapper.setProps({
      syncCredentials: {
        platform: 'openai',
        type: 'apikey',
        base_url: 'https://openaikey.example/v1',
        api_key: 'new-key'
      }
    })
    pending.resolve(liveCatalogResult())
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('discards an account result that arrives after the modal closed', async () => {
    const pending = deferred<CatalogResult>()
    syncUpstreamModels.mockReturnValue(pending.promise)

    const wrapper = mountSelector({ modelValue: ['manual-model'], accountId: 46 })
    await triggerSync(wrapper)

    wrapper.unmount()
    pending.resolve(liveCatalogResult())
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
  })

  it('discards a result for the previous account', async () => {
    const pending = deferred<CatalogResult>()
    syncUpstreamModels.mockReturnValue(pending.promise)

    const wrapper = mountSelector({ modelValue: ['manual-model'], accountId: 46 })
    await triggerSync(wrapper)

    await wrapper.setProps({ accountId: 47 })
    pending.resolve(liveCatalogResult())
    await flushPromises()

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.emitted('upstream-synced')).toBeUndefined()
    expect(showSuccess).not.toHaveBeenCalled()
  })
})
