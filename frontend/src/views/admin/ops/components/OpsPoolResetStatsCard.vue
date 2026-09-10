<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { opsAPI, type OpsPoolResetStatsResponse } from '@/api/admin/ops'

interface Props {
  refreshToken: number
}

const props = defineProps<Props>()
const { t } = useI18n()

const loading = ref(false)
const errorMessage = ref('')
const stats = ref<OpsPoolResetStatsResponse | null>(null)

const accountRows = computed(() => stats.value?.by_account ?? [])
const protocolRows = computed(() => stats.value?.by_protocol ?? [])

function formatCount(value: unknown): string {
  return typeof value === 'number' && Number.isFinite(value)
    ? Math.max(0, Math.trunc(value)).toLocaleString()
    : '0'
}

function formatTimestamp(value?: string | null): string {
  if (!value) return t('admin.ops.poolResetStats.never')
  const timestamp = new Date(value)
  if (Number.isNaN(timestamp.getTime())) return t('common.unknown')
  return timestamp.toLocaleString()
}

async function loadData() {
  loading.value = true
  errorMessage.value = ''
  try {
    stats.value = await opsAPI.getPoolResetStats()
  } catch (err: any) {
    console.error('[OpsPoolResetStatsCard] Failed to load data', err)
    stats.value = null
    errorMessage.value = err?.message || t('admin.ops.poolResetStats.failedToLoad')
  } finally {
    loading.value = false
  }
}

watch(() => props.refreshToken, () => {
  void loadData()
}, { immediate: true })
</script>

<template>
  <section class="card p-4 md:p-5" data-testid="pool-reset-stats-card">
    <div class="mb-4 flex flex-wrap items-start justify-between gap-3">
      <div>
        <h3 class="text-sm font-bold text-gray-900 dark:text-white">
          {{ t('admin.ops.poolResetStats.title') }}
        </h3>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.poolResetStats.description') }}
        </p>
      </div>
      <button
        type="button"
        class="flex shrink-0 items-center gap-1.5 rounded-lg bg-gray-100 px-3 py-1.5 text-xs font-bold text-gray-700 transition-colors hover:bg-gray-200 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-dark-700 dark:text-gray-300 dark:hover:bg-dark-600"
        :disabled="loading"
        :title="t('common.refresh')"
        @click="loadData"
      >
        <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />
        <span>{{ t('common.refresh') }}</span>
      </button>
    </div>

    <div v-if="loading" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('admin.ops.loadingText') }}
    </div>

    <div v-else-if="errorMessage" class="rounded-xl bg-red-50 p-3 text-xs text-red-600 dark:bg-red-900/20 dark:text-red-400">
      {{ errorMessage }}
    </div>

    <template v-else-if="stats">
      <div class="grid grid-cols-1 divide-y divide-gray-100 border-y border-gray-100 sm:grid-cols-3 sm:divide-x sm:divide-y-0 dark:divide-dark-700 dark:border-dark-700">
        <div class="px-2 py-3 sm:px-4">
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.poolResetStats.triggered') }}</p>
          <p class="mt-1 text-xl font-semibold text-emerald-600 dark:text-emerald-400" data-testid="pool-reset-triggered">
            {{ formatCount(stats.triggered) }}
          </p>
        </div>
        <div class="px-2 py-3 sm:px-4">
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.poolResetStats.suppressed') }}</p>
          <p class="mt-1 text-xl font-semibold text-amber-600 dark:text-amber-400" data-testid="pool-reset-suppressed">
            {{ formatCount(stats.suppressed) }}
          </p>
        </div>
        <div class="px-2 py-3 sm:px-4">
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.poolResetStats.lastReset') }}</p>
          <p class="mt-1 break-words text-sm font-medium text-gray-900 dark:text-white" data-testid="pool-reset-last">
            {{ formatTimestamp(stats.last_reset_at) }}
          </p>
        </div>
      </div>

      <div class="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-2">
        <div class="min-w-0 overflow-hidden rounded-xl border border-gray-200 dark:border-dark-700">
          <div class="border-b border-gray-200 px-3 py-2 text-xs font-semibold text-gray-700 dark:border-dark-700 dark:text-gray-200">
            {{ t('admin.ops.poolResetStats.byAccount') }}
          </div>
          <div v-if="accountRows.length === 0" class="p-3 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.ops.poolResetStats.empty') }}
          </div>
          <div v-else class="max-h-56 divide-y divide-gray-100 overflow-y-auto dark:divide-dark-800">
            <div v-for="row in accountRows" :key="row.account_id" class="flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-xs">
              <span class="font-medium text-gray-900 dark:text-gray-100">
                {{ t('admin.ops.poolResetStats.account') }} #{{ row.account_id }}
              </span>
              <span class="text-gray-500 dark:text-gray-400">
                {{ t('admin.ops.poolResetStats.triggeredShort') }} {{ formatCount(row.triggered) }}
                <span class="mx-1">/</span>
                {{ t('admin.ops.poolResetStats.suppressedShort') }} {{ formatCount(row.suppressed) }}
              </span>
              <time class="w-full text-[11px] text-gray-400 dark:text-gray-500" :datetime="row.last_reset_at || undefined">
                {{ t('admin.ops.poolResetStats.lastReset') }}: {{ formatTimestamp(row.last_reset_at) }}
              </time>
            </div>
          </div>
        </div>

        <div class="min-w-0 overflow-hidden rounded-xl border border-gray-200 dark:border-dark-700">
          <div class="border-b border-gray-200 px-3 py-2 text-xs font-semibold text-gray-700 dark:border-dark-700 dark:text-gray-200">
            {{ t('admin.ops.poolResetStats.byProtocol') }}
          </div>
          <div v-if="protocolRows.length === 0" class="p-3 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.ops.poolResetStats.empty') }}
          </div>
          <div v-else class="max-h-56 divide-y divide-gray-100 overflow-y-auto dark:divide-dark-800">
            <div v-for="row in protocolRows" :key="row.protocol" class="flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-xs">
              <span class="font-medium text-gray-900 dark:text-gray-100">{{ row.protocol }}</span>
              <span class="text-gray-500 dark:text-gray-400">
                {{ t('admin.ops.poolResetStats.triggeredShort') }} {{ formatCount(row.triggered) }}
                <span class="mx-1">/</span>
                {{ t('admin.ops.poolResetStats.suppressedShort') }} {{ formatCount(row.suppressed) }}
              </span>
              <time class="w-full text-[11px] text-gray-400 dark:text-gray-500" :datetime="row.last_reset_at || undefined">
                {{ t('admin.ops.poolResetStats.lastReset') }}: {{ formatTimestamp(row.last_reset_at) }}
              </time>
            </div>
          </div>
        </div>
      </div>

      <p class="mt-3 text-[11px] text-gray-400 dark:text-gray-500" data-testid="pool-reset-persistence">
        {{ t('admin.ops.poolResetStats.persistence', { storage: stats.persistence }) }}
      </p>
    </template>
  </section>
</template>
