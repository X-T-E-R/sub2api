<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.testAccountConnection')"
    width="normal"
    @close="handleClose"
  >
    <div class="space-y-4">
      <!-- Account Info Card -->
      <div
        v-if="account"
        class="flex items-center justify-between rounded-xl border border-gray-200 bg-gradient-to-r from-gray-50 to-gray-100 p-3 dark:border-dark-500 dark:from-dark-700 dark:to-dark-600"
      >
        <div class="flex items-center gap-3">
          <div
            class="flex h-10 w-10 items-center justify-center rounded-lg bg-gradient-to-br from-primary-500 to-primary-600"
          >
            <Icon name="play" size="md" class="text-white" :stroke-width="2" />
          </div>
          <div>
            <div class="font-semibold text-gray-900 dark:text-gray-100">{{ account.name }}</div>
            <div class="flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
              <span
                class="rounded bg-gray-200 px-1.5 py-0.5 text-[10px] font-medium uppercase dark:bg-dark-500"
              >
                {{ account.type }}
              </span>
              <span>{{ t('admin.accounts.account') }}</span>
            </div>
          </div>
        </div>
        <span
          :class="[
            'rounded-full px-2.5 py-1 text-xs font-semibold',
            account.status === 'active'
              ? 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400'
              : 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
          ]"
        >
          {{ account.status }}
        </span>
      </div>

      <div class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.selectTestModel') }}
        </label>
        <Select
          v-model="selectedModelId"
          :options="selectableModels"
          :disabled="loadingModels || status === 'connecting'"
          value-key="id"
          label-key="display_name"
          data-testid="account-test-model"
          :placeholder="loadingModels ? t('common.loading') + '...' : t('admin.accounts.selectTestModel')"
        />
      </div>

      <div v-if="isOpenAIAccount" class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.openai.testMode') }}
        </label>
        <Select
          v-model="testMode"
          :options="openAITestModeOptions"
          :disabled="status === 'connecting'"
        />
      </div>

      <div v-if="showEffortSelect" class="space-y-1.5">
        <label
          for="account-test-effort"
          class="text-sm font-medium text-gray-700 dark:text-gray-300"
        >
          {{ t('admin.accounts.testEffort.label') }}
        </label>
        <Select
          id="account-test-effort"
          :model-value="effortSelectionValue"
          :options="effortOptions"
          :disabled="status === 'connecting' || Boolean(modelEffortSuffix)"
          :searchable="false"
          :aria-label="t('admin.accounts.testEffort.label')"
          data-testid="account-test-effort"
          @update:model-value="updateEffortSelection"
        />
        <p
          class="text-xs text-gray-500 dark:text-gray-400"
          data-testid="account-test-effort-hint"
        >
          {{ effortHint }}
        </p>
      </div>

      <div v-if="supportsImageTest" class="space-y-1.5">
        <TextArea
          v-model="testPrompt"
          :label="t('admin.accounts.imagePromptLabel')"
          :placeholder="t('admin.accounts.imagePromptPlaceholder')"
          :hint="t('admin.accounts.imageTestHint')"
          :disabled="status === 'connecting'"
          rows="3"
        />
      </div>

      <!-- Request / response metadata reported by the run -->
      <AccountTestMetadataPanel
        :metadata="testMetadata"
        :requested-model="selectedModelId"
        :effort-fallback="effectiveEffortLabel"
        :effort-source="effectiveEffortSource"
        :prompt-fallback="effectivePromptFallback"
        :prompt-source="effectivePromptSource"
        :prompt-pending="effectivePromptPending"
      />

      <!-- Terminal Output -->
      <div class="group relative">
        <div
          class="overflow-hidden rounded-xl border border-gray-700 bg-gray-900 dark:border-gray-800 dark:bg-black"
        >
          <!-- Response mode reported by the run; outside the scrolling area so
               it never covers or gets covered by the streamed text. -->
          <div v-if="streamModeBadge" class="border-b border-white/10 px-4 py-1.5">
            <span
              class="rounded-full bg-white/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-gray-400"
              data-testid="account-test-terminal-mode"
            >
              {{ streamModeBadge }}
            </span>
          </div>
          <div
            ref="terminalRef"
            data-testid="account-test-terminal"
            class="max-h-[240px] min-h-[120px] overflow-y-auto p-4 font-mono text-sm"
          >

            <!-- Status Line -->
            <div v-if="status === 'idle'" class="flex items-center gap-2 text-gray-500">
              <Icon name="play" size="sm" :stroke-width="2" />
              <span>{{ t('admin.accounts.readyToTest') }}</span>
            </div>
            <div v-else-if="status === 'connecting'" class="flex items-center gap-2 text-yellow-400">
              <Icon name="refresh" size="sm" class="animate-spin" :stroke-width="2" />
              <span>{{ t('admin.accounts.connectingToApi') }}</span>
            </div>

            <!-- Output Lines -->
            <div v-for="(line, index) in outputLines" :key="index" :class="line.class">
              {{ line.text }}
            </div>

            <!-- Streaming Content -->
            <div v-if="streamingContent" class="text-green-400">
              {{ streamingContent }}<span class="animate-pulse">_</span>
            </div>

            <!-- Result Status -->
            <div
              v-if="status === 'success'"
              data-testid="account-test-result"
              data-status="success"
              class="mt-3 flex items-center gap-2 border-t border-gray-700 pt-3 text-green-400"
            >
              <Icon name="check" size="sm" :stroke-width="2" />
              <span>{{ t('admin.accounts.testCompleted') }}</span>
            </div>
            <div
              v-else-if="status === 'error'"
              data-testid="account-test-result"
              data-status="error"
              class="mt-3 flex items-center gap-2 border-t border-gray-700 pt-3 text-red-400"
            >
              <Icon name="x" size="sm" :stroke-width="2" />
              <span>{{ errorMessage }}</span>
            </div>
          </div>
        </div>

        <!-- Copy Button -->
        <button
          v-if="outputLines.length > 0"
          @click="copyOutput"
          class="absolute right-2 top-2 rounded-lg bg-gray-800/80 p-1.5 text-gray-400 opacity-0 transition-all hover:bg-gray-700 hover:text-white group-hover:opacity-100"
          :title="t('admin.accounts.copyOutput')"
        >
          <Icon name="link" size="sm" :stroke-width="2" />
        </button>
      </div>

      <div v-if="generatedImages.length > 0" class="space-y-2">
        <div class="text-xs font-medium text-gray-600 dark:text-gray-300">
          {{ t('admin.accounts.imagePreview') }}
        </div>
        <div class="flex flex-wrap justify-center gap-3">
          <div
            v-for="(image, index) in generatedImages"
            :key="`${image.url}-${index}`"
            class="group/img relative cursor-pointer overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm transition hover:border-primary-300 hover:shadow-md dark:border-dark-500 dark:bg-dark-700"
            @click="previewImageUrl = image.url"
          >
            <img :src="image.url" :alt="`test-image-${index + 1}`" class="max-h-[360px] w-full object-contain" />
            <div class="absolute inset-0 flex items-center justify-center bg-black/0 transition-colors group-hover/img:bg-black/20">
              <Icon name="eye" size="lg" class="text-white opacity-0 drop-shadow-lg transition-opacity group-hover/img:opacity-100" :stroke-width="2" />
            </div>
            <div class="border-t border-gray-100 px-3 py-1.5 text-xs text-gray-500 dark:border-dark-500 dark:text-gray-300">
              {{ image.mimeType || 'image/*' }}
            </div>
          </div>
        </div>
      </div>

      <!-- Image Lightbox -->
      <Teleport to="body">
        <Transition name="fade">
          <div
            v-if="previewImageUrl"
            class="fixed inset-0 z-[100] flex items-center justify-center bg-black/80 p-4"
            @click.self="previewImageUrl = ''"
          >
            <button
              class="absolute right-4 top-4 rounded-full bg-black/50 p-2 text-white transition-colors hover:bg-black/70"
              @click="previewImageUrl = ''"
            >
              <Icon name="x" size="lg" :stroke-width="2" />
            </button>
            <img
              :src="previewImageUrl"
              alt="preview"
              class="max-h-[90vh] max-w-[90vw] rounded-lg object-contain shadow-2xl"
            />
          </div>
        </Transition>
      </Teleport>

      <!-- Test Info -->
      <div class="flex items-center justify-between px-1 text-xs text-gray-500 dark:text-gray-400">
        <div class="flex items-center gap-3">
          <span class="flex items-center gap-1">
            <Icon name="grid" size="sm" :stroke-width="2" />
            {{ t('admin.accounts.testModel') }}
          </span>
        </div>
        <span class="flex items-center gap-1">
          <Icon name="chat" size="sm" :stroke-width="2" />
          {{
            supportsImageTest
              ? t('admin.accounts.imageTestMode')
              : textPromptSummary
          }}
        </span>
      </div>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button
          @click="handleClose"
          class="rounded-lg bg-gray-100 px-4 py-2 text-sm font-medium text-gray-700 transition-colors hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-300 dark:hover:bg-dark-500"
        >
          {{ t('common.close') }}
        </button>
        <button
          @click="startTest"
          :disabled="status === 'connecting' || !selectedModelId"
          data-testid="account-test-start"
          :class="[
            'flex items-center gap-2 rounded-lg px-4 py-2 text-sm font-medium transition-all',
            status === 'connecting' || !selectedModelId
              ? 'cursor-not-allowed bg-primary-400 text-white'
              : status === 'success'
                ? 'bg-green-500 text-white hover:bg-green-600'
                : status === 'error'
                  ? 'bg-orange-500 text-white hover:bg-orange-600'
                  : 'bg-primary-500 text-white hover:bg-primary-600'
          ]"
        >
          <Icon
            v-if="status === 'connecting'"
            name="refresh"
            size="sm"
            class="animate-spin"
            :stroke-width="2"
          />
          <Icon v-else-if="status === 'idle'" name="play" size="sm" :stroke-width="2" />
          <Icon v-else name="refresh" size="sm" :stroke-width="2" />
          <span>
            {{
              status === 'connecting'
                ? t('admin.accounts.testing')
                : status === 'idle'
                  ? t('admin.accounts.startTest')
                  : t('admin.accounts.retry')
            }}
          </span>
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import TextArea from '@/components/common/TextArea.vue'
import AccountTestMetadataPanel from '@/components/account/AccountTestMetadataPanel.vue'
import { Icon } from '@/components/icons'
import { useClipboard } from '@/composables/useClipboard'
import { buildApiUrl } from '@/api/client'
import { adminAPI } from '@/api/admin'
import {
  ACCOUNT_TEST_DEFAULT_EFFORT_VALUE,
  intersectReasoningLevels,
  normalizeReasoningDefaultLevel,
  normalizeReasoningLevels,
  readModelCapability,
  readModelMapping,
  readUpstreamCatalogMetadata,
  resolveDefaultEffortLevel,
  resolveEffectiveEffort,
  resolveModelEffortFamily,
  resolvePublishedDefaultLevel,
  type AccountTestEffortSource
} from '@/utils/accountTestEffort'
import {
  accountTestStreamModeLabelKey,
  consumeAccountTestStream,
  mergeAccountTestMetadata
} from '@/utils/accountTestStream'
import type {
  AccountTestMetadata,
  AccountTestModelOption,
  AccountTestStreamEvent,
  AccountTestStreamRequest
} from '@/types/accountTestStream'
import type { Account, ClaudeModel } from '@/types'

