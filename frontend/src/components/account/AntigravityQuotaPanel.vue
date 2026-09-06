<template>
  <div class="space-y-1 text-[10px]" data-testid="antigravity-quota-panel">
    <div class="space-y-1" data-testid="quota-compact">
      <UsageProgressBar
        v-for="window in compactWindows" :key="window.key"
        :label="window.compact" :label-title="window.title"
        :utilization="percent(window.key)!" :resets-at="window.resetTime"
        color="emerald" remaining-capacity
      >
        <template #percent>{{ antigravityPercentLabel(percent(window.key)!) }}</template>
      </UsageProgressBar>
      <template v-if="!compactWindows.length">
        <UsageProgressBar
          v-for="row in compactModels" :key="row.key"
          :label="row.compactLabel || familyLabels[row.family]" :label-title="row.title"
          :utilization="row.remainingPercent" :resets-at="row.resetTime"
          color="emerald" remaining-capacity
        >
          <template #percent>{{ antigravityPercentLabel(row.remainingPercent) }}</template>
        </UsageProgressBar>
        <span v-if="!modelRows.length" class="text-gray-500 dark:text-gray-400">{{ t('common.unknown') }}</span>
      </template>
    </div>
    <details class="text-gray-600 dark:text-gray-400" data-testid="quota-details">
      <summary class="w-fit cursor-pointer rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-blue-500" :title="compactWindows.length ? t('admin.accounts.usageWindow.quotaCompactHint') : undefined">
        {{ t(!compactWindows.length && modelRows.length ? 'admin.accounts.usageWindow.modelQuotaDetails' : 'admin.accounts.usageWindow.remainingQuota') }}
        <span v-if="isStale" class="text-amber-700 dark:text-amber-400"> · {{ t('admin.accounts.usageWindow.quotaStaleCompact') }}</span>
        <span v-else-if="usage.antigravity_window_state === 'partial' || usage.antigravity_quota_state === 'partial'" class="text-amber-700 dark:text-amber-400"> · {{ t('admin.accounts.usageWindow.quotaPartialCompact') }}</span>
      </summary>
      <div class="mt-2 w-full min-w-0 space-y-2 whitespace-normal">
        <div class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.usageWindow.remainingQuota') }}</div>
        <div class="space-y-3" data-testid="quota-windows">
          <section v-for="family in families" :key="family.name" :aria-label="family.name" class="space-y-2">
            <h4 class="font-semibold text-gray-800 dark:text-gray-200">{{ family.name }}</h4>
            <div v-for="window in family.windows" :key="window.key" class="min-w-0 space-y-0.5" :data-window="window.key">
              <div class="flex justify-between gap-2 text-gray-600 dark:text-gray-300">
                <span>{{ t(`admin.accounts.usageWindow.${window.label}`) }}</span>
                <span class="tabular-nums font-medium">{{ percent(window.key) === null ? t('common.unknown') : antigravityPercentLabel(percent(window.key)!) }}</span>
              </div>
              <div
                class="h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-gray-700"
                :role="percent(window.key) === null ? undefined : 'progressbar'"
                :aria-label="`${family.name} ${t(`admin.accounts.usageWindow.${window.label}`)} ${t('admin.accounts.usageWindow.remainingQuota')}`"
                :aria-valuenow="percent(window.key) ?? undefined"
                :aria-valuemin="0"
                :aria-valuemax="100"
              >
                <div v-if="percent(window.key) !== null" class="h-full rounded-full bg-emerald-500" :style="{ width: `${percent(window.key)}%` }"></div>
              </div>
              <div class="text-gray-500 dark:text-gray-400" :title="usage.antigravity_windows?.[window.key]?.reset_time">
                {{ resetLabel(window.key) }}
              </div>
              <div v-if="usage.antigravity_windows?.[window.key]?.stale" class="text-amber-600 dark:text-amber-400">
                {{ t('admin.accounts.usageWindow.antigravityStale') }}
              </div>
              <div v-if="usage.antigravity_windows?.[window.key]?.observed_at" class="text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.usageWindow.observedAt', { time: localTime(usage.antigravity_windows?.[window.key]?.observed_at) }) }}
              </div>
              <div v-if="usage.antigravity_windows?.[window.key]?.source_bucket_id" class="break-all text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.usageWindow.quotaSource', { source: usage.antigravity_windows?.[window.key]?.source_bucket_id }) }}
              </div>
            </div>
          </section>
        </div>
        <div v-if="usage.antigravity_window_state !== 'available'" class="text-gray-500 dark:text-gray-400">
          {{ t(usage.antigravity_window_state === 'partial' ? 'admin.accounts.usageWindow.explicitWindowsPartial' : 'admin.accounts.usageWindow.explicitWindowsUnavailable') }}
        </div>
        <div class="border-t border-gray-100 pt-1 text-gray-600 dark:border-gray-700 dark:text-gray-300">
          <span>{{ t('admin.accounts.aiCreditsBalance') }}</span>
          <div v-for="(credit, index) in usage.ai_credits || []" :key="index" class="flex justify-between gap-2 break-all">
            <span>{{ credit.credit_type || t('common.unknown') }}</span>
            <span class="tabular-nums">{{ antigravityCreditAmount(credit) ?? t('common.unknown') }}</span>
          </div>
          <span v-if="!usage.ai_credits?.length">: {{ t('common.unknown') }}</span>
        </div>
        <div v-if="modelRows.length" class="text-gray-500 dark:text-gray-400">
          <div>{{ t('admin.accounts.usageWindow.modelQuotaDetails') }}</div>
          <div class="mt-1 space-y-1">
            <div v-for="row in modelRows" :key="row.key">
              <div class="break-words">{{ row.title }}</div>
              <UsageProgressBar :label="row.compactLabel || familyLabels[row.family]" :label-title="row.title" :utilization="row.remainingPercent" :resets-at="row.resetTime" color="emerald" remaining-capacity>
                <template #percent>{{ antigravityPercentLabel(row.remainingPercent) }}</template>
              </UsageProgressBar>
            </div>
            <div v-if="usage.antigravity_quota_state === 'partial'" class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.usageWindow.antigravityPartial') }}</div>
            <div v-if="usage.antigravity_quota_stale" class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.usageWindow.antigravityStale') }}</div>
          </div>
        </div>
      </div>
    </details>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import type { AccountUsageInfo, AntigravityWindowKey } from '@/types'
