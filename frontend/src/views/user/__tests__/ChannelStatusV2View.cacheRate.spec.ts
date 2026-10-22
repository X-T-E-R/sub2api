import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ChannelStatusV2View from '../ChannelStatusV2View.vue'
import type { MonitorHealth, MonitorMetric } from '@/api/channelMonitorV2'

const { getDimensions, getSnapshot, getMatrix, getModels, getErrors, getUsers, showError, throughputHidden } =
  vi.hoisted(() => ({
    getDimensions: vi.fn(),
    getSnapshot: vi.fn(),
    getMatrix: vi.fn(),
    getModels: vi.fn(),
    getErrors: vi.fn(),
    getUsers: vi.fn(),
    showError: vi.fn(),
    throughputHidden: { value: false },
  }))

vi.mock('@/api/channelMonitorV2', () => ({
  getDimensions,
  getSnapshot,
  getMatrix,
  getModels,
  getErrors,
  getUsers,
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isAdmin: false }),
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError }),
}))
vi.mock('@/utils/featureFlags', () => ({
  isChannelMonitorThroughputHidden: () => throughputHidden.value,
}))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ replace: vi.fn() }),
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
      te: () => false,
      locale: { value: 'zh-CN' },
    }),
  }
})

const health: MonitorHealth = {
  overall: 'healthy',
  error_rate: 'healthy',
  ttft: 'healthy',
  cache: 'healthy',
  minimum_sample: 20,
}

function metrics(cache: {
  rate: number
  hasSamples?: boolean
  denominator?: number
  /** Mirrors public redaction: throughput and absolute counts are zeroed. */
  redacted?: boolean
}): MonitorMetric {
  const hasSamples = cache.hasSamples ?? (cache.denominator ?? 0) > 0
  const redacted = cache.redacted === true
  return {
    success_requests: hasSamples && !redacted ? 10 : 0,
    error_requests: 0,
    request_count: hasSamples && !redacted ? 10 : 0,
    token_count: 0,
    rpm: hasSamples && !redacted ? 2 : 0,
    tpm: hasSamples && !redacted ? 100 : 0,
    error_rate: 0,
    cache_rate: cache.rate,
    cache_rate_has_samples: cache.hasSamples,
    cache_rate_numerator: hasSamples && !redacted ? cache.rate * (cache.denominator ?? 100) : 0,
    cache_rate_denominator: redacted ? 0 : (cache.denominator ?? 0),
    ttft: { sample_count: hasSamples ? 10 : 0, p50_ms: hasSamples ? 100 : null, p95_ms: null, avg_ms: null },
    duration: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
  }
}

async function mountWith(overview: MonitorMetric, row: MonitorMetric) {
  const coverage = {
    requested_start: '2026-10-22T04:00:00Z',
    requested_end: '2026-10-22T05:00:00Z',
    coverage_start: '2026-10-22T04:00:00Z',
    data_through: '2026-10-22T05:00:00Z',
    computed_at: '2026-10-22T05:00:00Z',
    aggregation_lag_seconds: 0,
    coverage_complete: true,
    bucket_seconds: 300,
  }
  getDimensions.mockResolvedValue({ platforms: [], groups: [], models: [] })
  getSnapshot.mockResolvedValue({
    config: { version: 1, enabled: true, refresh_interval_seconds: 300, platforms: [], group_ids: [], health_thresholds: {} },
    coverage,
    metrics: overview,
    health,
    trend: [],
  })
  getMatrix.mockResolvedValue({ coverage, group_by: 'platform', items: [] })
  getModels.mockResolvedValue({
    items: [{ platform: 'anthropic', model: 'claude-sonnet-5', metrics: row, health }],
  })
  getErrors.mockResolvedValue({ items: [] })
  getUsers.mockResolvedValue({ items: [] })

  const wrapper = mount(ChannelStatusV2View, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        Icon: true,
        LoadingSpinner: true,
        Select: true,
        FilterMultiSelect: true,
        MonitorTrendChart: true,
        RelayPulseMatrix: true,
        MonitorRankBadge: true,
      },
    },
  })
  await flushPromises()
  return wrapper
}

describe('ChannelStatusV2View cache rate samples', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    throughputHidden.value = false
  })

  it('shows "-" for an empty bucket and a real 0% when prompt tokens were observed', async () => {
    const empty = await mountWith(
      metrics({ rate: 0, hasSamples: false }),
      metrics({ rate: 0, hasSamples: false }),
    )
    expect(empty.text()).toContain('-')
    expect(empty.text()).not.toContain('0.00%')
    empty.unmount()

    const trueZero = await mountWith(
      metrics({ rate: 0, hasSamples: true, denominator: 120 }),
      metrics({ rate: 0, hasSamples: true, denominator: 120 }),
    )
    // Overview KPI + model row both report the observed 0%.
    expect(trueZero.text().match(/0\.00%/g)?.length).toBeGreaterThanOrEqual(2)
    trueZero.unmount()
  })

  it('keeps a redacted public payload that hid its denominator but kept samples', async () => {
    const wrapper = await mountWith(
      metrics({ rate: 0.5, hasSamples: true, denominator: 0 }),
      metrics({ rate: 1, hasSamples: true, denominator: 0 }),
    )
    expect(wrapper.text()).toContain('50.0%')
    expect(wrapper.text()).toContain('100.0%')
    wrapper.unmount()
  })

  it('drops throughput while throughput is hidden and still reports observed cache rates', async () => {
    throughputHidden.value = true
    const wrapper = await mountWith(
      metrics({ rate: 0, hasSamples: true, denominator: 120, redacted: true }),
      metrics({ rate: 1, hasSamples: true, denominator: 0, redacted: true }),
    )
    // Real 0% in the overview KPI, full hit in the model list row.
    expect(wrapper.text()).toContain('0.00%')
    expect(wrapper.text()).toContain('100.0%')
    // Redaction zeroed RPM/TPM, so the throughput surfaces are not rendered at all.
    expect(wrapper.text()).not.toContain('channelMonitorV2.metrics.tps')
    expect(wrapper.text()).not.toContain('channelMonitorV2.metrics.rpm')
    expect(wrapper.text()).not.toContain('NaN')
    wrapper.unmount()

    // Empty samples stay "no data" even though throughput is gone.
    const empty = await mountWith(
      metrics({ rate: 0, hasSamples: false, redacted: true }),
      metrics({ rate: 0, hasSamples: false, redacted: true }),
    )
    expect(empty.text()).toContain('-')
    expect(empty.text()).not.toContain('0.00%')
    empty.unmount()
  })
})
