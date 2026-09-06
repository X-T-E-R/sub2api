<template>
  <div class="space-y-1 text-[10px]" data-testid="antigravity-quota-panel">
    <div class="space-y-1" data-testid="quota-compact">
      <div v-for="window in windows" :key="window.key" :data-window="window.key">
        <UsageProgressBar
          v-if="window.percent !== null"
          :label="window.label" :label-title="window.title"
          :utilization="window.percent" :resets-at="window.resetTime"
          color="emerald" remaining-capacity
          role="progressbar" :aria-label="window.title"
          :aria-valuenow="window.percent" :aria-valuemin="0" :aria-valuemax="100"
        >
          <template #percent>{{ antigravityPercentLabel(window.percent) }}</template>
        </UsageProgressBar>
        <div v-else class="flex items-center gap-1 text-gray-500 dark:text-gray-400">
          <span class="w-[32px] shrink-0 rounded bg-gray-100 px-1 text-center font-medium dark:bg-gray-800" :title="window.title">{{ window.label }}</span>
          <span class="h-1.5 w-8 shrink-0 rounded-full bg-gray-200 dark:bg-gray-700"></span>
          <span>{{ t('common.unknown') }}</span>
        </div>
      </div>
    </div>
    <div v-if="isStale" class="text-amber-700 dark:text-amber-400">{{ t('admin.accounts.usageWindow.quotaStaleCompact') }}</div>
    <div v-else-if="usage.antigravity_window_state === 'partial' || usage.antigravity_quota_state === 'partial'" class="text-amber-700 dark:text-amber-400">{{ t('admin.accounts.usageWindow.quotaPartialCompact') }}</div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountUsageInfo, AntigravityWindowKey } from '@/types'
import { antigravityWindowPercent, antigravityPercentLabel } from '@/utils/antigravityUsage'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{ usage: AccountUsageInfo }>()
const { t } = useI18n()
const windowDefinitions: Array<{ key: AntigravityWindowKey; label: string; family: string; period: string }> = [
  { key: 'claude_5h', label: 'C 5h', family: 'Claude', period: 'explicitFiveHour' },
  { key: 'gemini_5h', label: 'G 5h', family: 'Gemini', period: 'explicitFiveHour' },
  { key: 'claude_weekly', label: 'C 1w', family: 'Claude', period: 'explicitWeekly' },
  { key: 'gemini_weekly', label: 'G 1w', family: 'Gemini', period: 'explicitWeekly' }
]
const windows = computed(() => windowDefinitions.map(window => {
  const resetTime = props.usage.antigravity_windows?.[window.key]?.reset_time
  return {
    ...window,
    title: window.family + ' ' + t('admin.accounts.usageWindow.' + window.period),
    percent: antigravityWindowPercent(props.usage, window.key),
    resetTime: resetTime && Number.isFinite(Date.parse(resetTime)) ? resetTime : null
  }
}))
const isStale = computed(() => props.usage.antigravity_quota_stale || Object.values(props.usage.antigravity_windows || {}).some(window => window?.stale))
</script>
