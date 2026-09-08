import type {
  AccountUsageInfo,
  AntigravityModelDetail,
  AntigravityModelQuota
} from '@/types'
import type { AntigravityWindowKey } from '@/types'

export const antigravityWindowPercent = (usage: AccountUsageInfo, key: AntigravityWindowKey): number | null => {
  const value = usage.antigravity_windows?.[key]?.remaining_fraction
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1
    ? value * 100
    : null
}

// Format only at the text boundary; sorting and bars retain the observed precision.
export const antigravityPercentLabel = (percent: number): string => `${Math.floor(percent)}%`

export const antigravityCreditAmount = (credit: NonNullable<AccountUsageInfo['ai_credits']>[number]): string | null => {
  if (typeof credit.amount_text === 'string') {
    const value = credit.amount_text.trim()
    return value !== '' && Number.isFinite(Number(value)) ? value : null
  }
  return typeof credit.amount === 'number' && Number.isFinite(credit.amount) ? String(credit.amount) : null
}

export type AntigravityQuotaFamily = 'gemini-pro' | 'gemini-flash' | 'gemini-image' | 'claude' | 'other'

export interface AntigravityQuotaRow {
  key: string
  family: AntigravityQuotaFamily
  modelIDs: string[]
  compactLabel: string
  title: string
  utilization: number
  remainingPercent: number
  resetTime: string | null
}

const familyOrder: Record<AntigravityQuotaFamily, number> = {
  'gemini-pro': 0,
  'gemini-flash': 1,
  'gemini-image': 2,
  claude: 3,
  other: 4
}

const modelRemainingPercent = (value: AntigravityModelQuota | undefined): number | null => {
  if (!value) return null
  if (value.remaining_fraction !== undefined && value.remaining_fraction !== null) {
    const fraction = value.remaining_fraction
    return typeof fraction === 'number' && Number.isFinite(fraction) && fraction >= 0 && fraction <= 1
      ? fraction * 100 : null
  }
  return typeof value.utilization === 'number' && Number.isFinite(value.utilization) &&
    value.utilization >= 0 && value.utilization <= 100 ? 100 - value.utilization : null
}

const validQuota = (value: AntigravityModelQuota | undefined): value is AntigravityModelQuota =>
  modelRemainingPercent(value) !== null

const quotaFamily = (modelID: string): AntigravityQuotaFamily => {
  const id = modelID.toLowerCase()
  if (id.startsWith('claude-')) return 'claude'
  if (!id.startsWith('gemini-')) return 'other'
  if (id.includes('image')) return 'gemini-image'
  if (/(^|[-.])pro(?:-|$)/.test(id)) return 'gemini-pro'
  if (/(^|[-.])flash(?:-|$)/.test(id)) return 'gemini-flash'
  return 'other'
}

const modelVersion = (modelID: string): string => {
  const match = modelID.match(/\d+(?:[.-]\d+)*/)
  return match ? match[0].replace(/-/g, '.') : ''
}

export const compactAntigravityModelLabel = (modelID: string): string => {
  const id = modelID.toLowerCase()
  const version = modelVersion(id)
  if (id.startsWith('gemini-')) {
    if (id.includes('image')) return `G${version}I`
    if (/(^|[-.])pro(?:-|$)/.test(id)) return `G${version}P`
    if (/(^|[-.])flash(?:-|$)/.test(id)) return `G${version}F`
    return `G${version || '?'}`
  }
  if (id.startsWith('claude-')) {
    const family = id.includes('opus') ? 'O' : id.includes('sonnet') ? 'S' : id.includes('haiku') ? 'H' : id.includes('fable') ? 'F' : 'C'
    return `${family}${version || '?'}`
  }
  const initials = id
    .split(/[^a-z0-9]+/)
    .filter(Boolean)
    .map((part) => part[0])
    .join('')
    .toUpperCase()
  return initials.slice(0, 5) || '?'
}

const quotaResetTime = (quota: AntigravityModelQuota): string | null => {
  const value = typeof quota.reset_time === 'string' ? quota.reset_time.trim() : ''
  return value && Number.isFinite(Date.parse(value)) ? value : null
}

const quotaTitle = (
  modelID: string,
  detail?: AntigravityModelDetail
): string => {
  const displayName = detail?.display_name?.trim()
  return displayName ? `${displayName} (${modelID})` : modelID
}

