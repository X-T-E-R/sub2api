export interface CodexDailySessionPoolForm {
  enabled: boolean
  min: number | string
  max: number | string
}

export function readCodexDailySessionPool(extra?: Record<string, unknown>): CodexDailySessionPoolForm {
  const value = (key: string, fallback: number) => {
    const current = extra?.[key]
    return typeof current === 'number' || typeof current === 'string' ? current : fallback
  }
  return {
    enabled: extra?.codex_daily_session_pool_enabled === true,
    min: value('codex_daily_session_pool_min', 5),
    max: value('codex_daily_session_pool_max', 10)
  }
}

export function isCodexDailySessionPoolValid(value: CodexDailySessionPoolForm): boolean {
  if (!value.enabled) return true
  return typeof value.min === 'number' && typeof value.max === 'number' &&
    Number.isInteger(value.min) && Number.isInteger(value.max) &&
    value.min >= 1 && value.max <= 1000 && value.min <= value.max
}

export function writeCodexDailySessionPool(
  extra: Record<string, unknown>,
  value: CodexDailySessionPoolForm,
  mode: string,
  touched: boolean
): void {
  if (!touched && !Object.prototype.hasOwnProperty.call(extra, 'codex_daily_session_pool_enabled')) return
  const enabled = mode === 'session' && value.enabled
  extra.codex_daily_session_pool_enabled = enabled
  if (enabled) {
    extra.codex_daily_session_pool_min = value.min
    extra.codex_daily_session_pool_max = value.max
  }
}
