<template>
  <BaseDialog
    :show="show"
    :title="dialogTitle"
    width="wide"
    :close-on-escape="!running"
    @close="handleClose"
  >
    <div class="space-y-5">
      <!-- Target Accounts Summary -->
      <div class="flex flex-wrap items-center gap-2 rounded-lg border border-gray-200 bg-gray-50/80 px-3.5 py-2.5 dark:border-dark-700 dark:bg-dark-800/60">
        <span class="text-xs font-medium text-gray-500 dark:text-dark-400">
          {{ t('admin.accounts.warmup.targetAccounts', { count: accounts.length }) }}
        </span>
        <div class="flex flex-wrap items-center gap-1.5">
          <span
            v-for="acc in previewAccounts"
            :key="acc.id"
            class="inline-flex items-center gap-1.5 rounded-md bg-white px-2 py-0.5 text-xs font-medium text-gray-800 ring-1 ring-gray-200/80 dark:bg-dark-700 dark:text-gray-200 dark:ring-dark-600"
          >
            <span>{{ acc.name }}</span>
            <span class="text-gray-400 dark:text-dark-400">#{{ acc.id }}</span>
            <span
              v-if="!acc.schedulable"
              class="rounded bg-amber-50 px-1 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
            >
              {{ t('admin.accounts.warmup.pausedBadge') }}
            </span>
          </span>
          <span
            v-if="remainingAccountCount > 0"
            class="text-xs text-gray-500 dark:text-dark-400"
          >
            +{{ remainingAccountCount }}
          </span>
        </div>
      </div>

      <!-- Bucket Mode Selector -->
      <div>
        <label class="input-label mb-2 block">{{ t('admin.accounts.warmup.bucketModeLabel') }}</label>
        <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <label
            v-for="option in bucketModeOptions"
            :key="option.value"
            :class="[
              'flex cursor-pointer items-center gap-2.5 rounded-lg border px-3.5 py-2.5 text-sm transition-colors',
              bucketMode === option.value
                ? 'border-primary-500 bg-primary-50/60 font-medium text-primary-900 dark:border-primary-500 dark:bg-primary-900/20 dark:text-primary-100'
                : 'border-gray-200 bg-white text-gray-700 hover:bg-gray-50 dark:border-dark-700 dark:bg-dark-800 dark:text-gray-300 dark:hover:bg-dark-700'
            ]"
          >
            <input
              v-model="bucketMode"
              type="radio"
              :value="option.value"
              :disabled="running"
              class="text-primary-600 focus:ring-primary-500"
            />
            <span>{{ option.label }}</span>
          </label>
        </div>
      </div>

      <!-- Custom Models Input -->
      <div v-if="bucketMode === 'custom_models'">
        <label class="input-label mb-1.5 block">{{ t('admin.accounts.warmup.customModelsLabel') }}</label>
        <input
          v-model="customModelsInput"
          type="text"
          :disabled="running"
          :placeholder="t('admin.accounts.warmup.customModelsPlaceholder')"
          class="input w-full font-mono text-sm"
        />
      </div>

      <!-- Effort & Post-Warmup Refresh Controls -->
      <div class="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div class="w-full sm:w-48">
          <label class="input-label mb-1.5 block">{{ t('admin.accounts.warmup.effortLabel') }}</label>
          <select
            v-model="effort"
            :disabled="running"
            class="input w-full text-sm"
          >
            <option value="low">low</option>
            <option value="medium">medium</option>
            <option value="high">high</option>
          </select>
        </div>

        <label class="flex cursor-pointer items-center gap-2 pb-1 text-sm text-gray-700 dark:text-gray-300">
          <input
            v-model="refreshUsage"
            type="checkbox"
            :disabled="running"
            class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
          />
          <span>{{ t('admin.accounts.warmup.refreshUsageLabel') }}</span>
        </label>
      </div>

      <!-- Request-Level Error -->
      <div
        v-if="requestError"
        class="rounded-lg border border-red-200 bg-red-50 px-3.5 py-2.5 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-950/40 dark:text-red-300"
      >
        {{ requestError }}
      </div>

      <!-- Results List -->
      <div v-if="response" class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700">
        <div class="flex items-center justify-between text-xs font-medium text-gray-600 dark:text-dark-300">
          <span>
            {{
              t('admin.accounts.warmup.summary', {
                total: response.total_accounts,
                succeeded: response.succeeded_accounts,
                failed: response.failed_accounts
              })
            }}
          </span>
        </div>

        <div class="max-h-80 space-y-2.5 overflow-y-auto pr-1">
          <div
            v-for="item in response.results"
            :key="item.account_id"
            class="rounded-lg border border-gray-200 bg-white p-3 dark:border-dark-700 dark:bg-dark-800/80"
          >
            <!-- Account Header -->
            <div class="flex items-center justify-between gap-2">
              <div class="flex items-center gap-2 min-w-0">
                <span class="truncate text-sm font-semibold text-gray-900 dark:text-white">
                  {{ item.account_name || `#${item.account_id}` }}
                </span>
                <span class="text-xs text-gray-400 dark:text-dark-400">#{{ item.account_id }}</span>
                <span
                  v-if="!item.schedulable"
                  class="rounded bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
                >
                  {{ t('admin.accounts.warmup.pausedBadge') }}
                </span>
              </div>
              <span
                :class="[
                  'shrink-0 rounded-full px-2 py-0.5 text-xs font-medium',
                  item.success
                    ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                    : 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'
                ]"
              >
                {{ item.success ? t('admin.accounts.warmup.statusSucceeded') : t('admin.accounts.warmup.statusFailed') }}
              </span>
            </div>

            <!-- Account-Level Error -->
            <p v-if="item.error && item.runs.length === 0" class="mt-1.5 text-xs text-red-600 dark:text-red-400">
              {{ item.error }}
            </p>

            <!-- Per-Bucket Run Rows -->
            <div v-if="item.runs.length > 0" class="mt-2 space-y-1.5">
              <div
                v-for="(run, rIdx) in item.runs"
                :key="`${item.account_id}-${rIdx}-${run.requested_model}`"
                class="flex flex-wrap items-center justify-between gap-2 rounded-md bg-gray-50 px-2.5 py-1.5 text-xs dark:bg-dark-900/50"
              >
                <div class="flex min-w-0 flex-wrap items-center gap-2">
                  <span class="rounded bg-gray-200/80 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-gray-700 dark:bg-dark-700 dark:text-gray-300">
                    {{ run.bucket }}
                  </span>
                  <span class="font-mono text-gray-800 dark:text-gray-200">
                    {{ run.resolved_model || run.requested_model }}
                  </span>
                  <template v-if="!run.error">
                    <span class="max-w-[200px] truncate text-gray-500 dark:text-dark-400" :title="run.response_excerpt">
                      "{{ run.response_excerpt }}"
                    </span>
                    <span
                      :class="[
                        'rounded px-1.5 py-0.5 text-[10px] font-medium',
                        run.answer_matched
                          ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                          : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
                      ]"
                    >
                      {{ run.answer_matched ? t('admin.accounts.warmup.answerMatched') : t('admin.accounts.warmup.answerUnmatched') }}
                    </span>
                  </template>
                  <span v-else class="text-red-600 dark:text-red-400">
                    {{ run.error }}
                  </span>
                </div>
                <span class="shrink-0 font-mono text-[11px] text-gray-400 dark:text-dark-400">
                  {{ run.duration_ms }} ms
                </span>
              </div>
            </div>

            <!-- Refreshed 5h / 7d Quota Strip -->
            <div v-if="item.usage" class="mt-2.5 flex flex-wrap items-center gap-2 border-t border-gray-100 pt-2 text-[11px] dark:border-dark-700">
              <span class="font-medium text-gray-500 dark:text-dark-400">
                {{ t('admin.accounts.warmup.windowsRefreshed') }}
              </span>
              <span
                v-for="win in summarizeWindows(item.usage)"
                :key="win.key"
                class="inline-flex items-center gap-1 rounded bg-gray-100 px-1.5 py-0.5 text-gray-700 dark:bg-dark-700 dark:text-gray-200"
              >
                <span class="font-semibold">{{ win.label }}</span>
                <span>{{ win.percentLabel }}</span>
                <span class="text-gray-400 dark:text-dark-400">·</span>
                <span class="text-gray-500 dark:text-dark-300">{{ win.resetLabel }}</span>
              </span>
            </div>
            <p v-else-if="item.usage_error" class="mt-2 text-xs text-amber-600 dark:text-amber-400">
              {{ item.usage_error }}
            </p>
          </div>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="flex items-center justify-end gap-2">
        <button
          type="button"
          :disabled="running"
          class="btn btn-secondary"
          @click="handleClose"
        >
          {{ t('common.close') }}
        </button>
        <button
          type="button"
          :disabled="!canSubmit"
          class="btn btn-primary inline-flex items-center gap-1.5"
          @click="runWarmup"
        >
          <Icon v-if="running" name="refresh" size="sm" class="animate-spin" />
          <Icon v-else name="bolt" size="sm" />
          <span>{{ running ? t('admin.accounts.warmup.runningButton') : t('admin.accounts.warmup.runButton') }}</span>
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { adminAPI } from '@/api/admin'
import type {
  WarmupAccountsRequest,
  WarmupAccountsResponse,
  WarmupBucketMode
} from '@/api/admin/accounts'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { antigravityPercentLabel, antigravityWindowPercent } from '@/utils/antigravityUsage'
import { formatRelativeTime } from '@/utils/format'
import type { Account, AccountUsageInfo, AntigravityWindowKey } from '@/types'

