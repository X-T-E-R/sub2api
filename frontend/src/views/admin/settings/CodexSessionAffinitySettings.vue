<template>
  <div class="card" data-testid="codex-session-affinity-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.settings.codexSessionAffinity.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.settings.codexSessionAffinity.description") }}
      </p>
    </div>

    <div class="space-y-4 p-6">
      <div
        v-if="loading"
        class="flex items-center gap-2 text-gray-500 dark:text-gray-400"
        data-testid="codex-session-affinity-loading"
      >
        <div class="h-4 w-4 animate-spin rounded-full border-b-2 border-primary-600" />
        {{ t("common.loading") }}
      </div>

      <template v-else>
        <div
          v-if="loadError"
          class="flex items-center justify-between gap-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 dark:border-red-900/60 dark:bg-red-950/30"
          data-testid="codex-session-affinity-load-error"
        >
          <p class="text-sm text-red-700 dark:text-red-300">
            {{ t("admin.settings.codexSessionAffinity.loadFailed") }}
          </p>
          <button
            type="button"
            class="btn btn-secondary btn-sm shrink-0"
            :disabled="saving"
            data-testid="codex-session-affinity-retry"
            @click="loadSettings"
          >
            {{ t("common.refresh") }}
          </button>
        </div>

        <div
          class="rounded-lg border border-blue-100 bg-blue-50 px-3 py-3 text-xs leading-5 text-blue-700 dark:border-blue-900/60 dark:bg-blue-950/30 dark:text-blue-300"
          data-testid="codex-session-affinity-behavior"
        >
          <p>{{ t("admin.settings.codexSessionAffinity.behaviorEnrollment") }}</p>
          <p>{{ t("admin.settings.codexSessionAffinity.behaviorCandidates") }}</p>
          <p>{{ t("admin.settings.codexSessionAffinity.behaviorRemoval") }}</p>
        </div>

        <div class="border-t border-gray-100 pt-4 dark:border-dark-700">
          <div class="mb-3 flex items-start justify-between gap-4">
            <div>
              <h3 class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.settings.codexSessionAffinity.groups") }}
              </h3>
              <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
                {{ t("admin.settings.codexSessionAffinity.groupsHint") }}
              </p>
            </div>
            <span class="shrink-0 text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.codexSessionAffinity.selectedCount", { count: form.group_ids.length }) }}
            </span>
          </div>

          <div
            v-if="groupOptions.length === 0"
            class="rounded-lg border border-dashed border-gray-200 p-5 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
            data-testid="codex-session-affinity-empty"
          >
            {{ t("admin.settings.codexSessionAffinity.noGroups") }}
          </div>

          <div
            v-else
            class="grid max-h-64 gap-2 overflow-y-auto rounded-lg border border-gray-200 p-3 dark:border-dark-600 sm:grid-cols-2"
            data-testid="codex-session-affinity-group-list"
          >
            <label
              v-for="group in groupOptions"
              :key="group.id"
              class="flex min-w-0 cursor-pointer items-start gap-3 rounded-md px-2 py-2 hover:bg-gray-50 dark:hover:bg-dark-700"
              :data-testid="`codex-session-affinity-group-${group.id}`"
            >
              <input
                type="checkbox"
                class="mt-0.5 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                :checked="isSelected(group.id)"
                :disabled="controlsDisabled"
                @change="toggleGroup(group.id, ($event.target as HTMLInputElement).checked)"
              />
              <span class="min-w-0">
                <span class="block truncate text-sm text-gray-800 dark:text-gray-100">
                  {{ group.name || `#${group.id}` }}
                </span>
                <span class="mt-0.5 block text-xs text-gray-500 dark:text-gray-400">
                  #{{ group.id }} · {{ group.platform }} · {{ group.status }}
                  <span
                    v-if="!isEligibleGroup(group)"
                    class="ml-1 text-amber-600 dark:text-amber-400"
                  >
                    · {{ t("admin.settings.codexSessionAffinity.ineligible") }}
                  </span>
                </span>
              </span>
            </label>
          </div>
        </div>

        <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="controlsDisabled"
            data-testid="codex-session-affinity-save"
            @click="saveSettings"
          >
            <span
              v-if="saving"
              class="mr-1 inline-block h-4 w-4 animate-spin rounded-full border-2 border-white border-t-transparent"
            />
            {{ saving ? t("common.saving") : t("admin.settings.codexSessionAffinity.save") }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api";
import type { AdminGroup } from "@/types";
import type { CodexSessionAffinitySettings } from "@/types/codexSessionAffinity";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";

const MAX_GROUPS = 1000;
const { t } = useI18n();
const appStore = useAppStore();
const loading = ref(true);
const loaded = ref(false);
const loadError = ref(false);
const saving = ref(false);
const groups = ref<AdminGroup[]>([]);
const form = reactive<CodexSessionAffinitySettings>({ group_ids: [] });

const controlsDisabled = computed(
  () => loading.value || saving.value || loadError.value || !loaded.value,
);

type GroupOption = Pick<AdminGroup, "id" | "name" | "status"> & {
  platform: AdminGroup["platform"] | "unknown";
};

const groupOptions = computed<GroupOption[]>(() => {
  const eligible = groups.value.filter(isEligibleGroup);
  const eligibleIDs = new Set(eligible.map((group) => group.id));
  const selectedIneligible = groups.value.filter(
    (group) => form.group_ids.includes(group.id) && !eligibleIDs.has(group.id),
  );
  const known = new Set(groups.value.map((group) => group.id));
  const missing = form.group_ids
    .filter((id) => !known.has(id))
    .map<GroupOption>((id) => ({
      id,
      name: `#${id}`,
      platform: "unknown",
      status: "inactive",
    }));
  return [...eligible, ...selectedIneligible, ...missing];
});

function isEligibleGroup(group: GroupOption): boolean {
  return group.platform === "openai" || group.platform === "composite";
}

function applySettings(value: CodexSessionAffinitySettings | null | undefined): void {
  const seen = new Set<number>();
  const groupIDs: number[] = [];
  if (Array.isArray(value?.group_ids)) {
    for (const id of value.group_ids) {
      if (!Number.isSafeInteger(id) || id <= 0 || seen.has(id)) continue;
      seen.add(id);
      groupIDs.push(id);
      if (groupIDs.length >= MAX_GROUPS) break;
    }
  }
  form.group_ids = groupIDs;
}

function isSelected(groupID: number): boolean {
  return form.group_ids.includes(groupID);
}

function toggleGroup(groupID: number, selected: boolean): void {
  if (selected) {
    if (!isSelected(groupID) && form.group_ids.length < MAX_GROUPS) {
      form.group_ids.push(groupID);
    }
    return;
  }
  form.group_ids = form.group_ids.filter((id) => id !== groupID);
}

async function loadSettings(): Promise<void> {
  loading.value = true;
  loaded.value = false;
  loadError.value = false;
  try {
    const [settings, availableGroups] = await Promise.all([
      adminAPI.settings.getCodexSessionAffinitySettings(),
      adminAPI.groups.getAllIncludingInactive(),
    ]);
    applySettings(settings);
    groups.value = availableGroups;
    loaded.value = true;
  } catch (error: unknown) {
    loadError.value = true;
    groups.value = [];
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.codexSessionAffinity.loadFailed"),
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
    const response = await adminAPI.settings.updateCodexSessionAffinitySettings({
      group_ids: [...form.group_ids],
    });
    applySettings(response);
    appStore.showSuccess(t("admin.settings.codexSessionAffinity.saved"));
  } catch (error: unknown) {
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.codexSessionAffinity.saveFailed"),
      ),
    );
  } finally {
    saving.value = false;
  }
}

onMounted(loadSettings);
</script>
