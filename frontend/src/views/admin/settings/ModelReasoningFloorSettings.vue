<template>
  <div class="card" data-testid="model-reasoning-floor-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.settings.modelReasoningFloor.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.settings.modelReasoningFloor.description") }}
      </p>
    </div>

    <div class="space-y-5 p-6">
      <div
        v-if="loading"
        class="flex items-center gap-2 text-gray-500 dark:text-gray-400"
        data-testid="model-reasoning-floor-loading"
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
          data-testid="model-reasoning-floor-load-error"
        >
          <p class="text-sm text-red-700 dark:text-red-300">
            {{ t("admin.settings.modelReasoningFloor.loadFailed") }}
          </p>
          <button
            type="button"
            class="btn btn-secondary btn-sm shrink-0"
            :disabled="saving"
            data-testid="model-reasoning-floor-retry"
            @click="loadSettings"
          >
            {{ t("common.refresh") }}
          </button>
        </div>

        <div class="flex items-center justify-between gap-4">
          <div>
            <label class="font-medium text-gray-900 dark:text-white">
              {{ t("admin.settings.modelReasoningFloor.enabled") }}
            </label>
            <p class="text-sm text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.modelReasoningFloor.enabledHint") }}
            </p>
          </div>
          <Toggle
            v-model="form.enabled"
            :disabled="controlsDisabled"
            data-testid="model-reasoning-floor-enabled"
          />
        </div>

        <p
          class="rounded-lg border border-blue-100 bg-blue-50 px-3 py-2 text-xs text-blue-700 dark:border-blue-900/60 dark:bg-blue-950/30 dark:text-blue-300"
          data-testid="model-reasoning-floor-priority-hint"
        >
          {{ t("admin.settings.modelReasoningFloor.priorityHint") }}
        </p>

        <div class="border-t border-gray-100 pt-4 dark:border-dark-700">
          <div
            v-if="form.rules.length === 0"
            class="rounded-lg border border-dashed border-gray-200 p-6 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
            data-testid="model-reasoning-floor-empty"
          >
            {{ t("admin.settings.modelReasoningFloor.empty") }}
          </div>

          <div
            v-for="(rule, index) in form.rules"
            :key="`model-reasoning-floor-${index}`"
            class="mb-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600"
            :data-testid="`model-reasoning-floor-rule-${index}`"
          >
            <div class="mb-3 flex items-center justify-between gap-3">
              <span class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.settings.modelReasoningFloor.ruleHeader", { index: index + 1 }) }}
              </span>
              <button
                type="button"
                class="rounded p-1 text-red-400 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
                :aria-label="t('admin.settings.modelReasoningFloor.removeRule')"
                :title="t('admin.settings.modelReasoningFloor.removeRule')"
                :data-testid="`model-reasoning-floor-remove-${index}`"
                :disabled="controlsDisabled"
                @click="removeRule(index)"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>

            <div class="grid gap-4 md:grid-cols-[minmax(0,1fr)_minmax(12rem,0.45fr)]">
              <div>
                <label
                  :for="`model-reasoning-floor-model-${index}`"
                  class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400"
                >
                  {{ t("admin.settings.modelReasoningFloor.model") }}
                </label>
                <input
                  :id="`model-reasoning-floor-model-${index}`"
                  v-model="rule.model"
                  type="text"
                  class="input input-sm w-full font-mono"
                  :class="validationErrors[index] && 'border-red-500 dark:border-red-500'"
                  :placeholder="t('admin.settings.modelReasoningFloor.modelPlaceholder')"
                  :aria-invalid="Boolean(validationErrors[index])"
                  :data-testid="`model-reasoning-floor-model-${index}`"
                  :disabled="controlsDisabled"
                />
                <p
                  v-if="validationErrors[index]"
                  class="mt-1 text-xs text-red-600 dark:text-red-400"
                  role="alert"
                  :data-testid="`model-reasoning-floor-error-${index}`"
                >
                  {{ validationMessage(validationErrors[index]) }}
                </p>
              </div>

              <div>
                <label
                  :for="`model-reasoning-floor-effort-${index}`"
                  class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400"
                >
                  {{ t("admin.settings.modelReasoningFloor.minEffort") }}
                </label>
                <Select
                  :id="`model-reasoning-floor-effort-${index}`"
                  :model-value="rule.min_effort"
                  :options="effortOptions"
                  :aria-label="t('admin.settings.modelReasoningFloor.minEffort')"
                  :data-testid="`model-reasoning-floor-effort-${index}`"
                  :searchable="false"
                  :disabled="controlsDisabled"
                  @update:model-value="updateMinEffort(index, $event)"
                />
              </div>
            </div>
          </div>

          <button
            type="button"
            class="btn btn-secondary btn-sm inline-flex items-center gap-1"
            :disabled="controlsDisabled || form.rules.length >= MAX_RULES"
            data-testid="model-reasoning-floor-add"
            @click="addRule"
          >
            <Icon name="plus" size="sm" />
            {{ t("admin.settings.modelReasoningFloor.addRule") }}
          </button>
          <p class="mt-2 text-xs text-gray-400 dark:text-gray-500">
            {{ t("admin.settings.modelReasoningFloor.rulesHint", { max: MAX_RULES }) }}
          </p>
        </div>

        <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="controlsDisabled"
            data-testid="model-reasoning-floor-save"
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
                d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l-2.647z"
              ></path>
            </svg>
            {{ saving ? t("common.saving") : t("admin.settings.modelReasoningFloor.save") }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api/admin";
import Icon from "@/components/icons/Icon.vue";
import Select from "@/components/common/Select.vue";
import Toggle from "@/components/common/Toggle.vue";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";
import {
  MODEL_REASONING_FLOOR_EFFORTS,
  type ModelReasoningFloorEffort,
  type ModelReasoningFloorRule,
  type ModelReasoningFloorSettings,
} from "@/types/modelReasoningFloor";

const MAX_RULES = 64;

type RuleValidationCode = "required" | "invalid" | "tooLong" | "duplicate";

const { t } = useI18n();
const appStore = useAppStore();
const loading = ref(true);
const saving = ref(false);
const loaded = ref(false);
const loadError = ref(false);
const form = reactive<ModelReasoningFloorSettings>({
  enabled: false,
  rules: [],
});

const controlsDisabled = computed(
  () => saving.value || loadError.value || !loaded.value,
);

const effortOptions = computed(() =>
  MODEL_REASONING_FLOOR_EFFORTS.map((value) => ({
    value,
    label: t(`admin.settings.modelReasoningFloor.efforts.${value}`),
  })),
);

const validationErrors = computed<Record<number, RuleValidationCode>>(() => {
  const errors: Record<number, RuleValidationCode> = {};
  const counts = new Map<string, number>();

  for (const rule of form.rules) {
    const model = rule.model;
    if (model && model.trim() === model) {
      counts.set(model, (counts.get(model) ?? 0) + 1);
    }
  }

  form.rules.forEach((rule, index) => {
    const model = rule.model;
    if (!model) {
      errors[index] = "required";
    } else if (model.length > 128) {
      errors[index] = "tooLong";
    } else if (
      model.trim() !== model ||
      /\s/.test(model) ||
      model.includes("*") ||
      model.includes("?")
    ) {
      errors[index] = "invalid";
    } else if ((counts.get(model) ?? 0) > 1) {
      errors[index] = "duplicate";
    }
  });

  return errors;
});

function isEffort(value: unknown): value is ModelReasoningFloorEffort {
  return (
    typeof value === "string" &&
    (MODEL_REASONING_FLOOR_EFFORTS as readonly string[]).includes(value)
  );
}

function normalizeRule(rule: ModelReasoningFloorRule): ModelReasoningFloorRule {
  return {
    model: typeof rule.model === "string" ? rule.model : "",
    min_effort: isEffort(rule.min_effort) ? rule.min_effort : "minimal",
  };
}

function applySettings(value: ModelReasoningFloorSettings | null | undefined): void {
  form.enabled = Boolean(value?.enabled);
  form.rules = Array.isArray(value?.rules)
    ? value.rules.slice(0, MAX_RULES).map(normalizeRule)
    : [];
}

function validationMessage(code: RuleValidationCode): string {
  return t(`admin.settings.modelReasoningFloor.validation.${code}`);
}

function updateMinEffort(
  index: number,
  value: string | number | boolean | null,
): void {
  const rule = form.rules[index];
  if (rule && isEffort(value)) {
    rule.min_effort = value;
  }
}

function addRule(): void {
  if (form.rules.length >= MAX_RULES) return;
  form.rules.push({ model: "", min_effort: "minimal" });
}

function removeRule(index: number): void {
  form.rules.splice(index, 1);
}

async function loadSettings(): Promise<void> {
  loading.value = true;
  loaded.value = false;
  loadError.value = false;
  try {
    applySettings(await adminAPI.settings.getModelReasoningFloorSettings());
    loaded.value = true;
  } catch (error: unknown) {
    loadError.value = true;
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.modelReasoningFloor.loadFailed"),
      ),
    );
  } finally {
    loading.value = false;
  }
}

async function saveSettings(): Promise<void> {
  if (saving.value || !loaded.value || loadError.value) return;

  if (Object.keys(validationErrors.value).length > 0) {
    appStore.showError(t("admin.settings.modelReasoningFloor.validationFailed"));
    return;
  }

  saving.value = true;
  const payload: ModelReasoningFloorSettings = {
    enabled: form.enabled,
    rules: form.rules.map((rule) => ({
      model: rule.model,
      min_effort: rule.min_effort,
    })),
  };

  try {
    const response = await adminAPI.settings.updateModelReasoningFloorSettings(
      payload,
    );
    if (response) {
      applySettings(response);
    }
    appStore.showSuccess(t("admin.settings.modelReasoningFloor.saved"));
  } catch (error: unknown) {
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.modelReasoningFloor.saveFailed"),
      ),
    );
  } finally {
    saving.value = false;
  }
}

onMounted(loadSettings);
</script>