const { t } = useI18n()
const { copyToClipboard } = useClipboard()

interface OutputLine {
  text: string
  class: string
}

interface PreviewImage {
  url: string
  mimeType?: string
}

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const terminalRef = ref<HTMLElement | null>(null)
const status = ref<'idle' | 'connecting' | 'success' | 'error'>('idle')
const outputLines = ref<OutputLine[]>([])
const streamingContent = ref('')
const errorMessage = ref('')
const availableModels = ref<AccountTestModelOption[]>([])
const selectedModelId = ref('')
const testPrompt = ref('')
const loadingModels = ref(false)
const reasoningEffort = ref(ACCOUNT_TEST_DEFAULT_EFFORT_VALUE)
/** Global enabled names and the shared default, re-read on every open. */
const globalEffortLevels = ref<string[]>([])
const globalEffortDefaultLevel = ref('')
const globalEffortLoading = ref(false)
const testMetadata = ref<AccountTestMetadata>({})
const globalTestPrompt = ref('')
const globalTestPromptDefault = ref('')
const globalTestPromptLoading = ref(false)
let abortController: AbortController | null = null
const generatedImages = ref<PreviewImage[]>([])
const testMode = ref<'default' | 'compact'>('default')
const isOpenAIAccount = computed(() => props.account?.platform === 'openai')
const openAITestModeOptions = computed(() => [
  { value: 'default', label: t('admin.accounts.openai.testModeDefault') },
  { value: 'compact', label: t('admin.accounts.openai.testModeCompact') }
])
const previewImageUrl = ref('')
const prioritizedGeminiModels = ['gemini-3.1-flash-image', 'gemini-2.5-flash-image', 'gemini-3.5-flash', 'gemini-2.5-flash', 'gemini-2.5-pro', 'gemini-3-flash-preview', 'gemini-3-pro-preview', 'gemini-2.0-flash']
const supportsGeminiImageTest = computed(() => {
  const modelID = selectedModelId.value.toLowerCase()
  if (!modelID.startsWith('gemini-') || !modelID.includes('-image')) return false

  return props.account?.platform === 'gemini' || (props.account?.platform === 'antigravity' && props.account?.type === 'apikey')
})

