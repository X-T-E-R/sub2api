<template>
  <div
    class="rounded-lg border border-gray-200 p-4 dark:border-dark-600"
    data-testid="antigravity-thought-policy-override"
  >
    <div class="flex items-start justify-between gap-4">
      <p class="min-w-0 text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
        {{ t("title") }}
      </p>
      <button
        v-if="fetchableGlobal && !globalsResolved && !loadingGlobal"
        type="button"
        class="btn btn-secondary btn-sm shrink-0"
        data-testid="antigravity-thought-policy-override-global-retry"
        @click="loadGlobal"
      >
        {{ t("retry") }}
      </button>
    </div>

    <p
      v-if="loadingGlobal"
      class="mt-3 flex items-center gap-2 text-[11px] text-gray-500 dark:text-gray-400"
      data-testid="antigravity-thought-policy-override-global-loading"
    >
      <span class="h-3 w-3 animate-spin rounded-full border-b-2 border-primary-600"></span>
      {{ t("loadingGlobal") }}
    </p>
    <p
      v-else-if="!globalsResolved"
      class="mt-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-[11px] leading-relaxed text-amber-700 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-300"
      data-testid="antigravity-thought-policy-override-global-unknown"
    >
      {{ t("globalUnknown") }}
    </p>

    <div class="mt-3 space-y-3">
      <div v-for="row in rows" :key="row.key">
        <div class="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between sm:gap-3">
          <div class="min-w-0">
            <p class="text-xs font-medium text-gray-700 dark:text-gray-300">
              {{ t(row.labelKey) }}
            </p>
            <p class="mt-0.5 text-[11px] leading-relaxed text-gray-500 dark:text-gray-400">
              {{ t(row.hintKey) }}
            </p>
          </div>
          <div
            class="flex shrink-0 self-start rounded-md border border-gray-200 p-0.5 dark:border-dark-600"
            role="radiogroup"
            :aria-label="t(row.labelKey)"
            :data-testid="`antigravity-thought-policy-override-${row.key}`"
          >
            <button
              v-for="choice in choices"
              :key="choice"
              type="button"
              role="radio"
              :aria-checked="overrideFor(row.key) === choice"
              :disabled="disabled"
              class="rounded px-2 py-1 text-[11px] font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-60"
              :class="
                overrideFor(row.key) === choice
                  ? 'bg-primary-600 text-white'
                  : 'text-gray-600 hover:bg-gray-100 dark:text-gray-300 dark:hover:bg-dark-700'
              "
              :data-testid="`antigravity-thought-policy-override-${row.key}-${choice}`"
              @click="choose(row.key, choice)"
            >
              {{ t(`choices.${choice}`) }}
            </button>
          </div>
        </div>
        <p
          class="mt-1 text-[11px]"
          :data-testid="`antigravity-thought-policy-override-${row.key}-effective`"
        >
          <span class="text-gray-400 dark:text-gray-500">{{ t("effectiveLabel") }}</span>
          <span
            class="ml-1 font-medium"
            :class="effectiveLabelClass(row.key)"
          >
            {{ effectiveText(row.key) }}
          </span>
        </p>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { getAntigravityThoughtPolicySettings } from "@/api/admin/antigravityThoughtPolicy";
import { ANTIGRAVITY_THOUGHT_POLICY_MESSAGES } from "./antigravityThoughtPolicyMessages";
import {
  normalizeAntigravityThoughtPolicySettings,
  readAntigravityThoughtPolicyOverride,
  resolveAntigravityThoughtPolicy,
  setAntigravityThoughtPolicyOverride,
  type AntigravityThoughtPolicyKey,
  type AntigravityThoughtPolicyOverride,
  type AntigravityThoughtPolicySettings,
} from "@/types/antigravityThoughtPolicy";

const { t } = useI18n({ messages: ANTIGRAVITY_THOUGHT_POLICY_MESSAGES });

const props = withDefaults(
  defineProps<{
    /** Account Extra map; other keys are preserved untouched. */
    modelValue?: Record<string, unknown> | null;
    /**
     * Global pair supplied by the parent. Omitted (undefined) means "load it here";
     * an explicit null means "known to be unavailable", which skips loading and shows
     * the inherited value as unknown instead of assuming off.
     */
    globalPolicy?: AntigravityThoughtPolicySettings | null;
    disabled?: boolean;
  }>(),
  { modelValue: null, globalPolicy: undefined, disabled: false },
);

const emit = defineEmits<{
  (e: "update:modelValue", value: Record<string, unknown>): void;
}>();

const choices: readonly AntigravityThoughtPolicyOverride[] = ["inherit", "on", "off"];

const rows: ReadonlyArray<{
  key: AntigravityThoughtPolicyKey;
  labelKey: string;
  hintKey: string;
}> = [
  {
    key: "suppress_thought_only_stop",
    labelKey: "suppressLabel",
    hintKey: "suppressHint",
  },
  {
    key: "continue_thought_only",
    labelKey: "continueLabel",
    hintKey: "continueHint",
  },
];

const loadingGlobal = ref(false);
const globalSettings = ref<AntigravityThoughtPolicySettings | null>(props.globalPolicy ?? null);
const globalsResolved = computed(() => globalSettings.value !== null);
const fetchableGlobal = computed(() => props.globalPolicy === undefined);

function overrideFor(key: AntigravityThoughtPolicyKey): AntigravityThoughtPolicyOverride {
  return readAntigravityThoughtPolicyOverride(props.modelValue, key);
}

const effective = computed(() => {
  const global = globalSettings.value;
  if (global === null) {
    return null;
  }
  return resolveAntigravityThoughtPolicy(props.modelValue, global);
});

function effectiveValue(key: AntigravityThoughtPolicyKey): boolean | null {
  return effective.value === null ? null : effective.value[key];
}

function effectiveText(key: AntigravityThoughtPolicyKey): string {
  const choice = overrideFor(key);
  if (choice === "on") return t("effective.onOverride");
  if (choice === "off") return t("effective.offOverride");
  const value = effectiveValue(key);
  if (value === null) return t("effective.unknown");
  return value ? t("effective.inheritedOn") : t("effective.inheritedOff");
}

function effectiveLabelClass(key: AntigravityThoughtPolicyKey): string {
  return effectiveValue(key)
    ? "text-primary-600 dark:text-primary-400"
    : "text-gray-500 dark:text-gray-400";
}

function choose(key: AntigravityThoughtPolicyKey, choice: AntigravityThoughtPolicyOverride): void {
  if (props.disabled) return;
  emit(
    "update:modelValue",
    setAntigravityThoughtPolicyOverride(props.modelValue, key, choice),
  );
}

async function loadGlobal(): Promise<void> {
  if (!fetchableGlobal.value || loadingGlobal.value) return;
  loadingGlobal.value = true;
  try {
    globalSettings.value = normalizeAntigravityThoughtPolicySettings(
      await getAntigravityThoughtPolicySettings(),
    );
  } catch {
    // Unknown inherited values must stay visibly unknown; never show a guessed "off".
    globalSettings.value = null;
  } finally {
    loadingGlobal.value = false;
  }
}

watch(
  () => props.globalPolicy,
  (value) => {
    if (value !== undefined) {
      globalSettings.value = value;
    }
  },
);

onMounted(() => {
  if (fetchableGlobal.value) {
    void loadGlobal();
  }
});
</script>