import { antigravityWindowPercent, antigravityPercentLabel, antigravityCreditAmount, buildAntigravityQuotaRows } from '@/utils/antigravityUsage'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{ usage: AccountUsageInfo }>()
const { t, locale } = useI18n()
const families: Array<{ name: string; windows: Array<{ key: AntigravityWindowKey; label: string }> }> = [
  { name: 'Claude', windows: [{ key: 'claude_5h', label: 'explicitFiveHour' }, { key: 'claude_weekly', label: 'explicitWeekly' }] },
  { name: 'Gemini', windows: [{ key: 'gemini_5h', label: 'explicitFiveHour' }, { key: 'gemini_weekly', label: 'explicitWeekly' }] }
]
const now = ref(Date.now())
const { pause, resume } = useIntervalFn(() => { now.value = Date.now() }, 60_000, { immediate: false })
watch(() => props.usage.antigravity_windows, (windows) => {
  now.value = Date.now()
  if (Object.values(windows || {}).some((window) => window?.reset_time)) resume()
  else pause()
}, { immediate: true })
const modelRows = computed(() => buildAntigravityQuotaRows(props.usage))
const compactModels = computed(() => [...modelRows.value].sort((a, b) => a.remainingPercent - b.remainingPercent).slice(0, 2))
const familyLabels = { 'gemini-pro': 'G Pro', 'gemini-flash': 'G Fl', 'gemini-image': 'G Im', claude: 'C', other: '?' }
const compactWindows = computed(() => families.flatMap(family => family.windows
  .filter(window => percent(window.key) !== null)
  .sort((a, b) => percent(a.key)! - percent(b.key)!)
  .slice(0, 1)
  .map(window => ({
    ...window,
    resetTime: validResetTime(props.usage.antigravity_windows?.[window.key]?.reset_time),
    compact: `${family.name === 'Claude' ? 'C' : 'G'} ${window.label === 'explicitFiveHour' ? '5h' : '7d'}`,
    title: `${family.name} ${t(`admin.accounts.usageWindow.${window.label}`)}`
  }))))
const isStale = computed(() => props.usage.antigravity_quota_stale || Object.values(props.usage.antigravity_windows || {}).some(window => window?.stale))
const percent = (key: AntigravityWindowKey) => antigravityWindowPercent(props.usage, key)
const validResetTime = (value?: string) => value && Number.isFinite(Date.parse(value)) ? value : null
const localTime = (value?: string) => value && Number.isFinite(Date.parse(value))
  ? new Date(value).toLocaleString(locale.value, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
  : t('common.unknown')
const resetLabel = (key: AntigravityWindowKey) => {
  const value = props.usage.antigravity_windows?.[key]?.reset_time
  if (!value || !Number.isFinite(Date.parse(value))) return t('admin.accounts.usageWindow.resetUnknown')
  const minutes = Math.ceil((Date.parse(value) - now.value) / 60_000)
  if (minutes <= 0) return t('admin.accounts.usageWindow.resetElapsed', { time: localTime(value) })
  const duration = minutes >= 1440 ? `${Math.floor(minutes / 1440)}d ${Math.floor(minutes % 1440 / 60)}h`
    : minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m` : `${minutes}m`
  return t('admin.accounts.usageWindow.resetCountdown', { duration, time: localTime(value) })
}
</script>