const supportsOpenAIImageTest = computed(() => {
  const modelID = selectedModelId.value.toLowerCase()
  if (!modelID.startsWith('gpt-image-')) return false
  return props.account?.platform === 'openai'
})

const supportsImageTest = computed(() => supportsGeminiImageTest.value || supportsOpenAIImageTest.value)

/**
 * Antigravity OAuth/upstream tests resolve the model through the effort
 * mapping policy; API-key accounts are routed to the native Gemini/Claude
 * tests, which ignore it.
 */
const isAntigravityEffortAccount = computed(
  () => props.account?.platform === 'antigravity' && props.account?.type !== 'apikey'
)

/**
 * The account's own request→upstream mapping. Its keys are the models this
 * account can actually be tested with, and an entry whose target is a formal
 * effort template is the only thing that authorizes an effort level.
 */
const accountModelMapping = computed(() => readModelMapping(props.account?.credentials))
const catalogMetadata = computed(() => readUpstreamCatalogMetadata(props.account?.extra))

const selectableModels = computed<AccountTestModelOption[]>(() => {
  const keys = Object.keys(accountModelMapping.value)
  if (!isAntigravityEffortAccount.value || keys.length === 0) return availableModels.value
  return keys.map((id) => ({
    id,
    type: 'model',
    display_name: catalogMetadata.value[id]?.displayName || id,
    created_at: ''
  }))
})

