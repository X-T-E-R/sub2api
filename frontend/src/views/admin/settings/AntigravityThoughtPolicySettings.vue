<template>
  <div class="card" data-testid="antigravity-thought-policy-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.settings.antigravityThoughtPolicy.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.settings.antigravityThoughtPolicy.description") }}
      </p>
    </div>

    <div class="space-y-5 p-6">
      <div
        v-if="loading"
        class="flex items-center gap-2 text-gray-500 dark:text-gray-400"
        data-testid="antigravity-thought-policy-loading"
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
          data-testid="antigravity-thought-policy-load-error"
        >
          <p class="text-sm text-red-700 dark:text-red-300">
            {{ t("admin.settings.antigravityThoughtPolicy.loadFailed") }}
          </p>
          <button
            type="button"
            class="btn btn-secondary btn-sm shrink-0"
            :disabled="saving"
            data-testid="antigravity-thought-policy-retry"
            @click="loadSettings"
          >
            {{ t("common.refresh") }}
          </button>
        </div>

        <div
          class="divide-y divide-gray-100 overflow-hidden rounded-lg border border-gray-100 dark:divide-dark-700 dark:border-dark-700"
        >
          <div class="flex flex-col gap-3 px-4 py-3 sm:flex-row sm:items-start sm:justify-between sm:gap-6">
            <div class="min-w-0">
              <p class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.settings.antigravityThoughtPolicy.suppressLabel") }}
              </p>
              <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                {{ t("admin.settings.antigravityThoughtPolicy.suppressHint") }}
              </p>
            </div>
            <div class="flex shrink-0 items-center gap-2 self-start sm:pt-0.5">
              <span
                class="text-xs font-medium"
                :class="
                  policy.suppress_thought_only_stop
                    ? 'text-primary-600 dark:text-primary-400'
                    : 'text-gray-400 dark:text-gray-500'
                "
                aria-hidden="true"
              >
                {{
                  policy.suppress_thought_only_stop
                    ? t("admin.settings.antigravityThoughtPolicy.stateOn")
                    : t("admin.settings.antigravityThoughtPolicy.stateOff")
                }}
              </span>
              <Toggle
                id="antigravity-thought-policy-suppress"
                :model-value="policy.suppress_thought_only_stop"
                :disabled="controlsDisabled"
                :aria-label="t('admin.settings.antigravityThoughtPolicy.suppressLabel')"
                data-testid="antigravity-thought-policy-suppress"
                @update:model-value="setSwitch('suppress_thought_only_stop', $event)"
              />
            </div>
          </div>

          <div class="flex flex-col gap-3 px-4 py-3 sm:flex-row sm:items-start sm:justify-between sm:gap-6">
            <div class="min-w-0">
              <p class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.settings.antigravityThoughtPolicy.continueLabel") }}
              </p>
              <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">
                {{ t("admin.settings.antigravityThoughtPolicy.continueHint") }}
              </p>
            </div>
            <div class="flex shrink-0 items-center gap-2 self-start sm:pt-0.5">
              <span
                class="text-xs font-medium"
                :class="
                  policy.continue_thought_only
                    ? 'text-primary-600 dark:text-primary-400'
                    : 'text-gray-400 dark:text-gray-500'
                "
                aria-hidden="true"
              >
                {{
                  policy.continue_thought_only
                    ? t("admin.settings.antigravityThoughtPolicy.stateOn")
                    : t("admin.settings.antigravityThoughtPolicy.stateOff")
                }}
              </span>
              <Toggle
                id="antigravity-thought-policy-continue"
                :model-value="policy.continue_thought_only"
                :disabled="controlsDisabled"
                :aria-label="t('admin.settings.antigravityThoughtPolicy.continueLabel')"
                data-testid="antigravity-thought-policy-continue"
                @update:model-value="setSwitch('continue_thought_only', $event)"
              />
            </div>
          </div>
        </div>

        <p
          class="text-xs leading-relaxed text-gray-500 dark:text-gray-400"
          data-testid="antigravity-thought-policy-hint"
        >
          {{ t("admin.settings.antigravityThoughtPolicy.independenceHint") }}
        </p>

        <div
          class="flex flex-col gap-3 border-t border-gray-100 pt-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between"
        >
          <p
            class="text-xs text-gray-500 dark:text-gray-400"
            data-testid="antigravity-thought-policy-affects-hint"
          >
            {{ t("admin.settings.antigravityThoughtPolicy.affectsHint") }}
          </p>
          <button
            type="button"
            class="btn btn-primary btn-sm shrink-0"
            :disabled="controlsDisabled"
            data-testid="antigravity-thought-policy-save"
            @click="saveSettings"
          >
            <svg v-if="saving" class="mr-1 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
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
            {{
              saving
                ? t("common.saving")
                : t("admin.settings.antigravityThoughtPolicy.save")
            }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import {
  getAntigravityThoughtPolicySettings,
  updateAntigravityThoughtPolicySettings,
} from "@/api/admin/antigravityThoughtPolicy";
import Toggle from "@/components/common/Toggle.vue";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";
import {
  ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS,
  normalizeAntigravityThoughtPolicySettings,
  type AntigravityThoughtPolicyKey,
  type AntigravityThoughtPolicySettings,
} from "@/types/antigravityThoughtPolicy";

const { t } = useI18n();
const appStore = useAppStore();

const loading = ref(true);
const saving = ref(false);
const loaded = ref(false);
const loadError = ref(false);
const policy = ref<AntigravityThoughtPolicySettings>({
  ...ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS,
});

/**
 * Settings that could not be read must not be overwritten: without a successful load
 * both switches stay read-only until the administrator retries.
 */
const controlsDisabled = computed(
  () => saving.value || loadError.value || !loaded.value,
);

function applySettings(value: unknown): void {
  policy.value = normalizeAntigravityThoughtPolicySettings(value);
}

function setSwitch(key: AntigravityThoughtPolicyKey, value: boolean): void {
  if (controlsDisabled.value) return;
  policy.value = { ...policy.value, [key]: value };
}

async function loadSettings(): Promise<void> {
  loading.value = true;
  loaded.value = false;
  loadError.value = false;
  try {
    applySettings(await getAntigravityThoughtPolicySettings());
    loaded.value = true;
  } catch (error: unknown) {
    loadError.value = true;
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.antigravityThoughtPolicy.loadFailed"),
      ),
    );
  } finally {
    loading.value = false;
  }
}

async function saveSettings(): Promise<void> {
  if (controlsDisabled.value) return;
  saving.value = true;
  try {
    // A failed save keeps the edited pair on screen so it can be retried as-is.
    const response = await updateAntigravityThoughtPolicySettings({
      ...policy.value,
    });
    applySettings(response);
    appStore.showSuccess(t("admin.settings.antigravityThoughtPolicy.saved"));
  } catch (error: unknown) {
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.antigravityThoughtPolicy.saveFailed"),
      ),
    );
  } finally {
    saving.value = false;
  }
}

onMounted(loadSettings);
</script>
