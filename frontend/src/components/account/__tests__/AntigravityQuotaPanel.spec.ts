import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AntigravityQuotaPanel from '../AntigravityQuotaPanel.vue'
import UsageProgressBar from '../UsageProgressBar.vue'
import type { AccountUsageInfo, AntigravityQuotaWindow } from '@/types'
import accounts from '@/i18n/locales/en/admin/accounts'
import { antigravityWindowPercent, antigravityCreditAmount } from '@/utils/antigravityUsage'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({
    locale: { value: 'en' },
    t: (key: string, args: Record<string, string> = {}) => {
      const messages: Record<string, unknown> = { admin: accounts, common: { unknown: 'Unknown' } }
      let value: unknown = messages
      for (const part of key.split('.')) value = (value as Record<string, unknown>)?.[part]
      return typeof value === 'string' ? value.replace(/\{(\w+)\}/g, (_, name: string) => args[name] ?? '') : key
    }
  })
}))

const empty = (): AccountUsageInfo => ({ five_hour: null, seven_day: null, seven_day_sonnet: null })
const observation = (remaining_fraction: number, reset_time?: string): AntigravityQuotaWindow => ({
  source_bucket_id: '3p-5h', remaining_fraction, reset_time, observed_at: '2026-09-05T12:00:00Z'
})
const render = (usage: AccountUsageInfo) => mount(AntigravityQuotaPanel, {
  props: { usage }
})

