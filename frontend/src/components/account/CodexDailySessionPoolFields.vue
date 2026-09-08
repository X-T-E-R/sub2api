<template>
  <div class="mt-4 space-y-3" data-testid="codex-daily-session-pool">
    <div class="flex items-center justify-between gap-4">
      <label :id="`${idPrefix}-label`" class="input-label mb-0">
        {{ t('admin.accounts.openai.dailySessionPool.title') }}
      </label>
      <Toggle
        :model-value="modelValue.enabled"
        :aria-labelledby="`${idPrefix}-label`"
        data-testid="daily-session-pool-toggle"
        @update:model-value="update({ enabled: $event })"
      />
    </div>
    <p class="text-xs text-gray-500 dark:text-gray-400">
      {{ t('admin.accounts.openai.dailySessionPool.description') }}
    </p>
    <div v-if="modelValue.enabled" class="grid grid-cols-2 gap-3">
      <div>
        <label :for="`${idPrefix}-min`" class="input-label">
          {{ t('admin.accounts.openai.dailySessionPool.min') }}
        </label>
        <input
          :id="`${idPrefix}-min`"
          v-model.number="minimum"
          type="number"
          min="1"
          max="1000"
          step="1"
          required
          class="input"
          data-testid="daily-session-pool-min"
        />
      </div>
      <div>
        <label :for="`${idPrefix}-max`" class="input-label">
          {{ t('admin.accounts.openai.dailySessionPool.max') }}
        </label>
        <input
          :id="`${idPrefix}-max`"
          v-model.number="maximum"
          type="number"
          min="1"
          max="1000"
          step="1"
          required
          class="input"
          data-testid="daily-session-pool-max"
        />
      </div>
      <p v-if="!isCodexDailySessionPoolValid(modelValue)" role="alert" class="col-span-2 text-xs text-red-600 dark:text-red-400">
        {{ t('admin.accounts.openai.dailySessionPool.invalidRange') }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { isCodexDailySessionPoolValid } from './codexDailySessionPool'
import type { CodexDailySessionPoolForm } from './codexDailySessionPool'

const props = defineProps<{ modelValue: CodexDailySessionPoolForm; idPrefix: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: CodexDailySessionPoolForm] }>()
const { t } = useI18n()
const update = (value: Partial<CodexDailySessionPoolForm>) => emit('update:modelValue', { ...props.modelValue, ...value })
const minimum = computed({ get: () => props.modelValue.min, set: (min) => update({ min }) })
const maximum = computed({ get: () => props.modelValue.max, set: (max) => update({ max }) })
</script>
