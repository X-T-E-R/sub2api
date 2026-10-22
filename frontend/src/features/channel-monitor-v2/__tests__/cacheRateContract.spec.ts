/**
 * Vertical contract check for `cache_rate_has_samples`.
 *
 * Payloads below mirror what the Go serializer emits for ChannelMonitorV2Metric
 * (json tags in backend/internal/service/channel_monitor_v2.go) after
 * redactChannelMonitorV2Metric: the flag is always present, the rate survives
 * redaction, absolute tokens/counts and the denominator are zeroed, and RPM/TPM
 * are zeroed when throughput is hidden. They are synthetic JSON round-trips of
 * that contract, not captured production responses.
 */
import { describe, expect, it } from 'vitest'

import { formatMonitorCacheRate, monitorCacheRate } from '../monitorFormat'

type Json = Record<string, any>

/** One serialized metric, with every field the backend writes. */
function serializedMetric(overrides: Json = {}): Json {
  const metric: Json = {
    success_requests: 12,
    error_requests: 0,
    request_count: 12,
    input_tokens: 0,
    output_tokens: 0,
    cache_creation_tokens: 0,
    cache_read_tokens: 0,
    token_count: 0,
    rpm: 0,
    tpm: 0,
    error_rate: 0,
    success_rate: 1,
    cache_rate: 0,
    cache_rate_has_samples: false,
    cache_rate_numerator: 0,
    cache_rate_denominator: 0,
    ttft: { sample_count: 12, p50_ms: 820, p90_ms: null, p95_ms: null, avg_ms: 900 },
    duration: { sample_count: 0, p50_ms: null, p90_ms: null, p95_ms: null, avg_ms: null },
    ...overrides,
  }
  // Round-trip so the fixtures are plain JSON, exactly like an HTTP response body.
  return JSON.parse(JSON.stringify(metric))
}

const coverage: Json = {
  requested_start: '2026-10-22T04:00:00Z',
  requested_end: '2026-10-22T05:00:00Z',
  coverage_start: '2026-10-22T04:00:00Z',
  data_through: '2026-10-22T04:35:00Z',
  computed_at: '2026-10-22T04:35:00Z',
  aggregation_lag_seconds: 30,
  coverage_complete: true,
  bucket_seconds: 300,
}

const health: Json = {
  overall: 'healthy',
  error_rate: 'healthy',
  ttft: 'healthy',
  cache: 'warning',
  score: 82,
  error_rate_score: 100,
  ttft_score: 80,
  cache_score: 60,
  minimum_sample: 20,
}

const empty = serializedMetric({ cache_rate: 0, cache_rate_has_samples: false })
/** Public redaction kept the rate and the flag but hid the denominator. */
const redactedTrueZero = serializedMetric({ cache_rate: 0, cache_rate_has_samples: true })
const redactedFull = serializedMetric({ cache_rate: 1, cache_rate_has_samples: true })
const redactedPartial = serializedMetric({ cache_rate: 0.8, cache_rate_has_samples: true })
/** Admin payload: denominator intact, real 0% hit rate. */
const adminTrueZero = serializedMetric({
  cache_rate: 0,
  cache_rate_has_samples: true,
  cache_rate_denominator: 120,
  input_tokens: 120,
  cache_read_tokens: 0,
})
/** Explicit false wins over a stale positive denominator. */
const explicitFalse = serializedMetric({ cache_rate: 0.5, cache_rate_has_samples: false, cache_rate_denominator: 100 })
/** Payload from a backend that predates the flag: denominator is the only signal. */
const legacyWithSamples = serializedMetric({ cache_rate: 0.75, cache_rate_denominator: 100 })
delete legacyWithSamples.cache_rate_has_samples
const legacyEmpty = serializedMetric({ cache_rate: 0, cache_rate_denominator: 0 })
delete legacyEmpty.cache_rate_has_samples