const selectedModel = computed(() =>
  selectableModels.value.find((model) => model.id === selectedModelId.value)
)

/**
 * Capability of the selected model: the backend projection for the account's
 * mapping when it exists, otherwise whatever the model list publishes. Levels
 * are never derived from the globally configured names.
 */
const selectedCapability = computed(() =>
  catalogMetadata.value[selectedModelId.value] ?? readModelCapability(selectedModel.value)
)

/** Levels the account itself publishes for the selected model. */
const publishedEffortLevels = computed(() => selectedCapability.value.levels)

/** Levels whose suffix the backend resolver can actually resolve. */
const suffixEffortLevels = computed(() =>
  isAntigravityEffortAccount.value ? globalEffortLevels.value : publishedEffortLevels.value
)

/** Formal effort family of the selected entry; `null` for literal variants. */
const effortFamily = computed(() =>
  isAntigravityEffortAccount.value
    ? resolveModelEffortFamily(
        selectedModelId.value,
        suffixEffortLevels.value,
        accountModelMapping.value
      )
    : null
)

/**
 * Levels the selector offers. An effort family is limited to the names the
 * global effort settings enable, because those are the only suffixes the
 * backend resolver recognizes. A model whose own native levels are published
 * keeps them as declared: native thinking is not a suffix policy. Anything else
 * offers nothing rather than a fabricated level.
 */
