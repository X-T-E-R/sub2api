import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AntigravityQuotaPanel from '../AntigravityQuotaPanel.vue'
import UsageProgressBar from '../UsageProgressBar.vue'
import type { AccountUsageInfo, AntigravityQuotaWindow } from '@/types'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const empty = (): AccountUsageInfo => ({ five_hour: null, seven_day: null, seven_day_sonnet: null })
const observation = (remaining_fraction: number, reset_time?: string): AntigravityQuotaWindow => ({
  source_bucket_id: 'synthetic-window', remaining_fraction, reset_time
})
const render = (usage: AccountUsageInfo) => mount(AntigravityQuotaPanel, { props: { usage } })

describe('fixed Antigravity quota rows', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(new Date('2026-09-05T12:00:00Z')) })
  afterEach(() => { vi.useRealTimers() })

  it('always displays all four windows in the requested order without disclosure controls', () => {
    const wrapper = render({ ...empty(), antigravity_windows: {
      claude_5h: observation(1), gemini_5h: observation(0),
      claude_weekly: observation(0.256), gemini_weekly: observation(0.9968689)
    } })
    const bars = wrapper.findAllComponents(UsageProgressBar)
    expect(bars.map(bar => bar.props('label'))).toEqual(['C 5h', 'G 5h', 'C 1w', 'G 1w'])
    expect(bars.map(bar => bar.text().match(/\d+%/)?.[0])).toEqual(['100%', '0%', '25%', '99%'])
    for (const [index, expected] of [100, 0, 25.6, 99.68689].entries()) {
      expect(bars[index].props('utilization')).toBeCloseTo(expected, 10)
    }
    expect(wrapper.find('details').exists()).toBe(false)
    expect(wrapper.find('summary').exists()).toBe(false)
    expect(wrapper.find('button').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps four labelled unknown rows when windows are unavailable, without inventing model-derived windows', () => {
    const wrapper = render({ ...empty(), antigravity_quota: { 'claude-sonnet': { utilization: 20 } } })
    const rows = wrapper.findAll('[data-window]')
    expect(rows.map(row => row.attributes('data-window'))).toEqual(['claude_5h', 'gemini_5h', 'claude_weekly', 'gemini_weekly'])
    expect(rows.every(row => row.text().includes('common.unknown'))).toBe(true)
    for (const label of ['C 5h', 'G 5h', 'C 1w', 'G 1w']) expect(wrapper.text()).toContain(label)
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
    expect(wrapper.text()).not.toContain('80%')
    wrapper.unmount()
  })

  it('keeps partial and invalid windows visible as unknown without hiding valid zeros', () => {
    const wrapper = render({ ...empty(), antigravity_window_state: 'partial', antigravity_windows: {
      claude_5h: observation(0), gemini_5h: observation(NaN), claude_weekly: observation(1.1)
    } })
    expect(wrapper.findAll('[data-window]')).toHaveLength(4)
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(1)
    expect(wrapper.get('[data-window="claude_5h"]').text()).toContain('0%')
    expect(wrapper.get('[data-window="gemini_5h"]').text()).toContain('common.unknown')
    expect(wrapper.text()).not.toContain('NaN')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.quotaPartialCompact')
    wrapper.unmount()
  })

  it('preserves reset countdowns and stale exhausted values after their reset time', async () => {
    const wrapper = render({ ...empty(), antigravity_windows: {
      claude_5h: { ...observation(0, '2026-09-05T11:00:00Z'), stale: true },
      gemini_5h: observation(0.42, '2026-09-05T13:00:00Z'),
      claude_weekly: observation(0.25, 'bad')
    } })
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.quotaStaleCompact')
    expect(wrapper.get('[data-window="claude_5h"]').text()).toContain('usage.resetPending')
    const bars = wrapper.findAllComponents(UsageProgressBar)
    expect(bars[1].props('resetsAt')).toBe('2026-09-05T13:00:00Z')
    expect(bars[2].props('resetsAt')).toBeNull()
    await vi.advanceTimersByTimeAsync(3600_000)
    expect(wrapper.get('[data-window="claude_5h"]').text()).toContain('0%')
    expect(bars[0].props('utilization')).toBe(0)
    wrapper.unmount()
  })

  it.each([[0, '0%'], [1, '100%'], [0.99999, '99%'], [0.256, '25%']])(
    'preserves fraction %s and formats only the percentage label', (fraction, label) => {
      const wrapper = render({ ...empty(), antigravity_windows: { claude_5h: observation(fraction as number) } })
      const bar = wrapper.getComponent(UsageProgressBar)
      expect(bar.text()).toContain(label)
      expect(bar.props('utilization')).toBeCloseTo((fraction as number) * 100, 10)
      wrapper.unmount()
    }
  )
})
