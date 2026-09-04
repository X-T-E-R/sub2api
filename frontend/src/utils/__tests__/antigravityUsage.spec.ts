import { describe, expect, it } from 'vitest'

import {
  buildAntigravityQuotaRows,
  getAntigravityTier,
  hasAntigravityIneligibleTier
} from '../antigravityUsage'

describe('antigravity usage projection', () => {
  it('projects current model versions and preserves explicit zero utilization', () => {
    const rows = buildAntigravityQuotaRows({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null,
      antigravity_quota: {
        'gemini-3.8-flash': { utilization: 0, reset_time: null },
        'gemini-3.1-pro-high': { utilization: 70, reset_time: '2026-09-06T02:00:00Z' },
        'future-model': { utilization: 33 }
      },
      antigravity_quota_details: {
        'gemini-3.8-flash': { display_name: 'Gemini 3.8 Flash' }
      }
    })

    expect(rows).toEqual(expect.arrayContaining([
      expect.objectContaining({ key: 'gemini-3.8-flash', compactLabel: 'G3.8F', utilization: 0 }),
      expect.objectContaining({ key: 'gemini-3.1-pro-high', compactLabel: 'G3.1P', utilization: 70 }),
      expect.objectContaining({ key: 'future-model', compactLabel: 'FM', utilization: 33 })
    ]))
    expect(rows.find((row) => row.key === 'gemini-3.8-flash')?.title)
      .toBe('Gemini 3.8 Flash (gemini-3.8-flash)')
  })

  it('does not combine distinct model values into a synthetic family observation', () => {
    const rows = buildAntigravityQuotaRows({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null,
      antigravity_quota: {
        'gemini-3.1-flash-image': { utilization: 20, reset_time: '2026-09-06T01:00:00Z' },
        'gemini-3-pro-image': { utilization: 70, reset_time: '2026-09-06T02:00:00Z' }
      }
    })

    expect(rows).toHaveLength(2)
    expect(rows.map(({ utilization, resetTime }) => ({ utilization, resetTime }))).toEqual(
      expect.arrayContaining([
        { utilization: 20, resetTime: '2026-09-06T01:00:00Z' },
        { utilization: 70, resetTime: '2026-09-06T02:00:00Z' }
      ])
    )
  })

  it('drops a deprecated alias only when its replacement has a valid quota', () => {
    const rows = buildAntigravityQuotaRows({
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null,
      antigravity_quota: {
        'old-model': { utilization: 10 },
        'future-model': { utilization: 20 }
      },
      model_forwarding_rules: { 'old-model': 'future-model' }
    })

    expect(rows.map((row) => row.key)).toEqual(['future-model'])
  })

  it('prefers the live tier and ineligible flag over legacy account extra', () => {
    const extra = {
      load_code_assist: {
        paidTier: { id: 'g1-ultra-tier' },
        ineligibleTiers: [{}]
      }
    }
    const usage = {
      source: 'active' as const,
      updated_at: null,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null,
      subscription_tier: 'FREE',
      antigravity_ineligible: false
    }

    expect(getAntigravityTier(usage, extra)).toBe('free-tier')
    expect(hasAntigravityIneligibleTier(usage, extra)).toBe(false)
    expect(getAntigravityTier(null, extra)).toBe('g1-ultra-tier')
    expect(hasAntigravityIneligibleTier(null, extra)).toBe(true)
  })

  it('keeps legacy tier and ineligible fallback for an empty passive cache miss', () => {
    const usage = {
      source: 'passive' as const,
      antigravity_quota_state: 'unavailable' as const,
      antigravity_subscription_state: 'unavailable' as const,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null
    }
    const extra = {
      load_code_assist: {
        paidTier: { id: 'g1-pro-tier' },
        ineligibleTiers: [{ reasonCode: 'INELIGIBLE_ACCOUNT' }]
      }
    }

    expect(getAntigravityTier(usage, extra)).toBe('g1-pro-tier')
    expect(hasAntigravityIneligibleTier(usage, extra)).toBe(true)
  })

  it('lets a timestamped passive snapshot supersede legacy extra', () => {
    const usage = {
      source: 'passive' as const,
      updated_at: '2026-09-05T18:00:00Z',
      subscription_tier: 'FREE',
      antigravity_ineligible: false,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null
    }
    const extra = {
      load_code_assist: {
        paidTier: { id: 'g1-ultra-tier' },
        ineligibleTiers: [{}]
      }
    }

    expect(getAntigravityTier(usage, extra)).toBe('free-tier')
    expect(hasAntigravityIneligibleTier(usage, extra)).toBe(false)
  })

  it('lets an explicit active unavailable response supersede legacy extra without a timestamp', () => {
    const usage = {
      source: 'active' as const,
      antigravity_quota_state: 'unavailable' as const,
      antigravity_subscription_state: 'unavailable' as const,
      five_hour: null,
      seven_day: null,
      seven_day_sonnet: null
    }
    const extra = {
      load_code_assist: {
        paidTier: { id: 'g1-ultra-tier' },
        ineligibleTiers: [{}]
      }
    }

    expect(getAntigravityTier(usage, extra)).toBeNull()
    expect(hasAntigravityIneligibleTier(usage, extra)).toBe(false)
  })
})