const supportedEffortLevels = computed(() => {
  if (!isAntigravityEffortAccount.value) return publishedEffortLevels.value
  if (effortFamily.value) {
    return intersectReasoningLevels(publishedEffortLevels.value, globalEffortLevels.value)
  }
  return selectedCapability.value.nativeLevels ? publishedEffortLevels.value : []
})

/** A model ID that names an enabled level decides the effort by itself. */
const modelEffortSuffix = computed(() => effortFamily.value?.level ?? '')

/**
 * Level the selector names for "use default", or `''` when it cannot be
 * resolved. An effort template's own default wins over the global one; a bare
 * `{effort}` defers to the shared global default, which may have moved since
 * the catalog snapshot was written, so metadata is not trusted for it. The run
 * itself always leaves the default to the server.
 */
const defaultEffortLevel = computed(() => {
  if (isAntigravityEffortAccount.value) {
    const family = effortFamily.value
    if (family) {
      return resolvePublishedDefaultLevel(supportedEffortLevels.value, [
        family.template.defaultEffort,
        globalEffortDefaultLevel.value
      ])
    }
    return resolvePublishedDefaultLevel(supportedEffortLevels.value, [
      selectedCapability.value.defaultLevel
    ])
  }
  return resolveDefaultEffortLevel({
    levels: supportedEffortLevels.value,
    metadataDefaultLevel: selectedCapability.value.defaultLevel
  })
})

/**
 * True when this selection can carry a reasoning effort at all: an authorized
 * effort family, the model's own native thinking levels, or - for other
 * providers - the levels the model list publishes.
 */
const effortApplies = computed(() => {
  if (!isAntigravityEffortAccount.value) return supportedEffortLevels.value.length > 0
  return effortFamily.value !== null || selectedCapability.value.nativeLevels
})

const showEffortSelect = computed(() => effortApplies.value)

const effectiveEffort = computed(() =>
  resolveEffectiveEffort({
    selection: reasoningEffort.value,
    modelSuffix: modelEffortSuffix.value
  })
)

const effectiveEffortSource = computed<AccountTestEffortSource>(() => effectiveEffort.value.source)

/**
 * What the selector shows. A model ID suffix locks the control and displays the
 * level that will actually be sent, so the visible value never disagrees with
 * the request.
 */
const effortSelectionValue = computed(() =>
  modelEffortSuffix.value || reasoningEffort.value
)

const updateEffortSelection = (value: string | number | boolean | null) => {
  if (typeof value === 'string') reasoningEffort.value = value
}

const defaultEffortOptionLabel = computed(() =>
  defaultEffortLevel.value
    ? t('admin.accounts.testEffort.useDefaultWithLevel', { level: defaultEffortLevel.value })
    : t('admin.accounts.testEffort.useDefault')
)

const effortOptions = computed(() => [
  { value: ACCOUNT_TEST_DEFAULT_EFFORT_VALUE, label: defaultEffortOptionLabel.value },
  ...supportedEffortLevels.value.map((level) => ({ value: level, label: level }))
])