describe('explicit Antigravity quota presentation', () => {
  it('expired exhausted model details use the real remaining-mode child and stay pending', async () => {
    const wrapper = render({ ...empty(), antigravity_quota: {
      'claude-sonnet': { utilization: 100, reset_time: '2026-09-04T12:00:00Z' }
    } })
    await wrapper.get('summary').trigger('click')
    const details = wrapper.get('details')
    expect(details.text()).toContain('0%')
    expect(details.text()).toContain('usage.resetPending')
    expect(details.text()).not.toContain('usage.resetNow')
    wrapper.unmount()
  })
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(new Date('2026-09-05T12:00:00Z')) })
  afterEach(() => { vi.useRealTimers() })

  it('uses compact shared bars and keeps exact windows and typed credits in closed details', () => {
    const wrapper = render({ ...empty(), antigravity_window_state: 'available', antigravity_windows: {
      claude_5h: observation(0, '2026-09-05T13:00:00Z'),
      claude_weekly: observation(0.256),
      gemini_5h: observation(0.42, '2026-09-07T12:00:00Z'),
      gemini_weekly: observation(1)
    }, ai_credits: [{ credit_type: 'GOOGLE_ONE_AI', amount_text: '0' }, { credit_type: 'OTHER', amount_text: '12.340' }] })
    expect(wrapper.get('[data-testid="quota-windows"]').findAll('[data-window]')).toHaveLength(4)
    expect(wrapper.get('details').attributes('open')).toBeUndefined()
    expect(wrapper.get('[data-testid="quota-compact"]').findAllComponents(UsageProgressBar).map(bar => bar.props('utilization'))).toEqual([0, 42])
    expect(wrapper.get('[data-testid="quota-compact"]').text()).not.toContain('GOOGLE_ONE_AI')
    expect(wrapper.findAll('section').map(node => node.attributes('aria-label'))).toEqual(['Claude', 'Gemini'])
    expect(wrapper.findAll('[role="progressbar"]').map(node => node.attributes('aria-valuenow'))).toEqual(['0', '25.6', '42', '100'])
    expect(wrapper.get('[data-window="gemini_5h"]').text()).toContain('2d 0h')
    expect(wrapper.get('[data-window="gemini_5h"]').text()).toContain('42%')
    expect(wrapper.text()).toContain('GOOGLE_ONE_AI0')
    expect(wrapper.text()).toContain('OTHER12.340')
    expect(wrapper.text()).not.toContain('12.34%')
    wrapper.unmount()
  })

  it('shows real model fallback without fabricating windows', () => {
    const wrapper = render({ ...empty(), antigravity_quota: {
      'claude-sonnet-high': { utilization: 20 }, 'claude-sonnet-low': { utilization: 40 }
    } })
    expect(wrapper.findAll('[role="progressbar"]')).toHaveLength(0)
    expect(wrapper.findAll('[data-window]').every(node => node.text().includes('Unknown'))).toBe(true)
    expect(wrapper.text()).toContain('Window quotas unavailable')
    expect(wrapper.get('summary').text()).toBe('Model quotas (remaining)')
    expect(wrapper.get('[data-testid="quota-compact"]').text()).toContain('80%')
    expect(wrapper.get('[data-testid="quota-compact"]').text()).not.toContain('5h')
    wrapper.unmount()
  })

  it('does not spend collapsed rows on missing windows and keeps stale status visible', () => {
    const wrapper = render({ ...empty(), antigravity_window_state: 'partial', antigravity_windows: {
      claude_5h: { ...observation(0), stale: true }
    } })
    expect(wrapper.get('[data-testid="quota-compact"]').findAllComponents(UsageProgressBar)).toHaveLength(1)
    expect(wrapper.get('summary').text()).toContain('Stale')
    expect(wrapper.get('[data-window="gemini_5h"]').text()).toContain('Unknown')
    expect(wrapper.get('[data-window="claude_5h"]').text()).toContain('Source: 3p-5h')
    wrapper.unmount()
  })

  it('bounds default model rows but keeps every full model name in details', () => {
    const quota = Object.fromEntries(['a', 'b', 'c', 'd'].map(id => [`model-${id}`, { utilization: 30 }]))
    const wrapper = render({ ...empty(), antigravity_quota: quota })
    expect(wrapper.get('[data-testid="quota-compact"]').findAllComponents(UsageProgressBar)).toHaveLength(2)
    for (const id of Object.keys(quota)) expect(wrapper.get('details').text()).toContain(id)
    wrapper.unmount()
  })

  it('selects the lowest known window per family without inventing missing values or reset times', () => {
    const wrapper = render({ ...empty(), antigravity_windows: {
      claude_5h: observation(0.87), claude_weekly: observation(0.25, 'bad'),
      gemini_5h: observation(NaN), gemini_weekly: observation(0.35)
    } })
    const bars = wrapper.get('[data-testid="quota-compact"]').findAllComponents(UsageProgressBar)
    expect(bars.map(bar => [bar.props('label'), bar.props('utilization')])).toEqual([['C 7d', 25], ['G 7d', 35]])
    expect(bars[0].props('resetsAt')).toBeNull()
    expect(wrapper.text()).not.toContain('NaN')
    wrapper.unmount()
  })

  it('preserves stale exhausted observations after reset and displays their observation time', async () => {
    const wrapper = render({ ...empty(), error: 'network_error', antigravity_window_state: 'unavailable', antigravity_windows: {
      claude_5h: { ...observation(0, '2026-09-05T11:00:00Z'), stale: true }
    } })
    const window = wrapper.get('[data-window="claude_5h"]')
    expect(window.text()).toContain('0%')
    expect(window.text()).toContain('Reset time passed')
    expect(window.text()).toContain('Observed')
    expect(window.text()).toContain(accounts.accounts.usageWindow.antigravityStale)
    await vi.advanceTimersByTimeAsync(3600_000)
    expect(window.get('[role="progressbar"]').attributes('aria-valuenow')).toBe('0')
    wrapper.unmount()
  })

  it('rejects invalid fractions and distinguishes invalid credits from real zero', () => {
    for (const value of [NaN, Infinity, -0.1, 1.1]) {
      expect(antigravityWindowPercent({ ...empty(), antigravity_windows: { claude_5h: observation(value) } }, 'claude_5h')).toBeNull()
    }
    expect(antigravityCreditAmount({ amount_text: '' })).toBeNull()
    expect(antigravityCreditAmount({ amount_text: 'bad', amount: 0 })).toBeNull()
    expect(antigravityCreditAmount({ amount_text: '0' })).toBe('0')
    expect(antigravityCreditAmount({ amount: 0 })).toBe('0')
    expect(antigravityCreditAmount({ amount_text: '9007199254740993.123' })).toBe('9007199254740993.123')
  })
})