export const buildAntigravityQuotaRows = (usage?: AccountUsageInfo | null): AntigravityQuotaRow[] => {
  const quotas = usage?.antigravity_quota
  if (!quotas) return []
  const forwardingRules = usage?.model_forwarding_rules ?? {}
  const details = usage?.antigravity_quota_details ?? {}
  const entries = Object.entries(quotas)
    .filter((entry): entry is [string, AntigravityModelQuota] => validQuota(entry[1]))
    .filter(([modelID]) => {
      const target = forwardingRules[modelID]
      return !target || !validQuota(quotas[target])
    })
    .map(([modelID, quota]) => ({
      modelID,
      quota,
      detail: details[modelID],
      family: quotaFamily(modelID)
    }))

  const grouped = new Map<string, typeof entries>()
  const rows: AntigravityQuotaRow[] = []
  for (const entry of entries) {
    if (entry.family === 'other') {
      rows.push({
        key: entry.modelID,
        family: 'other',
        modelIDs: [entry.modelID],
        compactLabel: compactAntigravityModelLabel(entry.modelID),
        title: quotaTitle(entry.modelID, entry.detail),
        utilization: entry.quota.utilization,
        remainingPercent: modelRemainingPercent(entry.quota)!,
        resetTime: quotaResetTime(entry.quota)
      })
      continue
    }
    const resetTime = quotaResetTime(entry.quota) ?? ''
    const groupKey = `${entry.family}\u0000${modelRemainingPercent(entry.quota)}\u0000${resetTime}`
    const current = grouped.get(groupKey) ?? []
    current.push(entry)
    grouped.set(groupKey, current)
  }

  for (const familyEntries of grouped.values()) {
    familyEntries.sort((a, b) => a.modelID.localeCompare(b.modelID))
    const family = familyEntries[0].family
    const modelIDs = familyEntries.map(({ modelID }) => modelID)
    rows.push({
      key: modelIDs.length === 1 ? modelIDs[0] : `${family}:${modelIDs.join(',')}`,
      family,
      modelIDs,
      compactLabel: familyEntries.length === 1
        ? compactAntigravityModelLabel(familyEntries[0].modelID)
        : '',
      title: familyEntries
        .map(({ modelID, detail }) => quotaTitle(modelID, detail))
        .join('; '),
      utilization: familyEntries[0].quota.utilization,
      remainingPercent: modelRemainingPercent(familyEntries[0].quota)!,
      resetTime: quotaResetTime(familyEntries[0].quota)
    })
  }

  return rows.sort((a, b) => {
    const familyDelta = familyOrder[a.family] - familyOrder[b.family]
    return familyDelta || a.key.localeCompare(b.key)
  })
}

const legacyAntigravityTier = (extra?: Record<string, unknown>): string | null => {
  const loadCodeAssist = record(extra?.load_code_assist)
  for (const tier of [loadCodeAssist?.paidTier, loadCodeAssist?.currentTier]) {
    const id = text(typeof tier === 'string' ? tier : record(tier)?.id)
    if (id) return id
  }
  return null
}

const ownsAntigravityObservation = (
  usage: AccountUsageInfo | null | undefined
): usage is AccountUsageInfo => {
  if (!usage) return false
  if (usage.source === 'active') return true
  return typeof usage.updated_at === 'string' && usage.updated_at.trim() !== ''
}

export const getAntigravityTier = (
  usage: AccountUsageInfo | null | undefined,
  extra?: Record<string, unknown>
): string | null => {
  if (ownsAntigravityObservation(usage)) {
    const raw = usage.subscription_tier_raw?.trim()
    if (raw) return raw
    switch (usage.subscription_tier?.trim().toUpperCase()) {
      case 'FREE': return 'free-tier'
      case 'PRO': return 'g1-pro-tier'
      case 'ULTRA': return 'g1-ultra-tier'
      default: return null
    }
  }
  return legacyAntigravityTier(extra)
}

type AntigravityIneligibleTier = NonNullable<AccountUsageInfo['antigravity_ineligible_tiers']>[number]

const record = (value: unknown): Record<string, unknown> | undefined =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined

const text = (value: unknown): string | undefined =>
  typeof value === 'string' ? value.trim() || undefined : undefined

export const getAntigravityIneligibleTiers = (
  usage: AccountUsageInfo | null | undefined,
  extra?: Record<string, unknown>
): AntigravityIneligibleTier[] => {
  const current = ownsAntigravityObservation(usage)
  const entries = current
    ? usage.antigravity_ineligible_tiers
    : record(extra?.load_code_assist)?.ineligibleTiers
  if (!Array.isArray(entries)) return []
  return entries.flatMap((value: unknown) => {
    const entry = record(value)
    if (!entry) return []
    const tier = entry.tier
    const normalized = {
      tier_id: text(current ? entry.tier_id : typeof tier === 'string' ? tier : record(tier)?.id),
      reason_code: text(current ? entry.reason_code : entry.reasonCode),
      reason_message: text(current ? entry.reason_message : entry.reasonMessage)
    }
    return Object.values(normalized).some(Boolean) ? [normalized] : []
  })
}

export const hasAntigravityIneligibleTier = (
  usage: AccountUsageInfo | null | undefined,
  extra?: Record<string, unknown>
): boolean => {
  // Old cache snapshots have only the presence flag. Keep it informational and
  // never merge a current observation with unrelated legacy account metadata.
  if (ownsAntigravityObservation(usage) && usage.antigravity_ineligible_tiers === undefined) {
    return usage.antigravity_ineligible === true
  }
  return getAntigravityIneligibleTiers(usage, extra).length > 0
}