const effortHint = computed(() => {
  if (modelEffortSuffix.value) {
    return t('admin.accounts.testEffort.suffixOverrides', { level: modelEffortSuffix.value })
  }
  if (supportedEffortLevels.value.length > 0) {
    return t('admin.accounts.testEffort.hint')
  }
  if (publishedEffortLevels.value.length > 0) {
    return t('admin.accounts.testEffort.levelsGloballyDisabled')
  }
  return selectedCapability.value.accountProven
    ? t('admin.accounts.testEffort.levelsUnavailable')
    : t('admin.accounts.testEffort.levelsUnproven')
})

/**
 * Effort shown before the run reports the authoritative `effective_effort`.
 * A literal variant applies no effort, so nothing is claimed for it.
 */
const effectiveEffortLabel = computed(() => {
  if (!effortApplies.value) return ''
  return effectiveEffort.value.source === 'default'
    ? defaultEffortOptionLabel.value
    : effectiveEffort.value.effort
})

const effectivePromptFallback = computed(() =>
  supportsImageTest.value
    ? testPrompt.value.trim()
    : globalTestPrompt.value || globalTestPromptDefault.value
)

const effectivePromptSource = computed<'custom' | 'global' | 'builtin' | undefined>(() => {
  if (supportsImageTest.value) {
    const value = testPrompt.value.trim()
    if (!value) return undefined
    return value === t('admin.accounts.imagePromptDefault').trim() ? 'builtin' : 'custom'
  }
  if (globalTestPrompt.value) return 'global'
  return globalTestPromptDefault.value ? 'builtin' : undefined
})

const effectivePromptPending = computed(
  () => !supportsImageTest.value && globalTestPromptLoading.value
)

const streamModeBadge = computed(() => {
  const mode = (testMetadata.value.stream_mode || '').trim()
  if (!mode) return ''
  const key = accountTestStreamModeLabelKey(mode)
  return key ? t(key) : mode
})

/** Footer summary length; the full value stays in the metadata panel. */
const FOOTER_PROMPT_MAX_CHARS = 48

const textPromptSummary = computed(() => {
  const prompt = effectivePromptFallback.value
  if (!prompt) {
    return t('admin.accounts.testPromptSummary', {
      prompt: t('admin.accounts.testMetadata.notProvided')
    })
  }
  return t('admin.accounts.testPromptSummary', {
    prompt:
      prompt.length > FOOTER_PROMPT_MAX_CHARS
        ? `${prompt.slice(0, FOOTER_PROMPT_MAX_CHARS)}…`
        : prompt
  })
})

const loadGlobalTestPrompt = async () => {
  globalTestPromptLoading.value = true
  try {
    const settings = await adminAPI.settings.getModelTestPromptSettings()
    globalTestPrompt.value = typeof settings?.prompt === 'string' ? settings.prompt.trim() : ''
    globalTestPromptDefault.value =
      typeof settings?.default_prompt === 'string' ? settings.default_prompt.trim() : ''
  } catch (error) {
    // The modal still works: without the setting the backend keeps using its
    // own default, and the panel does not claim a prompt it never read.
    console.error('Failed to load the global test prompt:', error)
    globalTestPrompt.value = ''
    globalTestPromptDefault.value = ''
  } finally {
    globalTestPromptLoading.value = false
  }
}

/** Keeps the caller's element type, which carries provider capability fields. */
const sortTestModels = <T extends ClaudeModel>(models: T[]): T[] => {
  const priorityMap = new Map(prioritizedGeminiModels.map((id, index) => [id, index]))

  return [...models].sort((a, b) => {
    const aPriority = priorityMap.get(a.id) ?? Number.MAX_SAFE_INTEGER
    const bPriority = priorityMap.get(b.id) ?? Number.MAX_SAFE_INTEGER
    if (aPriority !== bPriority) return aPriority - bPriority
    return 0
  })
}