const snapshot: Json = {
  config: {
    version: 1,
    enabled: true,
    refresh_interval_seconds: 300,
    platforms: [{ platform: 'anthropic', enabled: true, models: ['claude-sonnet-5'] }],
    group_ids: [7],
    health_thresholds: { minimum_sample: 20 },
  },
  coverage,
  metrics: redactedTrueZero,
  health,
  trend: [
    { bucket_start: '2026-10-22T04:00:00Z', metrics: redactedPartial, health },
    { bucket_start: '2026-10-22T04:05:00Z', metrics: empty, health },
    { bucket_start: '2026-10-22T04:10:00Z', metrics: redactedTrueZero, health },
    { bucket_start: '2026-10-22T04:15:00Z', metrics: redactedFull, health },
  ],
}

const matrix: Json = {
  coverage,
  group_by: 'platform_model',
  items: [
    {
      platform: 'anthropic',
      group_id: 7,
      group_name: 'default',
      model: 'claude-sonnet-5',
      metrics: redactedPartial,
      health,
      buckets: [
        { bucket_start: '2026-10-22T04:00:00Z', metrics: redactedPartial, health },
        { bucket_start: '2026-10-22T04:05:00Z', metrics: empty, health },
        { bucket_start: '2026-10-22T04:10:00Z', metrics: redactedTrueZero, health },
      ],
    },
  ],
}

/** List surfaces (models / users tabs) put the same metric object on each row. */
const list = {
  coverage,
  items: [
    { platform: 'anthropic', model: 'claude-sonnet-5', metrics: redactedTrueZero, health },
    { platform: 'openai', model: 'gpt-5.1', metrics: empty, health },
  ],
}

describe('cache_rate_has_samples contract across monitor surfaces', () => {
  it('serializes the flag on the shared metric object', () => {
    for (const metric of [empty, redactedTrueZero, adminTrueZero, explicitFalse]) {
      expect(Object.prototype.hasOwnProperty.call(metric, 'cache_rate_has_samples')).toBe(true)
    }
  })

  it('resolves the same cache text for snapshot, trend, matrix, buckets and list rows', () => {
    const positions: Array<[string, Json, string]> = [
      ['snapshot.metrics', snapshot.metrics, '0.00%'],
      ['trend[0].metrics', snapshot.trend[0].metrics, '80.0%'],
      ['trend[1].metrics (empty)', snapshot.trend[1].metrics, '-'],
      ['trend[2].metrics (true zero)', snapshot.trend[2].metrics, '0.00%'],
      ['trend[3].metrics (full)', snapshot.trend[3].metrics, '100.0%'],
      ['matrix.items[0].metrics', matrix.items[0].metrics, '80.0%'],
      ['matrix.items[0].buckets[1].metrics', matrix.items[0].buckets[1].metrics, '-'],
      ['matrix.items[0].buckets[2].metrics', matrix.items[0].buckets[2].metrics, '0.00%'],
      ['models list row', list.items[0].metrics, '0.00%'],
      ['users list row (empty)', list.items[1].metrics, '-'],
    ]
    for (const [label, metric, expected] of positions) {
      expect(`${label}: ${formatMonitorCacheRate(metric)}`).toBe(`${label}: ${expected}`)
    }
  })

  it('lets an explicit flag win over the denominator fallback', () => {
    // Redaction hides the denominator: the flag alone keeps the real 0%.
    expect(redactedTrueZero.cache_rate_denominator).toBe(0)
    expect(monitorCacheRate(redactedTrueZero)).toBe(0)
    // Explicitly "no samples" must not become 50% just because a denominator survived.
    expect(explicitFalse.cache_rate_denominator).toBe(100)
    expect(monitorCacheRate(explicitFalse)).toBeNull()
    expect(formatMonitorCacheRate(explicitFalse)).toBe('-')
    // Only a payload without the field falls back to the denominator.
    expect(formatMonitorCacheRate(legacyWithSamples)).toBe('75.0%')
    expect(formatMonitorCacheRate(legacyEmpty)).toBe('-')
  })

  it('keeps the rate readable when throughput redaction zeroed RPM/TPM', () => {
    const hiddenThroughput = serializedMetric({
      cache_rate: 0,
      cache_rate_has_samples: true,
      rpm: 0,
      tpm: 0,
      request_count: 0,
      input_tokens: 0,
      cache_read_tokens: 0,
    })
    expect(formatMonitorCacheRate(hiddenThroughput)).toBe('0.00%')
    expect(Number.isNaN(Number(hiddenThroughput.cache_rate))).toBe(false)
  })
})