interface Props {
  show: boolean
  accounts: Account[]
}

interface Emits {
  (e: 'close'): void
  (
    e: 'completed',
    payload: {
      response: WarmupAccountsResponse
      usageByAccountId: Record<number, AccountUsageInfo>
    }
  ): void
}

const props = defineProps<Props>()
const emit = defineEmits<Emits>()

const { t } = useI18n()
const appStore = useAppStore()

const bucketMode = ref<WarmupBucketMode>('auto_buckets')
const customModelsInput = ref('')
const effort = ref('low')
const refreshUsage = ref(true)
const running = ref(false)
const requestError = ref('')
const response = ref<WarmupAccountsResponse | null>(null)

const bucketModeOptions = computed<Array<{ value: WarmupBucketMode; label: string }>>(() => [
  { value: 'auto_buckets', label: t('admin.accounts.warmup.modes.autoBuckets') },
  { value: 'gemini_only', label: t('admin.accounts.warmup.modes.geminiOnly') },
  { value: 'claude_only', label: t('admin.accounts.warmup.modes.claudeOnly') },
  { value: 'custom_models', label: t('admin.accounts.warmup.modes.customModels') }
])

const dialogTitle = computed(() => {
  if (props.accounts.length === 1) {
    return t('admin.accounts.warmup.titleSingle', { name: props.accounts[0].name })
  }
  return t('admin.accounts.warmup.titleBatch', { count: props.accounts.length })
})

