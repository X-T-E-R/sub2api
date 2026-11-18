import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import AccountActionMenu from '../AccountActionMenu.vue'
import AccountBulkActionsBar from '../AccountBulkActionsBar.vue'
import AccountWarmupModal from '../AccountWarmupModal.vue'
import { adminAPI } from '@/api/admin'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => ({
  ...(await vi.importActual<typeof import('vue-i18n')>('vue-i18n')),
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key
  })
}))

const makeAccount = (overrides: Partial<Account> = {}): Account => ({
  id: 70,
  name: 'brad',
  platform: 'antigravity',
  type: 'oauth',
  status: 'active',
  schedulable: false,
  concurrency: 3,
  priority: 1,
  created_at: '2026-11-01T00:00:00Z',
  updated_at: '2026-11-18T00:00:00Z',
  ...overrides
})

describe('Account Warmup UI components', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.restoreAllMocks()
  })

  it('shows Warm Up Quota in AccountActionMenu only for Antigravity non-apikey non-shadow accounts', async () => {
    const agyWrapper = mount(AccountActionMenu, {
      props: {
        show: true,
        account: makeAccount({ platform: 'antigravity', type: 'oauth' }),
        position: { top: 10, left: 20 }
      },
      global: {
        stubs: { Teleport: true }
      }
    })
    expect(agyWrapper.text()).toContain('admin.accounts.warmupQuota')
    const warmupBtn = agyWrapper
      .findAll('button')
      .find(btn => btn.text().includes('admin.accounts.warmupQuota'))
    expect(warmupBtn).toBeDefined()
    await warmupBtn!.trigger('click')
    expect(agyWrapper.emitted('warmup')?.[0]?.[0]).toMatchObject({ id: 70 })
    agyWrapper.unmount()

    const apikeyWrapper = mount(AccountActionMenu, {
      props: {
        show: true,
        account: makeAccount({ platform: 'antigravity', type: 'apikey' }),
        position: { top: 10, left: 20 }
      },
      global: {
        stubs: { Teleport: true }
      }
    })
    expect(apikeyWrapper.text()).not.toContain('admin.accounts.warmupQuota')
    apikeyWrapper.unmount()
  })

  it('emits warmup from AccountBulkActionsBar when accounts are selected', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [70, 75],
        totalResults: 10,
        selectingAll: false,
        allResultsSelected: false
      }
    })
    const btn = wrapper
      .findAll('button')
      .find(item => item.text().includes('admin.accounts.bulkActions.warmup'))
    expect(btn).toBeDefined()
    await btn!.trigger('click')
    expect(wrapper.emitted('warmup')).toHaveLength(1)
    wrapper.unmount()
  })

  it('runs single-account warmup and emits refreshed usage map on completion', async () => {
    const warmupSpy = vi.spyOn(adminAPI.accounts, 'warmupAccount').mockResolvedValue({
      total_accounts: 1,
      succeeded_accounts: 1,
      failed_accounts: 0,
      bucket_mode: 'auto_buckets',
      results: [
        {
          account_id: 70,
          account_name: 'brad',
          platform: 'antigravity',
          schedulable: false,
          success: true,
          error: '',
          runs: [
            {
              bucket: 'gemini',
              requested_model: 'gemini-4-carbon',
              resolved_model: 'gemini-4-carbon-low',
              prompt_summary: 'add4d: 7230+8233',
              expected_answer: '15463',
              response_excerpt: '15463',
              answer_matched: true,
              status_code: 200,
              duration_ms: 1840,
              error: ''
            }
          ],
          usage: {
            five_hour: null,
            seven_day: null,
            seven_day_sonnet: null,
            antigravity_windows: {
              gemini_5h: {
                source_bucket_id: 'gemini_5h',
                remaining_fraction: 0.99,
                reset_time: '2026-11-18T10:00:00Z',
                observed_at: '2026-11-18T05:00:00Z'
              }
            }
          },
          usage_error: ''
        }
      ]
    })

    const wrapper = mount(AccountWarmupModal, {
      props: {
        show: true,
        accounts: [makeAccount()]
      },
      global: {
        stubs: { Teleport: true }
      }
    })

    const runBtn = wrapper
      .findAll('button')
      .find(btn => btn.text().includes('admin.accounts.warmup.runButton'))
    expect(runBtn).toBeDefined()
    await runBtn!.trigger('click')
    await flushPromises()

    expect(warmupSpy).toHaveBeenCalledWith(70, {
      bucket_mode: 'auto_buckets',
      effort: 'low',
      refresh_usage: true,
      concurrency: 3
    })
    expect(wrapper.text()).toContain('gemini-4-carbon-low')
    expect(wrapper.text()).toContain('1840 ms')
    expect(wrapper.text()).toContain('99%')
    const completedEvents = wrapper.emitted('completed')
    expect(completedEvents).toHaveLength(1)
    expect(completedEvents![0][0]).toHaveProperty('usageByAccountId.70')
    wrapper.unmount()
  })

  it('runs batch warmup with custom models when multiple accounts are provided', async () => {
    const batchSpy = vi.spyOn(adminAPI.accounts, 'batchWarmupAccounts').mockResolvedValue({
      total_accounts: 2,
      succeeded_accounts: 2,
      failed_accounts: 0,
      bucket_mode: 'custom_models',
      results: []
    })

    const wrapper = mount(AccountWarmupModal, {
      props: {
        show: true,
        accounts: [makeAccount({ id: 70 }), makeAccount({ id: 75, name: 'alice' })]
      },
      global: {
        stubs: { Teleport: true }
      }
    })

    const customRadio = wrapper.find('input[type="radio"][value="custom_models"]')
    await customRadio.setValue()
    const modelInput = wrapper.find('input[type="text"]')
    await modelInput.setValue('gemini-4-carbon, claude-opus-5-5')

    const runBtn = wrapper
      .findAll('button')
      .find(btn => btn.text().includes('admin.accounts.warmup.runButton'))
    await runBtn!.trigger('click')
    await flushPromises()

    expect(batchSpy).toHaveBeenCalledWith({
      account_ids: [70, 75],
      bucket_mode: 'custom_models',
      models: ['gemini-4-carbon', 'claude-opus-5-5'],
      effort: 'low',
      refresh_usage: true,
      concurrency: 3
    })
    wrapper.unmount()
  })
})
