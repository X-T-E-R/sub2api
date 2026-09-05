<template>
  <div class="min-w-[256px] max-w-[340px] space-y-2 whitespace-normal text-[10px]" data-testid="antigravity-quota-panel">
    <div class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.usageWindow.remainingQuota') }}</div>
    <div class="grid grid-cols-2 gap-3" data-testid="quota-columns">
      <section v-for="family in families" :key="family.name" :aria-label="family.name" class="contents">
        <h4 class="row-start-1 font-semibold text-gray-800 dark:text-gray-200">{{ family.name }}</h4>
        <div v-for="(window, index) in family.windows" :key="window.key" class="min-w-0 space-y-0.5" :style="{ gridRow: index + 2 }" :data-window="window.key">
          <div class="flex justify-between gap-2 text-gray-600 dark:text-gray-300">
            <span>{{ t(`admin.accounts.usageWindow.${window.label}`) }}</span>
            <span class="tabular-nums font-medium">{{ percent(window.key) === null ? t('common.unknown') : `${percent(window.key)}%` }}</span>
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
    <details v-if="modelRows.length" class="text-gray-500 dark:text-gray-400">
      <summary class="cursor-pointer">{{ t('admin.accounts.usageWindow.modelQuotaDetails') }}</summary>
      <div class="mt-1 space-y-1">
        <UsageProgressBar v-for="row in modelRows" :key="row.key" :label="row.compactLabel || row.family" :label-title="row.title" :utilization="100 - row.utilization" :resets-at="row.resetTime" color="emerald" remaining-capacity />
        <div v-if="usage.antigravity_quota_state === 'partial'" class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.usageWindow.antigravityPartial') }}</div>
        <div v-if="usage.antigravity_quota_stale" class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.usageWindow.antigravityStale') }}</div>
      </div>
    </details>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import type { AccountUsageInfo, AntigravityWindowKey } from '@/types'
import { antigravityWindowPercent, antigravityCreditAmount, buildAntigravityQuotaRows } from '@/utils/antigravityUsage'
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
const percent = (key: AntigravityWindowKey) => antigravityWindowPercent(props.usage, key)
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
