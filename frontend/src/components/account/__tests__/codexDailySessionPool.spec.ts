import { describe, expect, it } from 'vitest'
import { isCodexDailySessionPoolValid, readCodexDailySessionPool, writeCodexDailySessionPool } from '../codexDailySessionPool'

describe('Codex daily session pool form contract', () => {
  it('leaves unconfigured accounts and untouched imports unchanged', () => {
    const form = readCodexDailySessionPool()
    expect(form).toEqual({ enabled: false, min: 5, max: 10 })
    const extra = { unrelated: true }
    writeCodexDailySessionPool(extra, form, 'session', false)
    expect(extra).toEqual({ unrelated: true })
  })

  it('serializes explicit settings without changing unrelated extra', () => {
    const extra = { unrelated: true }
    writeCodexDailySessionPool(extra, { enabled: true, min: 5, max: 5 }, 'session', true)
    expect(extra).toEqual({
      unrelated: true,
      codex_daily_session_pool_enabled: true,
      codex_daily_session_pool_min: 5,
      codex_daily_session_pool_max: 5
    })
  })

  it('hydrates saved bounds and writes explicit false when disabled', () => {
    const extra = {
      codex_daily_session_pool_enabled: true,
      codex_daily_session_pool_min: 7,
      codex_daily_session_pool_max: 9
    }
    const form = readCodexDailySessionPool(extra)
    expect(form).toEqual({ enabled: true, min: 7, max: 9 })
    writeCodexDailySessionPool(extra, { ...form, enabled: false }, 'session', true)
    expect(extra).toEqual({
      codex_daily_session_pool_enabled: false,
      codex_daily_session_pool_min: 7,
      codex_daily_session_pool_max: 9
    })
  })

  it.each(['off', 'device', 'full'])('disables new allocation when switching to %s', mode => {
    const extra = { codex_daily_session_pool_enabled: true }
    writeCodexDailySessionPool(extra, { enabled: true, min: 5, max: 10 }, mode, false)
    expect(extra.codex_daily_session_pool_enabled).toBe(false)
  })

  it.each([
    [0, 5], [5, 1001], [10, 5], [5.5, 10], [5, 8.5], ['', 10], [5, ''], ['5', 10]
  ])('rejects invalid bounds %s / %s', (min, max) => {
    expect(isCodexDailySessionPoolValid({ enabled: true, min, max })).toBe(false)
  })

  it.each([[1, 1], [5, 10], [1000, 1000]])('accepts inclusive bounds %s / %s', (min, max) => {
    expect(isCodexDailySessionPoolValid({ enabled: true, min, max })).toBe(true)
  })
})
