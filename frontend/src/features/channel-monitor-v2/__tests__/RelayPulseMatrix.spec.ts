const i18nT = (key: string, params?: Record<string, unknown>) => {
  const map: Record<string, string> = {
    'channelMonitorV2.matrix.title': '色块矩阵',
    'channelMonitorV2.matrix.description': '趋势色块视图',
    'channelMonitorV2.bucket.minutes': '{count}分钟',
    'channelMonitorV2.matrix.wheelZoomX': '滚轮缩放横向',
    'channelMonitorV2.matrix.resetZoom': '重置缩放',
    'channelMonitorV2.matrix.dimension': '维度',
    'channelMonitorV2.metrics.successRate': '成功率',
    'channelMonitorV2.metrics.ttft': '首 Token',
    'channelMonitorV2.metrics.tps': '每秒 Token',
    'channelMonitorV2.metrics.cacheRate': '缓存率',
    'channelMonitorV2.matrix.scoreLine': '评分 {score}',
    'channelMonitorV2.metrics.successRateValue': '成功率 {value}',
    'channelMonitorV2.metrics.errorRateValue': '错误率 {value}',
    'channelMonitorV2.metrics.rpmValue': 'RPM {value}',
    'channelMonitorV2.metrics.tpmValue': 'TPM {value}',
    'channelMonitorV2.metrics.tpsValue': '每秒 Token {value}',
    'channelMonitorV2.metrics.ttftValue': '首 Token {value}',
    'channelMonitorV2.metrics.durationValue': '时长 {value}',
    'channelMonitorV2.metrics.cacheRateValue': '缓存率 {value}',
    'channelMonitorV2.matrix.noTrafficAt': '{time} 无流量',
    'channelMonitorV2.matrix.noTraffic': '无流量',
    'channelMonitorV2.matrix.legendAria': '图例',
    'channelMonitorV2.matrix.bad': '差',
    'channelMonitorV2.matrix.good': '好',
    'channelMonitorV2.matrix.healthyLegend': '健康',
    'channelMonitorV2.matrix.warningLegend': '警告',
    'channelMonitorV2.matrix.criticalLegend': '异常',
    'channelMonitorV2.matrix.unknownLegend': '未知',
  }
  const template = map[key] || key
  return template.replace(/\{(\w+)\}/g, (_, name) => String(params?.[name] ?? ''))
}

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: i18nT,
      te: (key: string) => key.startsWith('channelMonitorV2.'),
      locale: { value: 'zh' },
    }),
  }
})

import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { describe, expect, it } from 'vitest'
import RelayPulseMatrix from '../RelayPulseMatrix.vue'
import type { MonitorHealth, MonitorMetric } from '@/api/channelMonitorV2'

const health: MonitorHealth = {
  overall: 'warning',
  error_rate: 'warning',
  ttft: 'healthy',
  cache: 'warning',
  score: 52,
  error_rate_score: 40,
  ttft_score: 100,
  cache_score: 50,
  minimum_sample: 20,
}

function metrics(requestCount: number): MonitorMetric {
  return {
    success_requests: requestCount ? requestCount - 1 : 0,
    error_requests: requestCount ? 1 : 0,
    request_count: requestCount,
    token_count: 100,
    rpm: 1.5,
    tpm: 10.2,
    error_rate: requestCount ? 1 / requestCount : 0,
    cache_rate: 0.5,
    cache_rate_numerator: 50,
    cache_rate_denominator: 100,
    ttft: { sample_count: requestCount, p50_ms: 100, p95_ms: 300, avg_ms: 150 },
    duration: { sample_count: requestCount, p50_ms: 500, p95_ms: 900, avg_ms: 600 },
    upstream_affected_requests: 2,
    upstream_attempt_count: 3,
  }
}

