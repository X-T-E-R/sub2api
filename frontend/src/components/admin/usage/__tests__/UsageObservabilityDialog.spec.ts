import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import UsageObservabilityDialog from '../UsageObservabilityDialog.vue'
import { adminUsageAPI } from '@/api/admin/usage'

vi.mock('@/api/admin/usage', () => ({
  adminUsageAPI: { getObservability: vi.fn() }
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('UsageObservabilityDialog', () => {
  beforeEach(() => vi.mocked(adminUsageAPI.getObservability).mockReset())

  it('loads the bounded ledger only when opened', async () => {
    vi.mocked(adminUsageAPI.getObservability).mockResolvedValue({
      id: 42,
      duration_ms: 50,
      forward_duration_ms: 50,
      handler_duration_ms: 175,
      first_token_ms: null,
      first_visible_output_ms: null,
      semantic_output_seen: false,
      terminal_kind: 'response.completed',
      attempt_count: 2,
      account_switch_count: 1,
      failed_attempt_duration_ms: 100,
      retry_wait_ms: 0,
      account_switch_ms: 25,
      gateway_request_id: 'gateway-1',
      client_request_id: 'client-1',
      attempt_ledger: {
        version: 1,
        total_attempts: 2,
        truncated: false,
        attempts: [
          { sequence: 1, account_id: 7, platform: 'openai', started_offset_ms: 5, selection_ms: 5, slot_wait_ms: 0, forward_ms: 100, outcome: 'failover', reason: 'http_503', semantic_output_seen: false },
          { sequence: 2, account_id: 8, platform: 'openai', started_offset_ms: 130, selection_ms: 20, slot_wait_ms: 5, forward_ms: 50, outcome: 'success', terminal_kind: 'response.completed', semantic_output_seen: false }
        ]
      }
    })
    const wrapper = mount(UsageObservabilityDialog, {
      props: { show: false, usageId: 42 },
      global: {
        stubs: { Teleport: true }
      }
    })
    expect(adminUsageAPI.getObservability).not.toHaveBeenCalled()
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(adminUsageAPI.getObservability).toHaveBeenCalledWith(42, expect.any(Object))
    expect(wrapper.text()).toContain('response.completed')
    expect(wrapper.text()).toContain('http_503')
  })
})
