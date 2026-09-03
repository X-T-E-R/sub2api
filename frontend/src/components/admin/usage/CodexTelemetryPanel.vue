<template>
  <section class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700" data-testid="codex-telemetry-panel">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.codexTelemetry.title') }}</h4>
      <button type="button" class="btn btn-secondary text-xs" data-testid="codex-telemetry-download" @click="download">
        {{ t('usage.codexTelemetry.download') }}
      </button>
    </div>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.codexTelemetry.meaning') }}</p>
    <div class="flex flex-wrap gap-x-5 gap-y-2 text-xs text-gray-600 dark:text-gray-300">
      <div>{{ t('usage.codexTelemetry.transport') }}: {{ telemetry.transport }}</div>
      <div v-if="telemetry.response_id" class="break-all font-mono">{{ t('usage.codexTelemetry.responseId') }}: {{ telemetry.response_id }}</div>
      <div v-if="telemetry.stream_id" class="break-all font-mono">stream_id: {{ telemetry.stream_id }}</div>
      <div v-if="telemetry.connection_reused">{{ t('usage.codexTelemetry.reused') }}</div>
    </div>
    <p v-if="hasContextualObservation" class="text-xs text-amber-700 dark:text-amber-300" data-testid="codex-telemetry-inferred">
      {{ t('usage.codexTelemetry.inferred') }}
    </p>
    <p v-if="telemetry.truncated" class="text-xs text-amber-700 dark:text-amber-300" data-testid="codex-telemetry-truncated">
      {{ t('usage.codexTelemetry.truncated') }}
    </p>
    <p v-if="telemetry.unassociated_events" class="text-xs text-gray-500 dark:text-gray-400">
      {{ t('usage.codexTelemetry.unassociated', { count: telemetry.unassociated_events }) }}
    </p>
    <div v-for="(observation, index) in telemetry.observations" :key="index" class="space-y-2 rounded bg-gray-50 p-3 text-xs dark:bg-dark-800">
      <div class="flex flex-wrap gap-2 text-gray-500 dark:text-gray-400">
        <span class="break-all font-mono">{{ observation.source }}</span>
        <span>{{ t(`usage.codexTelemetry.association.${observation.association}`) }}</span>
      </div>
      <dl class="grid gap-x-4 gap-y-2 text-gray-800 dark:text-gray-200 sm:grid-cols-2">
        <div v-if="observation.engine_ids?.length" class="sm:col-span-2">
          <dt class="text-gray-500 dark:text-gray-400">{{ t('usage.codexTelemetry.engines') }}</dt>
          <dd class="mt-1 break-all font-mono">{{ observation.engine_ids.join(', ') }}</dd>
        </div>
        <div v-for="field in fields(observation)" :key="field.key">
          <dt class="text-gray-500 dark:text-gray-400">{{ t(`usage.codexTelemetry.${field.key}`) }}</dt>
          <dd class="mt-1 break-all font-mono">{{ field.value }}</dd>
        </div>
      </dl>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { saveAs } from 'file-saver'
import type { CodexTelemetryObservation, CodexTelemetrySnapshot } from '@/types/codexTelemetry'

const props = defineProps<{ telemetry: CodexTelemetrySnapshot }>()
const { t } = useI18n()
const hasContextualObservation = computed(() => props.telemetry.observations.some(item => item.association === 'active_response' || (item.association === 'http_response' && item.source !== 'http_headers')))

function fields(observation: CodexTelemetryObservation) {
  const keys = ['faster_model', 'active_limit', 'primary_used_percent', 'primary_window_minutes', 'secondary_used_percent', 'secondary_window_minutes'] as const
  return keys.flatMap(key => observation[key] == null ? [] : [{ key, value: observation[key] }])
}

function download() {
  saveAs(new Blob([JSON.stringify(props.telemetry, null, 2)], { type: 'application/json;charset=utf-8' }), 'codex-telemetry.json')
}
</script>
