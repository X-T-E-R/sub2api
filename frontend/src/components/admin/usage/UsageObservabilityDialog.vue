<template>
  <BaseDialog :show="show" :title="t('usage.observability.title')" width="extra-wide" @close="emit('close')">
    <div v-if="loading" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('common.loading') }}
    </div>
    <div v-else-if="error" class="rounded-lg bg-rose-50 p-4 text-sm text-rose-700 dark:bg-rose-500/10 dark:text-rose-300">
      {{ error }}
    </div>
    <div v-else-if="detail" class="space-y-5" data-testid="usage-observability-dialog">
      <dl class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <div v-for="item in summary" :key="item.label" class="rounded-lg border border-gray-200 p-3 dark:border-dark-700">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ item.label }}</dt>
          <dd class="mt-1 break-all text-sm font-medium text-gray-900 dark:text-white">{{ item.value }}</dd>
        </div>
      </dl>

      <div v-if="detail.attempt_ledger" class="space-y-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.observability.attempts') }}</h4>
          <span v-if="detail.attempt_ledger.truncated" class="rounded bg-amber-100 px-2 py-1 text-xs text-amber-700 dark:bg-amber-500/20 dark:text-amber-300">
            {{ t('usage.observability.truncated', { count: detail.attempt_ledger.folded?.count ?? 0 }) }}
          </span>
        </div>
        <div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
          <table class="min-w-full divide-y divide-gray-200 text-xs dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-gray-500 dark:bg-dark-800 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2">#</th>
                <th class="px-3 py-2">{{ t('admin.usage.account') }}</th>
                <th class="px-3 py-2">{{ t('usage.observability.forward') }}</th>
                <th class="px-3 py-2">{{ t('usage.observability.outcome') }}</th>
                <th class="px-3 py-2">{{ t('usage.observability.reason') }}</th>
                <th class="px-3 py-2">{{ t('usage.observability.nextAction') }}</th>
                <th class="px-3 py-2">{{ t('usage.observability.terminal') }}</th>
                <th class="px-3 py-2">{{ t('usage.observability.upstreamRequestId') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 text-gray-700 dark:divide-dark-700 dark:text-gray-300">
              <tr v-for="attempt in detail.attempt_ledger.attempts" :key="attempt.sequence">
                <td class="whitespace-nowrap px-3 py-2">{{ attempt.sequence }}</td>
                <td class="whitespace-nowrap px-3 py-2">#{{ attempt.account_id }} · {{ attempt.platform || '-' }}</td>
                <td class="whitespace-nowrap px-3 py-2 tabular-nums">{{ formatMs(attempt.forward_ms) }}</td>
                <td class="whitespace-nowrap px-3 py-2">{{ attempt.outcome }}</td>
                <td class="whitespace-nowrap px-3 py-2">{{ attempt.reason || (attempt.status_code ? `HTTP ${attempt.status_code}` : '-') }}</td>
                <td class="whitespace-nowrap px-3 py-2">{{ attempt.next_action || '-' }}<span v-if="attempt.wait_after_ms"> · {{ formatMs(attempt.wait_after_ms) }}</span></td>
                <td class="whitespace-nowrap px-3 py-2">{{ attempt.terminal_kind || '-' }}</td>
                <td class="max-w-[240px] truncate px-3 py-2 font-mono" :title="attempt.upstream_request_id">{{ attempt.upstream_request_id || '-' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <div v-else class="rounded-lg bg-gray-50 p-4 text-sm text-gray-500 dark:bg-dark-800 dark:text-gray-400">
        {{ t('usage.observability.noLedger') }}
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminUsageAPI } from '@/api/admin/usage'
import type { AdminUsageObservability } from '@/types'

const props = defineProps<{ show: boolean; usageId: number | null }>()
const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()
const detail = ref<AdminUsageObservability | null>(null)
const loading = ref(false)
const error = ref('')
let controller: AbortController | null = null

const formatMs = (value: number | null | undefined): string => value == null ? '-' : `${value.toLocaleString()} ms`
const formatBool = (value: boolean | null): string => value == null ? t('usage.semanticOutputUnknown') : value ? t('common.yes') : t('common.no')

const summary = computed(() => detail.value ? [
  { label: t('usage.latencyHandler'), value: formatMs(detail.value.handler_duration_ms) },
  { label: t('usage.latencyForward'), value: formatMs(detail.value.forward_duration_ms) },
  { label: t('usage.latencyFirstVisible'), value: formatMs(detail.value.first_visible_output_ms) },
  { label: t('usage.latencyLegacyTtft'), value: formatMs(detail.value.first_token_ms) },
  { label: t('usage.observability.semantic'), value: formatBool(detail.value.semantic_output_seen) },
  { label: t('usage.observability.terminal'), value: detail.value.terminal_kind || '-' },
  { label: t('usage.observability.gatewayRequestId'), value: detail.value.gateway_request_id || '-' },
  { label: t('usage.observability.clientRequestId'), value: detail.value.client_request_id || '-' },
] : [])

watch(() => [props.show, props.usageId] as const, async ([show, usageId]) => {
  controller?.abort()
  controller = null
  if (!show || usageId == null) {
    detail.value = null
    error.value = ''
    return
  }
  loading.value = true
  error.value = ''
  const current = new AbortController()
  controller = current
  try {
    detail.value = await adminUsageAPI.getObservability(usageId, { signal: current.signal })
  } catch (cause) {
    if (!current.signal.aborted) error.value = cause instanceof Error ? cause.message : t('usage.observability.loadFailed')
  } finally {
    if (controller === current) {
      controller = null
      loading.value = false
    }
  }
}, { immediate: true })
</script>