describe('RelayPulseMatrix', () => {
  it('shows privacy-safe hover tooltips and multi-band colors without click modal', async () => {
    const wrapper = mount(RelayPulseMatrix, {
      props: {
        rows: [{
          platform: 'openai',
          group_id: 7,
          group_name: '默认组',
          model: 'gpt-5',
          metrics: metrics(10),
          health,
          buckets: [
            { bucket_start: '2026-08-01T00:00:00Z', metrics: metrics(10), health },
            { bucket_start: '2026-08-01T00:01:00Z', metrics: metrics(0), health },
          ],
        }],
        coverage: {
          requested_start: '2026-08-01T00:00:00Z',
          requested_end: '2026-08-01T00:03:00Z',
          coverage_start: '2026-08-01T00:00:00Z',
          data_through: '2026-08-01T00:03:00Z',
          computed_at: '2026-08-01T00:03:00Z',
          aggregation_lag_seconds: 0,
          coverage_complete: true,
          bucket_seconds: 60,
        },
        healthMode: 'overall',
      },
    })

    const cells = wrapper.findAll('.pulse-cell')
    expect(cells).toHaveLength(3)
    // Hover tooltip content (privacy-safe: no absolute request/error counts)
    const tip = cells[0].text()
    expect(tip).toContain('成功率')
    expect(tip).toContain('90.0%')
    expect(tip).toContain('首 Token')
    expect(tip).toContain('每秒 Token')
    expect(tip).toContain('缓存率')
    expect(tip).toContain('RPM')
    expect(tip).not.toContain('请求数')
    expect(tip).not.toContain('用户错误')
    expect(tip).not.toContain('上游受影响')
    // Summary columns: success · ttft · tokens/s · cache
    const header = wrapper.find('.matrix-header').text()
    expect(header).toContain('成功率')
    expect(header).toContain('首 Token')
    expect(header).toContain('每秒 Token')
    expect(header).toContain('缓存率')
    // Multi-band class from score 52 → score5
    expect(cells[0].classes().some((c) => c.startsWith('health-score'))).toBe(true)
    // Redacted user payloads may have request_count=0 but still include score.
    expect(cells[1].classes().some((c) => c.startsWith('health-score'))).toBe(true)
    expect(cells[2].classes()).toContain('health-unknown')

    // No click-to-open modal
    await cells[0].trigger('click')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })
})

describe('RelayPulseMatrix cache rate samples', () => {
  it('shows "-" for buckets without samples and keeps real 0% / redacted samples distinct', () => {
    const empty = { ...metrics(0), cache_rate: 0, cache_rate_has_samples: false, cache_rate_denominator: 0 }
    const trueZero = { ...metrics(10), cache_rate: 0, cache_rate_has_samples: true, cache_rate_denominator: 120 }
    // Public payload: samples survived redaction, denominator and throughput did not.
    const redacted = {
      ...metrics(0),
      cache_rate: 0.5,
      cache_rate_has_samples: true,
      cache_rate_denominator: 0,
      cache_rate_numerator: 0,
      rpm: 0,
      tpm: 0,
    }
    const wrapper = mount(RelayPulseMatrix, {
      props: {
        rows: [{
          platform: 'anthropic',
          group_id: 7,
          group_name: '默认组',
          model: 'claude-sonnet-5',
          metrics: redacted,
          health,
          buckets: [
            { bucket_start: '2026-08-01T00:00:00Z', metrics: empty, health },
            { bucket_start: '2026-08-01T00:01:00Z', metrics: trueZero, health },
            { bucket_start: '2026-08-01T00:02:00Z', metrics: redacted, health },
          ],
        }],
        coverage: {
          requested_start: '2026-08-01T00:00:00Z',
          requested_end: '2026-08-01T00:03:00Z',
          coverage_start: '2026-08-01T00:00:00Z',
          data_through: '2026-08-01T00:03:00Z',
          computed_at: '2026-08-01T00:03:00Z',
          aggregation_lag_seconds: 0,
          coverage_complete: true,
          bucket_seconds: 60,
        },
        healthMode: 'overall',
      },
    })

    const cells = wrapper.findAll('.pulse-cell')
    expect(cells[0].text()).toContain('缓存率 -')
    expect(cells[1].text()).toContain('缓存率 0.00%')
    expect(cells[2].text()).toContain('缓存率 50.0%')
    // Row summary follows the same rule instead of defaulting to 0%.
    expect(wrapper.findAll('.matrix-row')[1].text()).toContain('50.0%')
  })
})

