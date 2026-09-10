import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpsPoolResetStatsCard from '../OpsPoolResetStatsCard.vue'

const mockGetPoolResetStats = vi.fn()

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getPoolResetStats: (...args: any[]) => mockGetPoolResetStats(...args),
  },
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (key === 'admin.ops.poolResetStats.persistence' && params) {
        return `${key}:${String(params.storage)}`
      }
      if (key === 'admin.ops.poolResetStats.never') return 'never'
      if (key === 'common.unknown') return 'unknown'
      return key
    },
  }),
}))

vi.mock('@/components/icons/Icon.vue', () => ({
  default: {
    props: ['name', 'size'],
    template: '<span class="icon-stub" />',
  },
}))

const sampleStats = {
  triggered: 4,
  suppressed: 2,
  last_reset_at: '2026-09-11T01:02:03Z',
  by_account: [
    {
      account_id: 17,
      triggered: 3,
      suppressed: 1,
      last_reset_at: '2026-09-11T01:02:03Z',
    },
  ],
  by_protocol: [
    {
      protocol: 'openai_h2',
      triggered: 4,
      suppressed: 2,
      last_reset_at: '2026-09-11T01:02:03Z',
    },
  ],
  persistence: 'process_memory',
  reset_on_restart: true,
}

describe('OpsPoolResetStatsCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('loads and renders totals plus account and protocol summaries', async () => {
    mockGetPoolResetStats.mockResolvedValue(sampleStats)

    const wrapper = mount(OpsPoolResetStatsCard, {
      props: { refreshToken: 0 },
    })
    await flushPromises()

    expect(mockGetPoolResetStats).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="pool-reset-triggered"]').text()).toBe('4')
    expect(wrapper.get('[data-testid="pool-reset-suppressed"]').text()).toBe('2')
    expect(wrapper.get('[data-testid="pool-reset-last"]').text()).not.toContain('never')
    expect(wrapper.text()).toContain('#17')
    expect(wrapper.text()).toContain('openai_h2')
    expect(wrapper.get('[data-testid="pool-reset-persistence"]').text()).toContain('process_memory')
  })

  it('reloads when the dashboard refresh token changes', async () => {
    mockGetPoolResetStats.mockResolvedValue(sampleStats)

    const wrapper = mount(OpsPoolResetStatsCard, {
      props: { refreshToken: 0 },
    })
    await flushPromises()
    await wrapper.setProps({ refreshToken: 1 })
    await flushPromises()

    expect(mockGetPoolResetStats).toHaveBeenCalledTimes(2)
  })

  it('renders the API error', async () => {
    mockGetPoolResetStats.mockRejectedValue(new Error('stats unavailable'))

    const wrapper = mount(OpsPoolResetStatsCard, {
      props: { refreshToken: 0 },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('stats unavailable')
  })
})