/**
 * Picks a sane default from the entries this account can actually test: the
 * projected mapping keys when they exist, otherwise the model list.
 */
const applyDefaultSelection = () => {
  const options = selectableModels.value
  if (options.length === 0) {
    selectedModelId.value = ''
    return
  }
  if (options.some((model) => model.id === selectedModelId.value)) return
  if (props.account?.platform === 'gemini') {
    selectedModelId.value = options[0].id
    return
  }
  // Prefer Sonnet when the account serves it, otherwise use the first entry.
  const sonnetModel = options.find((model) => model.id.includes('sonnet'))
  selectedModelId.value = sonnetModel?.id || options[0].id
}

// Load available models when modal opens
watch(
  () => props.show,
  async (newVal) => {
    if (newVal && props.account) {
      testPrompt.value = ''
      testMode.value = 'default'
      reasoningEffort.value = ACCOUNT_TEST_DEFAULT_EFFORT_VALUE
      globalEffortLevels.value = []
      globalEffortDefaultLevel.value = ''
      resetState()
      await Promise.all([
        loadAvailableModels(),
        loadGlobalTestPrompt(),
        loadGlobalEffortSettings()
      ])
      applyDefaultSelection()
    } else {
      abortStream()
    }
  }
)

// A level the newly selected model does not publish must not stay selected.
watch(supportedEffortLevels, (levels) => {
  if (!reasoningEffort.value) return
  if (!levels.includes(reasoningEffort.value)) {
    reasoningEffort.value = ACCOUNT_TEST_DEFAULT_EFFORT_VALUE
  }
})

watch(selectedModelId, () => {
  if (supportsImageTest.value && !testPrompt.value.trim()) {
    testPrompt.value = t('admin.accounts.imagePromptDefault')
  }
})

/**
 * Reads the global effort settings. They authorize which level names the
 * backend resolver recognizes and supply the shared default of a bare
 * `{effort}` template, so they must be read per open instead of cached: the
 * operator can change them at runtime, while a synced catalog snapshot cannot.
 */
const loadGlobalEffortSettings = async () => {
  if (!isAntigravityEffortAccount.value) return
  globalEffortLoading.value = true
  try {
    const settings = await adminAPI.settings.getAntigravityModelEffortSettings()
    globalEffortLevels.value = normalizeReasoningLevels(settings?.levels)
    globalEffortDefaultLevel.value = normalizeReasoningDefaultLevel(
      settings?.default_effort,
      globalEffortLevels.value
    )
  } catch (error) {
    console.error('Failed to load the global Antigravity effort settings:', error)
    globalEffortLevels.value = []
    globalEffortDefaultLevel.value = ''
  } finally {
    globalEffortLoading.value = false
  }
}

const loadAvailableModels = async () => {
  if (!props.account) return

  loadingModels.value = true
  selectedModelId.value = '' // Reset selection before loading
  try {
    const models = await adminAPI.accounts.getTestAvailableModels(props.account.id)
    availableModels.value = props.account.platform === 'gemini' || props.account.platform === 'antigravity'
      ? sortTestModels(models)
      : models
  } catch (error) {
    console.error('Failed to load available models:', error)
    // Fallback to empty list
    availableModels.value = []
    selectedModelId.value = ''
  } finally {
    loadingModels.value = false
  }
}

const resetState = () => {
  status.value = 'idle'
  outputLines.value = []
  streamingContent.value = ''
  errorMessage.value = ''
  testMetadata.value = {}
  generatedImages.value = []
  previewImageUrl.value = ''
}

const handleClose = () => {
  abortStream()
  emit('close')
}

const abortStream = () => {
  if (abortController) {
    abortController.abort()
    abortController = null
  }
}

const addLine = (text: string, className: string = 'text-gray-300') => {
  outputLines.value.push({ text, class: className })
  scrollToBottom()
}

