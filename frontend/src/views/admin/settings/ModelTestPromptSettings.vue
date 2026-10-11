<template>
  <div class="card" data-testid="model-test-prompt-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.settings.modelTestPrompt.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.settings.modelTestPrompt.description") }}
      </p>
    </div>

    <div class="space-y-5 p-6">
      <div
        v-if="loading"
        class="flex items-center gap-2 text-gray-500 dark:text-gray-400"
        data-testid="model-test-prompt-loading"
      >
        <div
          class="h-4 w-4 animate-spin rounded-full border-b-2 border-primary-600"
        ></div>
        {{ t("common.loading") }}
      </div>

      <template v-else>
        <div
          v-if="loadError"
          class="flex items-center justify-between gap-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 dark:border-red-900/60 dark:bg-red-950/30"
          data-testid="model-test-prompt-load-error"
        >
          <p class="text-sm text-red-700 dark:text-red-300">
            {{ t("admin.settings.modelTestPrompt.loadFailed") }}
          </p>
          <button
            type="button"
            class="btn btn-secondary btn-sm shrink-0"
            :disabled="saving"
            data-testid="model-test-prompt-retry"
            @click="loadSettings"
          >
            {{ t("common.refresh") }}
          </button>
        </div>

        <div>
          <label
            for="model-test-prompt"
            class="block text-sm font-medium text-gray-900 dark:text-white"
          >
            {{ t("admin.settings.modelTestPrompt.label") }}
          </label>
          <p class="mb-2 mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.modelTestPrompt.hint") }}
          </p>
          <textarea
            id="model-test-prompt"
            v-model="promptText"
            rows="4"
            class="input input-sm w-full"
            :class="tooLong && 'border-red-500 dark:border-red-500'"
            :placeholder="t('admin.settings.modelTestPrompt.placeholder')"
            :aria-invalid="tooLong"
            :disabled="controlsDisabled"
            data-testid="model-test-prompt-input"
          ></textarea>
          <p
            v-if="tooLong"
            class="mt-1 text-xs text-red-600 dark:text-red-400"
            role="alert"
            data-testid="model-test-prompt-error"
          >
            {{
              t("admin.settings.modelTestPrompt.tooLong", {
                max: MAX_CHARS,
                count: charCount,
              })
            }}
          </p>
          <p
            v-else-if="formReadable"
            class="mt-1 text-xs text-gray-400 dark:text-gray-500"
            data-testid="model-test-prompt-counter"
          >
            {{
              t("admin.settings.modelTestPrompt.counter", {
                count: charCount,
                max: MAX_CHARS,
              })
            }}
          </p>
        </div>

        <div
          class="rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 dark:border-dark-600 dark:bg-dark-700/50"
          data-testid="model-test-prompt-default"
        >
          <div class="text-xs font-medium text-gray-600 dark:text-gray-300">
            {{ t("admin.settings.modelTestPrompt.defaultLabel") }}
          </div>
          <p class="mt-1 whitespace-pre-wrap break-words text-xs text-gray-500 dark:text-gray-400">
            {{ builtinDefault || t("admin.settings.modelTestPrompt.defaultUnavailable") }}
          </p>
          <p class="mt-1 text-[11px] text-gray-400 dark:text-gray-500">
            {{ t("admin.settings.modelTestPrompt.defaultHint") }}
          </p>
        </div>

        <div
          class="flex flex-col gap-3 border-t border-gray-100 pt-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between"
        >
          <p
            class="text-xs text-gray-500 dark:text-gray-400"
            data-testid="model-test-prompt-affects-hint"
          >
            {{ t("admin.settings.modelTestPrompt.affectsHint") }}
          </p>
          <div class="flex shrink-0 items-center gap-2">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              :disabled="controlsDisabled || !storedPrompt"
              data-testid="model-test-prompt-reset"
              @click="resetToDefault"
            >
              {{ t("admin.settings.modelTestPrompt.reset") }}
            </button>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              :disabled="controlsDisabled || tooLong"
              data-testid="model-test-prompt-save"
              @click="saveSettings"
            >
              <svg
                v-if="saving"
                class="mr-1 h-4 w-4 animate-spin"
                fill="none"
                viewBox="0 0 24 24"
              >
                <circle
                  class="opacity-25"
                  cx="12"
                  cy="12"
                  r="10"
                  stroke="currentColor"
                  stroke-width="4"
                ></circle>
                <path
                  class="opacity-75"
                  fill="currentColor"
                  d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
                ></path>
              </svg>
              {{ saving ? t("common.saving") : t("admin.settings.modelTestPrompt.save") }}
            </button>
          </div>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api/admin";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";
