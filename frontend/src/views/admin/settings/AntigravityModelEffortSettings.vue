<template>
  <div class="card" data-testid="antigravity-model-effort-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.settings.antigravityModelEffort.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.settings.antigravityModelEffort.description") }}
      </p>
    </div>

    <div class="space-y-5 p-6">
      <div
        v-if="loading"
        class="flex items-center gap-2 text-gray-500 dark:text-gray-400"
        data-testid="antigravity-model-effort-loading"
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
          data-testid="antigravity-model-effort-load-error"
        >
          <p class="text-sm text-red-700 dark:text-red-300">
            {{ t("admin.settings.antigravityModelEffort.loadFailed") }}
          </p>
          <button
            type="button"
            class="btn btn-secondary btn-sm shrink-0"
            :disabled="saving"
            data-testid="antigravity-model-effort-retry"
            @click="loadSettings"
          >
            {{ t("common.refresh") }}
          </button>
        </div>

        <div>
          <label
            for="antigravity-model-effort-levels"
            class="block text-sm font-medium text-gray-900 dark:text-white"
          >
            {{ t("admin.settings.antigravityModelEffort.levels") }}
          </label>
          <p class="mb-2 mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.antigravityModelEffort.levelsHint") }}
          </p>
          <textarea
            id="antigravity-model-effort-levels"
            v-model="namesText"
            rows="5"
            class="input input-sm w-full font-mono"
            :class="namesError && 'border-red-500 dark:border-red-500'"
            :placeholder="t('admin.settings.antigravityModelEffort.levelsPlaceholder')"
            :aria-invalid="Boolean(namesError)"
            :disabled="controlsDisabled"
            data-testid="antigravity-model-effort-levels"
          ></textarea>
          <p
            v-if="namesError"
            class="mt-1 text-xs text-red-600 dark:text-red-400"
            role="alert"
            data-testid="antigravity-model-effort-levels-error"
          >
            {{ namesError }}
          </p>
          <p
            v-else-if="formReadable"
            class="mt-1 text-xs text-gray-400 dark:text-gray-500"
            data-testid="antigravity-model-effort-levels-summary"
          >
            {{
              t("admin.settings.antigravityModelEffort.levelsSummary", {
                count: parsedLevels.length,
                max: MAX_LEVELS,
              })
            }}
          </p>
        </div>

        <div>
          <label
            for="antigravity-model-effort-default"
            class="block text-sm font-medium text-gray-900 dark:text-white"
          >
            {{ t("admin.settings.antigravityModelEffort.defaultEffort") }}
          </label>
          <p class="mb-2 mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.antigravityModelEffort.defaultEffortHint") }}
          </p>
          <Select
            id="antigravity-model-effort-default"
            :model-value="defaultEffort"
            :options="levelOptions"
            :placeholder="t('admin.settings.antigravityModelEffort.defaultPlaceholder')"
            :aria-label="t('admin.settings.antigravityModelEffort.defaultEffort')"
            :searchable="false"
            :error="Boolean(defaultError)"
            :disabled="controlsDisabled"
            data-testid="antigravity-model-effort-default"
            @update:model-value="updateDefaultEffort"
          />
          <p
            v-if="defaultError"
            class="mt-1 text-xs text-red-600 dark:text-red-400"
            role="alert"
            data-testid="antigravity-model-effort-default-error"
          >
            {{ defaultError }}
          </p>
        </div>

        <div
          class="flex flex-col gap-3 border-t border-gray-100 pt-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between"
        >
          <p
            class="text-xs text-gray-500 dark:text-gray-400"
            data-testid="antigravity-model-effort-affects-hint"
          >
            {{ t("admin.settings.antigravityModelEffort.affectsHint") }}
          </p>
          <button
            type="button"
            class="btn btn-primary btn-sm shrink-0"
            :disabled="controlsDisabled"
            data-testid="antigravity-model-effort-save"
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
            {{ saving ? t("common.saving") : t("admin.settings.antigravityModelEffort.save") }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api/admin";
import Select from "@/components/common/Select.vue";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";
import {
  ANTIGRAVITY_EFFORT_MAX_LEVELS as MAX_LEVELS,
  ANTIGRAVITY_EFFORT_MAX_NAME_LENGTH as MAX_NAME_LENGTH,
  ANTIGRAVITY_EFFORT_NAME_PATTERN,
  type AntigravityModelEffortSettings,
} from "@/types/antigravityModelEffort";

type NameIssueKind = "required" | "invalid" | "duplicate" | "tooMany";
type DefaultIssueKind = "defaultRequired" | "defaultNotInLevels";

const { t } = useI18n();
const appStore = useAppStore();

const loading = ref(true);
const saving = ref(false);
const loaded = ref(false);
const loadError = ref(false);
const namesText = ref("");
const defaultEffort = ref("");

const controlsDisabled = computed(
  () => saving.value || loadError.value || !loaded.value,
);

/**
 * Validation copy is only meaningful for settings that were actually read:
 * an unsaved panel must not blame the administrator for its own empty state.
 */
const formReadable = computed(() => loaded.value && !loadError.value);

/** Tokens as typed, split on newlines and commas. */
const rawTokens = computed(() =>
  namesText.value
    .split(/[\n,]/)
    .map((token) => token.trim())
    .filter((token) => token.length > 0),
);

const nameIssues = computed(() => {
  const invalid: string[] = [];
  const duplicates: string[] = [];
  const levels: string[] = [];

  for (const token of rawTokens.value) {
    const normalized = token.toLowerCase();
    if (
      normalized.length > MAX_NAME_LENGTH ||
      !ANTIGRAVITY_EFFORT_NAME_PATTERN.test(normalized)
    ) {
      if (!invalid.includes(normalized)) invalid.push(normalized);
      continue;
    }
    if (levels.includes(normalized)) {
      if (!duplicates.includes(normalized)) duplicates.push(normalized);
      continue;
    }
    levels.push(normalized);
  }

  return { invalid, duplicates, levels };
});

const parsedLevels = computed(() => nameIssues.value.levels);

const namesIssueKind = computed<NameIssueKind | null>(() => {
  if (rawTokens.value.length === 0) return "required";
  if (nameIssues.value.invalid.length > 0) return "invalid";
  if (nameIssues.value.duplicates.length > 0) return "duplicate";
  if (parsedLevels.value.length > MAX_LEVELS) return "tooMany";
  return null;
});

/** True whenever the stored default can no longer be submitted as-is. */
const defaultUnavailable = computed(
  () =>
    defaultEffort.value === "" ||
    !parsedLevels.value.includes(defaultEffort.value),
);

const defaultIssueKind = computed<DefaultIssueKind | null>(() => {
  if (defaultEffort.value === "") return "defaultRequired";
  // A names problem already blocks saving; do not stack a second message on it.
  if (namesIssueKind.value !== null) return null;
  return defaultUnavailable.value ? "defaultNotInLevels" : null;
});

const levelOptions = computed(() =>
  parsedLevels.value.map((value) => ({ value, label: value })),
);

function formatOffenders(names: string[]): string {
  const shown = names
    .slice(0, 5)
    .map((name) => (name.length > 24 ? `${name.slice(0, 24)}…` : name));
  return names.length > shown.length
    ? `${shown.join(", ")} +${names.length - shown.length}`
    : shown.join(", ");
}

const namesError = computed(() => {
  if (!formReadable.value) return "";
  const kind = namesIssueKind.value;
  if (kind === null) return "";
  if (kind === "invalid") {
    return t("admin.settings.antigravityModelEffort.validation.invalid", {
      names: formatOffenders(nameIssues.value.invalid),
    });
  }
  if (kind === "duplicate") {
    return t("admin.settings.antigravityModelEffort.validation.duplicate", {
      names: formatOffenders(nameIssues.value.duplicates),
    });
  }
  if (kind === "tooMany") {
    return t("admin.settings.antigravityModelEffort.validation.tooMany", {
      max: MAX_LEVELS,
    });
  }
  return t("admin.settings.antigravityModelEffort.validation.required");
});

const defaultError = computed(() => {
  if (!formReadable.value) return "";
  const kind = defaultIssueKind.value;
  return kind === null
    ? ""
    : t(`admin.settings.antigravityModelEffort.validation.${kind}`);
});

function applySettings(value: AntigravityModelEffortSettings | null | undefined): void {
  const levels = Array.isArray(value?.levels)
    ? value.levels
        .filter((level): level is string => typeof level === "string")
        .slice(0, MAX_LEVELS)
    : [];
  namesText.value = levels.join("\n");
  defaultEffort.value =
    typeof value?.default_effort === "string" ? value.default_effort : "";
}

function updateDefaultEffort(value: string | number | boolean | null): void {
  if (typeof value === "string") {
    defaultEffort.value = value;
  }
}

async function loadSettings(): Promise<void> {
  loading.value = true;
  loaded.value = false;
  loadError.value = false;
  try {
    applySettings(await adminAPI.settings.getAntigravityModelEffortSettings());
    loaded.value = true;
  } catch (error: unknown) {
    // Keep the failed panel read-only and offer a retry; never allow saving
    // over settings that could not be read.
    loadError.value = true;
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.antigravityModelEffort.loadFailed"),
      ),
    );
  } finally {
    loading.value = false;
  }
}

async function saveSettings(): Promise<void> {
  if (saving.value || !loaded.value || loadError.value) return;

  if (namesIssueKind.value !== null || defaultUnavailable.value) {
    appStore.showError(
      t("admin.settings.antigravityModelEffort.validationFailed"),
    );
    return;
  }

  saving.value = true;
  const payload: AntigravityModelEffortSettings = {
    levels: [...parsedLevels.value],
    default_effort: defaultEffort.value,
  };

  try {
    // On failure the edited form stays untouched so the administrator can fix
    // and resubmit instead of retyping everything.
    const response =
      await adminAPI.settings.updateAntigravityModelEffortSettings(payload);
    if (response) {
      applySettings(response);
    }
    appStore.showSuccess(
      t("admin.settings.antigravityModelEffort.saved"),
    );
  } catch (error: unknown) {
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.antigravityModelEffort.saveFailed"),
      ),
    );
  } finally {
    saving.value = false;
  }
}

onMounted(loadSettings);
</script>