const scrollToBottom = async () => {
  await nextTick()
  if (terminalRef.value) {
    terminalRef.value.scrollTop = terminalRef.value.scrollHeight
  }
}

const startTest = async () => {
  if (!props.account || !selectedModelId.value) return

  resetState()
  status.value = 'connecting'
  addLine(t('admin.accounts.startingTestForAccount', { name: props.account.name }), 'text-blue-400')
  addLine(t('admin.accounts.testAccountTypeLabel', { type: props.account.type }), 'text-gray-400')
  addLine('', 'text-gray-300')

  abortStream()

  abortController = new AbortController()

  try {
    // Use the configured API base; EventSource does not support POST.
    const url = buildApiUrl(`/admin/accounts/${props.account.id}/test`)

    const requestBody: AccountTestStreamRequest = {
      model_id: selectedModelId.value,
      prompt: supportsImageTest.value ? testPrompt.value.trim() : '',
      mode: isOpenAIAccount.value ? testMode.value : 'default'
    }
    // "Use default" sends no effort at all; a model ID suffix wins over the
    // dropdown so the request always matches the level the UI shows.
    if (effectiveEffort.value.effort) {
      requestBody.reasoning_effort = effectiveEffort.value.effort
    }

    await consumeAccountTestStream({
      url,
      body: requestBody,
      signal: abortController.signal,
      onEvent: handleEvent
    })
  } catch (error: unknown) {
    if (error instanceof DOMException && error.name === 'AbortError') {
      status.value = 'idle'
      return
    }
    status.value = 'error'
    const msg = error instanceof Error ? error.message : 'Unknown error'
    errorMessage.value = msg
    addLine(`Error: ${msg}`, 'text-red-400')
  }
}

const applyTestMetadata = (event: AccountTestStreamEvent) => {
  testMetadata.value = mergeAccountTestMetadata(testMetadata.value, event)
}

const handleEvent = (event: AccountTestStreamEvent) => {
  switch (event.type) {
    case 'test_metadata':
      applyTestMetadata(event)
      break

    case 'test_start':
      applyTestMetadata(event)
      addLine(t('admin.accounts.connectedToApi'), 'text-green-400')
      if (event.model) {
        addLine(t('admin.accounts.usingModel', { model: event.model }), 'text-cyan-400')
      }
      addLine(
        supportsImageTest.value
            ? t('admin.accounts.sendingImageRequest')
            : t('admin.accounts.sendingTestMessage'),
        'text-gray-400'
      )
      addLine('', 'text-gray-300')
      addLine(t('admin.accounts.response'), 'text-yellow-400')
      break

    case 'content':
      if (event.text) {
        streamingContent.value += event.text
        scrollToBottom()
      }
      break

    case 'status':
      if (event.text) {
        addLine(event.text, 'text-cyan-300')
      }
      break

    case 'image':
      if (event.image_url) {
        generatedImages.value.push({
          url: event.image_url,
          mimeType: event.mime_type
        })
        addLine(t('admin.accounts.imageReceived', { count: generatedImages.value.length }), 'text-purple-300')
      }
      break

    case 'test_complete':
      // Move streaming content to output lines
      if (streamingContent.value) {
        addLine(streamingContent.value, 'text-green-300')
        streamingContent.value = ''
      }
      if (event.success) {
        status.value = 'success'
      } else {
        status.value = 'error'
        errorMessage.value = event.error || 'Test failed'
      }
      break

    case 'error':
      status.value = 'error'
      errorMessage.value = event.error || 'Unknown error'
      if (streamingContent.value) {
        addLine(streamingContent.value, 'text-green-300')
        streamingContent.value = ''
      }
      break
  }
}

const copyOutput = () => {
  const text = outputLines.value.map((l) => l.text).join('\n')
  copyToClipboard(text, t('admin.accounts.outputCopied'))
}
</script>

<style>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s ease;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>
