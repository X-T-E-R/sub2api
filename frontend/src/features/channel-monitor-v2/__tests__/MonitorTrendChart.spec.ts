import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import MonitorTrendChart from '../MonitorTrendChart.vue'
import type { MonitorHealth, MonitorMetric } from '@/api/channelMonitorV2'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
      locale: { value: 'zh-CN' },
    }),
  }
})

vi.mock('vue-chartjs', () => ({
  Line: {
    name: 'Line',
    props: ['data', 'options'],
    template: '<div class="line-chart">{{ JSON.stringify(data) }}</div>',
  },
}))

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
}): MonitorMetric {
  return {
    success_requests: cache.hasSamples === false ? 0 : 10,
    error_requests: 0,
    request_count: cache.hasSamples === false ? 0 : 10,
    token_count: 0,
    rpm: 0,
    tpm: 0,
    error_rate: 0,
    cache_rate: cache.rate,
    cache_rate_has_samples: cache.hasSamples,
    cache_rate_numerator: cache.hasSamples === false ? 0 : cache.rate * (cache.denominator ?? 100),
    cache_rate_denominator: cache.denominator ?? (cache.hasSamples === false ? 0 : 100),
    ttft: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
    duration: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
  }
}

function mountChart(
  points: Array<{ start: string; metrics: MonitorMetric }>,
) {
  return mount(MonitorTrendChart, {
    props: {
      trend: points.map((point) => ({
        bucket_start: point.start,
        metrics: point.metrics,
        health,
      })),
      coverage: {
        requested_start: points[0]?.start ?? '2026-10-22T04:00:00Z',
        requested_end: '2026-10-22T05:00:00Z',
        coverage_start: points[0]?.start ?? '2026-10-22T04:00:00Z',
        data_through: '2026-10-22T05:00:00Z',
        computed_at: '2026-10-22T05:00:00Z',
        aggregation_lag_seconds: 0,
        coverage_complete: true,
        bucket_seconds: 300,
      },
      loading: false,
    },
    global: { stubs: { EmptyState: true } },
  })
}

function cacheDataset(wrapper: ReturnType<typeof mountChart>) {
  const data = JSON.parse(wrapper.find('.line-chart').text())
  return data.datasets.find(
    (dataset: { label: string }) =>
      dataset.label === 'channelMonitorV2.chart.cacheDataset',
  )
}

describe('MonitorTrendChart cache rate samples', () => {
  it('breaks the cache line on empty buckets and keeps real 0%, 100% and mixed read/write rates', () => {
    const wrapper = mountChart([
      // Real 75% (read 300 of input 100 + read 300) with no output tokens.
      { start: '2026-10-22T04:00:00Z', metrics: metrics({ rate: 0.75, denominator: 400 }) },
      // No traffic at all: this is "no data", not a 0% hit rate.
      { start: '2026-10-22T04:05:00Z', metrics: metrics({ rate: 0, hasSamples: false }) },
      // Prompt tokens observed with no cache read: a real 0%.
      { start: '2026-10-22T04:10:00Z', metrics: metrics({ rate: 0, denominator: 120 }) },
      // Everything cached: a real 100%.
      { start: '2026-10-22T04:15:00Z', metrics: metrics({ rate: 1, denominator: 90 }) },
      { start: '2026-10-22T04:20:00Z', metrics: metrics({ rate: 0.5, denominator: 200 }) },
    ])

    expect(cacheDataset(wrapper).data).toEqual([75, null, 0, 100, 50])
  })

  it('never averages an empty bucket into its neighbours', () => {
    const wrapper = mountChart([
      { start: '2026-10-22T04:00:00Z', metrics: metrics({ rate: 0.6, denominator: 100 }) },
      { start: '2026-10-22T04:05:00Z', metrics: metrics({ rate: 0, hasSamples: false }) },
      { start: '2026-10-22T04:10:00Z', metrics: metrics({ rate: 0.6, denominator: 100 }) },
    ])

    // With a 0 placeholder both neighbours would be dragged to 40%.
    expect(cacheDataset(wrapper).data).toEqual([60, null, 60])
  })

  it('keeps a redacted public payload with samples but no visible denominator', () => {
    const wrapper = mountChart([
      { start: '2026-10-22T04:00:00Z', metrics: metrics({ rate: 0.5, hasSamples: true, denominator: 0 }) },
      { start: '2026-10-22T04:05:00Z', metrics: metrics({ rate: 0, hasSamples: true, denominator: 0 }) },
    ])

    expect(cacheDataset(wrapper).data).toEqual([50, 0])
  })

  it('shows "-" instead of 0% in the tooltip for an empty bucket', () => {
    const wrapper = mountChart([
      { start: '2026-10-22T04:00:00Z', metrics: metrics({ rate: 0, hasSamples: false }) },
    ])
    const options = wrapper
      .findComponent({ name: 'Line' })
      .props('options') as {
      plugins: { tooltip: { callbacks: { label: (ctx: unknown) => string } } }
    }

    expect(
      options.plugins.tooltip.callbacks.label({
        dataset: { label: 'channelMonitorV2.chart.cacheDataset' },
        parsed: { y: null },
      }),
    ).toBe('channelMonitorV2.chart.cacheDataset: -')
    expect(
      options.plugins.tooltip.callbacks.label({
        dataset: { label: 'channelMonitorV2.chart.cacheDataset' },
        parsed: { y: 0 },
      }),
    ).toBe('channelMonitorV2.chart.cacheDataset: 0.00%')
  })

  it('names the missing sample in the tooltip and still draws an isolated observation', () => {
    const wrapper = mountChart([
      { start: '2026-10-22T04:00:00Z', metrics: metrics({ rate: 0.62, denominator: 0, hasSamples: true }) },
      { start: '2026-10-22T04:05:00Z', metrics: metrics({ rate: 0, hasSamples: false }) },
      { start: '2026-10-22T04:10:00Z', metrics: metrics({ rate: 0, denominator: 120 }) },
      { start: '2026-10-22T04:15:00Z', metrics: metrics({ rate: 0.8, denominator: 100 }) },
    ])
    const dataset = wrapper
      .findComponent({ name: 'Line' })
      .props('data')
      .datasets.find(
        (item: { label: string }) => item.label === 'channelMonitorV2.chart.cacheDataset',
      ) as { pointRadius: (ctx: { dataIndex: number }) => number }

    // Isolated observation between two empty buckets stays visible; gaps and
    // points inside a continuous run draw no dot.
    expect(dataset.pointRadius({ dataIndex: 0 })).toBe(3)
    expect(dataset.pointRadius({ dataIndex: 1 })).toBe(0)
    expect(dataset.pointRadius({ dataIndex: 2 })).toBe(0)

    const callbacks = (
      wrapper.findComponent({ name: 'Line' }).props('options') as {
        plugins: { tooltip: { callbacks: { afterBody: (items: Array<{ dataIndex: number }>) => string } } }
      }
    ).plugins.tooltip.callbacks
    expect(callbacks.afterBody([{ dataIndex: 1 }])).toBe('channelMonitorV2.chart.cacheDataset: -')
    expect(callbacks.afterBody([{ dataIndex: 2 }])).toBe('')
  })
})
