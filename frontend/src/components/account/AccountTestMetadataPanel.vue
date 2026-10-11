<template>
  <div
    class="rounded-xl border border-gray-200 bg-gray-50/70 p-3 dark:border-dark-500 dark:bg-dark-700/40"
    data-testid="account-test-metadata"
  >
    <div class="mb-2">
      <div
        class="text-[11px] font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400"
      >
        {{ t('admin.accounts.testMetadata.title') }}
      </div>
      <p class="mt-0.5 text-[11px] leading-snug text-gray-400 dark:text-gray-500">
        {{ t('admin.accounts.testMetadata.subtitle') }}
      </p>
    </div>

    <dl class="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-2">
      <div v-for="row in rows" :key="row.key" class="min-w-0">
        <dt class="text-[10px] font-medium uppercase tracking-wide text-gray-400 dark:text-gray-500">
          {{ row.label }}
        </dt>
        <dd class="mt-0.5 text-xs" :data-testid="`account-test-metadata-${row.key}`">
          <span
            v-if="!row.value"
            class="italic text-gray-400 dark:text-gray-500"
            data-testid="account-test-metadata-missing"
          >
            {{ row.pending ? t('common.loading') + '...' : t('admin.accounts.testMetadata.notProvided') }}
          </span>
          <span
            v-else
            :class="[
              'text-gray-800 dark:text-gray-100',
              row.mono ? 'break-all font-mono' : 'break-words',
              row.wrap ? 'block max-h-20 overflow-y-auto whitespace-pre-wrap' : ''
            ]"
          >{{ row.value }}</span>
          <span
            v-if="row.value && row.note"
            class="ml-1 text-[10px] text-gray-400 dark:text-gray-500"
          >{{ row.note }}</span>
        </dd>
      </div>
    </dl>

    <p
      v-if="responseModelConflict"
      class="mt-2 flex items-start gap-1.5 rounded-lg border border-amber-200 bg-amber-50 px-2 py-1.5 text-[11px] leading-snug text-amber-700 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-300"
      data-testid="account-test-metadata-conflict"
    >
      <Icon name="exclamationTriangle" size="sm" class="mt-px shrink-0" :stroke-width="2" />
      <span>{{ t('admin.accounts.testMetadata.responseConflictHint') }}</span>
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Icon } from '@/components/icons'
import type { AccountTestEffortSource } from '@/utils/accountTestEffort'
import { accountTestStreamModeLabelKey, metadataText, metadataVerbatim } from '@/utils/accountTestStream'
import type { AccountTestMetadata } from '@/types/accountTestStream'

/**
 * Request/response metadata reported by the account test run.
 *
 * Every field is optional on purpose: a value the run does not report is shown
 * as "not provided" rather than being inferred, and a response-model conflict
 * keeps both models on screen with an explicit warning.
 */
const props = defineProps<{
  metadata: AccountTestMetadata
  /** Model the operator selected, shown before the run reports one. */
  requestedModel?: string
  /** Effort the run is expected to use before it reports `effective_effort`. */
  effortFallback?: string
  effortSource?: AccountTestEffortSource
  /** Prompt the run is expected to send before it reports `effective_prompt`. */
  promptFallback?: string
  promptSource?: 'custom' | 'global' | 'builtin'
  /** True while the global prompt setting is still being read. */
  promptPending?: boolean
}>()

const { t } = useI18n()

const reportedRequestedModel = computed(() => metadataText(props.metadata.requested_model))
const reportedSentModel = computed(
  () => metadataText(props.metadata.upstream_sent_model) || metadataText(props.metadata.model)
)
const reportedResponseModel = computed(() => metadataText(props.metadata.upstream_response_model))
const reportedEffort = computed(() => metadataText(props.metadata.effective_effort))
const reportedPrompt = computed(() => metadataVerbatim(props.metadata.effective_prompt))
const reportedMode = computed(() => metadataText(props.metadata.stream_mode))

const responseModelConflict = computed(() => props.metadata.upstream_response_model_conflict === true)

function streamModeText(mode: string): string {
  const key = accountTestStreamModeLabelKey(mode)
  return key ? t(key) : mode
}

const promptNote = computed(() => {
  if (reportedPrompt.value) return t('admin.accounts.testMetadata.promptReported')
  if (props.promptSource === 'global') return t('admin.accounts.testMetadata.promptGlobal')
  if (props.promptSource === 'builtin') return t('admin.accounts.testMetadata.promptBuiltin')
  if (props.promptSource === 'custom') return t('admin.accounts.testMetadata.promptCustom')
  return ''
})

const effortNote = computed(() => {
  if (reportedEffort.value) return ''
  return props.effortSource === 'model' ? t('admin.accounts.testMetadata.effortFromSuffix') : ''
})

const rows = computed(() => [
  {
    key: 'requested-model',
    label: t('admin.accounts.testMetadata.requestedModel'),
    value: reportedRequestedModel.value || metadataText(props.requestedModel),
    mono: true,
    wrap: false,
    note: '',
    pending: false
  },
  {
    key: 'upstream-sent-model',
    label: t('admin.accounts.testMetadata.upstreamSentModel'),
    value: reportedSentModel.value,
    mono: true,
    wrap: false,
    note: '',
    pending: false
  },
  {
    key: 'upstream-response-model',
    label: t('admin.accounts.testMetadata.upstreamResponseModel'),
    value: reportedResponseModel.value,
    mono: true,
    wrap: false,
    note: '',
    pending: false
  },
  {
    key: 'effective-effort',
    label: t('admin.accounts.testMetadata.effectiveEffort'),
    value: reportedEffort.value || metadataText(props.effortFallback),
    mono: false,
    wrap: false,
    note: effortNote.value,
    pending: false
  },
  {
    key: 'effective-prompt',
    label: t('admin.accounts.testMetadata.effectivePrompt'),
    value: reportedPrompt.value || metadataText(props.promptFallback),
    mono: false,
    wrap: true,
    note: promptNote.value,
    pending: Boolean(props.promptPending) && !reportedPrompt.value && !metadataText(props.promptFallback)
  },
  {
    key: 'stream-mode',
    label: t('admin.accounts.testMetadata.streamModeLabel'),
    value: reportedMode.value ? streamModeText(reportedMode.value) : '',
    mono: false,
    wrap: false,
    note: '',
    pending: false
  }
])
</script>