const previewAccounts = computed(() => props.accounts.slice(0, 8))
const remainingAccountCount = computed(() => Math.max(0, props.accounts.length - previewAccounts.value.length))

const parsedCustomModels = computed(() =>
  customModelsInput.value
    .split(',')
    .map(item => item.trim())
    .filter(Boolean)
)

const canSubmit = computed(() => {
  if (running.value || props.accounts.length === 0) return false
  if (bucketMode.value === 'custom_models' && parsedCustomModels.value.length === 0) return false
  return true
})

watch(
  () => props.show,
  isOpen => {
    if (isOpen) {
      requestError.value = ''
      response.value = null
    }
  }
)

const handleClose = () => {
  if (running.value) return
  emit('close')
}

const windowDefs: Array<{ key: AntigravityWindowKey; label: string }> = [
  { key: 'gemini_5h', label: 'G 5h' },
  { key: 'gemini_weekly', label: 'G 1w' },
  { key: 'claude_5h', label: 'C 5h' },
  { key: 'claude_weekly', label: 'C 1w' }
]

const summarizeWindows = (usage: AccountUsageInfo) => {
  return windowDefs.map(def => {
    const pct = antigravityWindowPercent(usage, def.key)
    const rawReset = usage.antigravity_windows?.[def.key]?.reset_time
    const validReset = rawReset && Number.isFinite(Date.parse(rawReset)) ? rawReset : null
    return {
      key: def.key,
      label: def.label,
      percentLabel: pct !== null ? antigravityPercentLabel(pct) : '--',
      resetLabel: validReset ? formatRelativeTime(validReset) : t('admin.accounts.warmup.windowPending')
    }
  })
}

const runWarmup = async () => {
  if (!canSubmit.value) return
  running.value = true
  requestError.value = ''

  const payload: WarmupAccountsRequest = {
    bucket_mode: bucketMode.value,
    effort: effort.value,
    refresh_usage: refreshUsage.value,
    concurrency: 3
  }
  if (bucketMode.value === 'custom_models') {
    payload.models = parsedCustomModels.value
  }

  try {
    const res =
      props.accounts.length === 1
        ? await adminAPI.accounts.warmupAccount(props.accounts[0].id, payload)
        : await adminAPI.accounts.batchWarmupAccounts({
            ...payload,
            account_ids: props.accounts.map(a => a.id)
          })

    response.value = res

    const usageByAccountId: Record<number, AccountUsageInfo> = {}
    for (const item of res.results) {
      if (item.usage) {
        usageByAccountId[item.account_id] = item.usage
      }
    }
    emit('completed', { response: res, usageByAccountId })

    if (res.failed_accounts === 0) {
      appStore.showSuccess(
        t('admin.accounts.warmup.completedToast', {
          succeeded: res.succeeded_accounts,
          total: res.total_accounts
        })
      )
    } else {
      appStore.showWarning(
        t('admin.accounts.warmup.partialToast', {
          succeeded: res.succeeded_accounts,
          failed: res.failed_accounts
        })
      )
    }
  } catch (error) {
    requestError.value = extractApiErrorMessage(error, t('admin.accounts.warmup.failedToast'))
  } finally {
    running.value = false
  }
}
</script>