import {
  MODEL_TEST_PROMPT_MAX_CHARS as MAX_CHARS,
  countModelTestPromptChars,
  type ModelTestPromptSettings,
} from "@/types/modelTestPrompt";

/**
 * Global prompt for ordinary text account tests.
 *
 * The stored value may be empty, which means "use the built-in default"; the
 * card therefore always shows the built-in default it would fall back to, and
 * the reset action stores an empty prompt instead of copying that text.
 */
const { t } = useI18n();
const appStore = useAppStore();

const loading = ref(true);
const saving = ref(false);
const loaded = ref(false);
const loadError = ref(false);
const promptText = ref("");
const storedPrompt = ref("");
const builtinDefault = ref("");

const controlsDisabled = computed(
  () => saving.value || loadError.value || !loaded.value,
);

/** Validation copy is only meaningful for settings that were actually read. */
const formReadable = computed(() => loaded.value && !loadError.value);

const charCount = computed(() => countModelTestPromptChars(promptText.value));
const tooLong = computed(() => charCount.value > MAX_CHARS);

function applySettings(value: ModelTestPromptSettings | null | undefined): void {
  storedPrompt.value = typeof value?.prompt === "string" ? value.prompt : "";
  promptText.value = storedPrompt.value;
  builtinDefault.value =
    typeof value?.default_prompt === "string" ? value.default_prompt : "";
}

async function loadSettings(): Promise<void> {
  loading.value = true;
  loaded.value = false;
  loadError.value = false;
  try {
    applySettings(await adminAPI.settings.getModelTestPromptSettings());
    loaded.value = true;
  } catch (error: unknown) {
    // Keep the failed panel read-only and offer a retry; never save over
    // settings that could not be read.
    loadError.value = true;
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.modelTestPrompt.loadFailed"),
      ),
    );
  } finally {
    loading.value = false;
  }
}

async function persist(prompt: string, successMessage: string): Promise<boolean> {
  saving.value = true;
  const previousPrompt = promptText.value;
  promptText.value = prompt;
  try {
    // On failure the edited form stays untouched so the administrator can fix
    // and resubmit instead of retyping everything.
    const response = await adminAPI.settings.updateModelTestPromptSettings(prompt);
    if (response) {
      applySettings(response);
    } else {
      storedPrompt.value = prompt;
    }
    appStore.showSuccess(successMessage);
    return true;
  } catch (error: unknown) {
    promptText.value = previousPrompt;
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.modelTestPrompt.saveFailed"),
      ),
    );
    return false;
  } finally {
    saving.value = false;
  }
}

async function saveSettings(): Promise<void> {
  if (saving.value || !loaded.value || loadError.value || tooLong.value) return;
  // Whitespace-only input means "no custom prompt", i.e. the built-in default.
  await persist(
    promptText.value.trim(),
    t("admin.settings.modelTestPrompt.saved"),
  );
}

async function resetToDefault(): Promise<void> {
  if (saving.value || !loaded.value || loadError.value) return;
  await persist("", t("admin.settings.modelTestPrompt.resetSaved"));
}

onMounted(loadSettings);
</script>