describe('RelayPulseMatrix tooltip placement', () => {
  it('keeps the floating tooltip inside a narrow viewport', async () => {
    const originalWidth = window.innerWidth
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 390 })
    Object.defineProperty(HTMLElement.prototype, 'offsetWidth', {
      configurable: true,
      get(this: HTMLElement) {
        return this.classList?.contains('matrix-floating-tooltip') ? 220 : 0
      },
    })
    Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
      configurable: true,
      get(this: HTMLElement) {
        return this.classList?.contains('matrix-floating-tooltip') ? 140 : 0
      },
    })
    try {
      const wrapper = mount(RelayPulseMatrix, {
        props: {
          rows: [{
            platform: 'openai',
            group_id: 7,
            group_name: '默认组',
            model: 'gpt-5',
            metrics: metrics(10),
            health,
            buckets: [
              { bucket_start: '2026-08-01T00:00:00Z', metrics: metrics(10), health },
              { bucket_start: '2026-08-01T00:01:00Z', metrics: metrics(0), health },
            ],
          }],
          coverage: {
            requested_start: '2026-08-01T00:00:00Z',
            requested_end: '2026-08-01T00:02:00Z',
            coverage_start: '2026-08-01T00:00:00Z',
            data_through: '2026-08-01T00:02:00Z',
            computed_at: '2026-08-01T00:02:00Z',
            aggregation_lag_seconds: 0,
            coverage_complete: true,
            bucket_seconds: 60,
          },
          healthMode: 'overall',
        },
      })
      // jsdom reports a zero-sized rect at the left edge, the worst case for a
      // centred tooltip: without clamping it would render off-screen.
      await wrapper.findAll('.pulse-cell')[0].trigger('focus')
      await nextTick()
      await nextTick()
      const tooltip = document.body.querySelector('.matrix-floating-tooltip') as HTMLElement
      expect(tooltip).toBeTruthy()
      expect(parseFloat(tooltip.style.left)).toBe(118)
      expect(parseFloat(tooltip.style.top)).toBe(148)
      wrapper.unmount()
    } finally {
      delete (HTMLElement.prototype as Partial<HTMLElement>).offsetWidth
      delete (HTMLElement.prototype as Partial<HTMLElement>).offsetHeight
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: originalWidth })
    }
  })
})

describe('RelayPulseMatrix axis range', () => {
  it('uses selected requested range for the X axis even with partial coverage', () => {
    const wrapper = mount(RelayPulseMatrix, {
      props: {
        rows: [{
          platform: 'openai',
          group_id: 7,
          group_name: '默认组',
          model: 'gpt-5',
          metrics: metrics(10),
          health,
          buckets: [
            { bucket_start: '2026-08-01T00:02:00Z', metrics: metrics(10), health },
          ],
        }],
        coverage: {
          requested_start: '2026-08-01T00:00:00Z',
          requested_end: '2026-08-01T00:05:00Z',
          coverage_start: '2026-08-01T00:02:00Z',
          data_through: '2026-08-01T00:03:00Z',
          computed_at: '2026-08-01T00:03:00Z',
          aggregation_lag_seconds: 0,
          coverage_complete: false,
          bucket_seconds: 60,
        },
        healthMode: 'overall',
      },
    })
    // 5 minutes @ 60s buckets → 5 cells spanning the selected range
    expect(wrapper.findAll('.pulse-cell')).toHaveLength(5)
  })
})
