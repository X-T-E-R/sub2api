import type {
  AccountUsageInfo,
  AntigravityModelDetail,
  AntigravityModelQuota
} from '@/types'

export type AntigravityQuotaFamily = 'gemini-pro' | 'gemini-flash' | 'gemini-image' | 'claude' | 'other'

export interface AntigravityQuotaRow {
  key: string
  family: AntigravityQuotaFamily
  modelIDs: string[]
  compactLabel: string
  title: string
  utilization: number
  resetTime: string | null
}

const familyOrder: Record<AntigravityQuotaFamily, number> = {
  'gemini-pro': 0,
  'gemini-flash': 1,
  'gemini-image': 2,
  claude: 3,
  other: 4
}

const validQuota = (value: AntigravityModelQuota | undefined): value is AntigravityModelQuota =>
  value != null &&
  typeof value.utilization === 'number' &&
  Number.isFinite(value.utilization) &&
  value.utilization >= 0 &&
  value.utilization <= 100

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
        resetTime: quotaResetTime(entry.quota)
      })
      continue
    }
    const resetTime = quotaResetTime(entry.quota) ?? ''
    const groupKey = `${entry.family}\u0000${entry.quota.utilization}\u0000${resetTime}`
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
      resetTime: quotaResetTime(familyEntries[0].quota)
    })
  }

  return rows.sort((a, b) => {
    const familyDelta = familyOrder[a.family] - familyOrder[b.family]
    return familyDelta || a.key.localeCompare(b.key)
  })
}

const legacyAntigravityTier = (extra?: Record<string, unknown>): string | null => {
  const loadCodeAssist = extra?.load_code_assist as Record<string, unknown> | undefined
  const paidTier = loadCodeAssist?.paidTier as Record<string, unknown> | undefined
  if (typeof paidTier?.id === 'string' && paidTier.id.trim()) return paidTier.id.trim()
  const currentTier = loadCodeAssist?.currentTier as Record<string, unknown> | undefined
  if (typeof currentTier?.id === 'string' && currentTier.id.trim()) return currentTier.id.trim()
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

export const hasAntigravityIneligibleTier = (
  usage: AccountUsageInfo | null | undefined,
  extra?: Record<string, unknown>
): boolean => {
  if (ownsAntigravityObservation(usage)) return usage.antigravity_ineligible === true
  const loadCodeAssist = extra?.load_code_assist as Record<string, unknown> | undefined
  return Array.isArray(loadCodeAssist?.ineligibleTiers) && loadCodeAssist.ineligibleTiers.length > 0
}
