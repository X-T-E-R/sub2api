import { beforeEach, describe, expect, it, vi } from "vitest";
import { defineComponent } from "vue";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import type { AntigravityModelEffortSettings } from "@/types/antigravityModelEffort";

const apiMocks = vi.hoisted(() => ({
  getAntigravityModelEffortSettings: vi.fn(),
  updateAntigravityModelEffortSettings: vi.fn(),
}));
const toastMocks = vi.hoisted(() => ({
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api/admin", () => ({
  adminAPI: { settings: apiMocks },
}));

vi.mock("@/stores", () => ({
  useAppStore: () => toastMocks,
}));

vi.mock("@/utils/apiError", () => ({
  extractApiErrorMessage: (_error: unknown, fallback: string) => fallback,
}));

const i18nMocks = vi.hoisted(() => ({
  t: vi.fn(),
}));

vi.mock("vue-i18n", () => ({
  useI18n: () => ({ t: i18nMocks.t }),
}));

const SelectStub = defineComponent({
  props: {
    modelValue: { type: [String, Number, Boolean], default: null },
    options: { type: Array, default: () => [] },
    id: { type: String, default: undefined },
  },
  emits: ["update:modelValue"],
  template: `<select :id="id" :value="modelValue" @change="onChange"><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>`,
  setup(_, { emit }) {
    function onChange(event: Event): void {
      emit("update:modelValue", (event.target as HTMLSelectElement).value);
    }
    return { onChange };
  },
});

import AntigravityModelEffortSettings from "../AntigravityModelEffortSettings.vue";

const SEED_LEVELS = [
  "none",
  "minimal",
  "low",
  "medium",
  "high",
  "xhigh",
  "max",
];

function mountPanel(): VueWrapper {
  return mount(AntigravityModelEffortSettings, {
    global: {
      stubs: {
        Select: SelectStub,
      },
    },
  });
}

function textareaValue(wrapper: VueWrapper): string {
  return (wrapper.get('[data-testid="antigravity-model-effort-levels"]')
    .element as HTMLTextAreaElement).value;
}

function selectValue(wrapper: VueWrapper): string {
  return (wrapper.get('[data-testid="antigravity-model-effort-default"]')
    .element as HTMLSelectElement).value;
}

function optionValues(wrapper: VueWrapper): string[] {
  return wrapper
    .get('[data-testid="antigravity-model-effort-default"]')
    .findAll("option")
    .map((option) => option.attributes("value") ?? "");
}

describe("AntigravityModelEffortSettings", () => {
  beforeEach(() => {
    apiMocks.getAntigravityModelEffortSettings.mockReset();
    apiMocks.updateAntigravityModelEffortSettings.mockReset();
    toastMocks.showError.mockReset();
    toastMocks.showSuccess.mockReset();
    i18nMocks.t.mockReset();
    i18nMocks.t.mockImplementation(
      (key: string, params?: Record<string, string | number>) =>
        params
          ? key.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ""))
          : key,
    );
    apiMocks.getAntigravityModelEffortSettings.mockResolvedValue({
      levels: [...SEED_LEVELS],
      default_effort: "medium",
    });
    apiMocks.updateAntigravityModelEffortSettings.mockImplementation(
      async (settings: AntigravityModelEffortSettings) => settings,
    );
  });

  it("loads names and default, then saves an added future name as the default", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    expect(apiMocks.getAntigravityModelEffortSettings).toHaveBeenCalledTimes(1);
    expect(textareaValue(wrapper)).toBe(SEED_LEVELS.join("\n"));
    expect(selectValue(wrapper)).toBe("medium");

    // A future token the frontend has no enum for must be accepted as-is.
    await wrapper
      .get('[data-testid="antigravity-model-effort-levels"]')
      .setValue(`${SEED_LEVELS.join("\n")}\nextra-low\nultra`);
    expect(optionValues(wrapper)).toContain("ultra");
    expect(optionValues(wrapper)).toContain("extra-low");

    await wrapper
      .get('[data-testid="antigravity-model-effort-default"]')
      .setValue("ultra");
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityModelEffortSettings).toHaveBeenCalledWith({
      levels: [...SEED_LEVELS, "extra-low", "ultra"],
      default_effort: "ultra",
    });
    expect(toastMocks.showSuccess).toHaveBeenCalledWith(
      "admin.settings.antigravityModelEffort.saved",
    );
    expect(toastMocks.showError).not.toHaveBeenCalled();
  });

  it("normalizes pasted names to lowercase and accepts comma separators", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="antigravity-model-effort-levels"]')
      .setValue("LOW, HIGH\n Extra-Low , medium");
    await wrapper
      .get('[data-testid="antigravity-model-effort-default"]')
      .setValue("high");
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityModelEffortSettings).toHaveBeenCalledWith({
      levels: ["low", "high", "extra-low", "medium"],
      default_effort: "high",
    });
  });

  it("blocks saving with inline errors for invalid, duplicate and empty names", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    const levels = wrapper.get('[data-testid="antigravity-model-effort-levels"]');

    await levels.setValue("low\nUltra!\nhigh");
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityModelEffortSettings).not.toHaveBeenCalled();
    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.antigravityModelEffort.validationFailed",
    );
    const invalidError = wrapper.get(
      '[data-testid="antigravity-model-effort-levels-error"]',
    ).text();
    expect(invalidError).toContain("validation.invalid");
    expect(i18nMocks.t).toHaveBeenCalledWith(
      "admin.settings.antigravityModelEffort.validation.invalid",
      { names: "ultra!" },
    );

    await levels.setValue("low\nLOW\nhigh");
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-levels-error"]').text(),
    ).toContain("validation.duplicate");

    await levels.setValue("");
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-levels-error"]').text(),
    ).toContain("validation.required");

    const tooMany = Array.from({ length: 65 }, (_, index) => `level-${index}`).join(
      "\n",
    );
    await levels.setValue(tooMany);
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-levels-error"]').text(),
    ).toContain("validation.tooMany");

    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    expect(apiMocks.updateAntigravityModelEffortSettings).not.toHaveBeenCalled();
  });

  it("requires the default effort to stay inside the allowed names", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="antigravity-model-effort-levels"]')
      .setValue("low\nhigh");
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityModelEffortSettings).not.toHaveBeenCalled();
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-default-error"]').text(),
    ).toContain("validation.defaultNotInLevels");

    await wrapper
      .get('[data-testid="antigravity-model-effort-default"]')
      .setValue("high");
    expect(
      wrapper.find('[data-testid="antigravity-model-effort-default-error"]').exists(),
    ).toBe(false);

    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityModelEffortSettings).toHaveBeenCalledWith({
      levels: ["low", "high"],
      default_effort: "high",
    });
  });

  it("keeps the edited form when saving fails", async () => {
    apiMocks.updateAntigravityModelEffortSettings.mockRejectedValueOnce(
      new Error("save failed"),
    );

    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="antigravity-model-effort-levels"]')
      .setValue("ultra\nlow");
    await wrapper
      .get('[data-testid="antigravity-model-effort-default"]')
      .setValue("ultra");
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    await flushPromises();

    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.antigravityModelEffort.saveFailed",
    );
    expect(textareaValue(wrapper)).toBe("ultra\nlow");
    expect(selectValue(wrapper)).toBe("ultra");
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-save"]').attributes("disabled"),
    ).toBeUndefined();
  });

  it("blocks saving after a load failure until retry succeeds", async () => {
    apiMocks.getAntigravityModelEffortSettings.mockReset();
    apiMocks.getAntigravityModelEffortSettings.mockRejectedValueOnce(
      new Error("load failed"),
    );

    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.find('[data-testid="antigravity-model-effort-load-error"]').exists(),
    ).toBe(true);
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-save"]').attributes("disabled"),
    ).toBeDefined();
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-levels"]').attributes("disabled"),
    ).toBeDefined();
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");
    expect(apiMocks.updateAntigravityModelEffortSettings).not.toHaveBeenCalled();
    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.antigravityModelEffort.loadFailed",
    );
    // The unread panel must not blame the administrator for its own empty state.
    expect(
      wrapper.find('[data-testid="antigravity-model-effort-levels-error"]').exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="antigravity-model-effort-default-error"]').exists(),
    ).toBe(false);
    expect(
      wrapper.find('[data-testid="antigravity-model-effort-levels-summary"]').exists(),
    ).toBe(false);

    apiMocks.getAntigravityModelEffortSettings.mockResolvedValueOnce({
      levels: ["ultra", "low"],
      default_effort: "ultra",
    });
    await wrapper.get('[data-testid="antigravity-model-effort-retry"]').trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="antigravity-model-effort-load-error"]').exists(),
    ).toBe(false);
    expect(textareaValue(wrapper)).toBe("ultra\nlow");
    expect(selectValue(wrapper)).toBe("ultra");
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-save"]').attributes("disabled"),
    ).toBeUndefined();
  });

  it("disables editing while a save is pending", async () => {
    let resolveUpdate!: (value: AntigravityModelEffortSettings) => void;
    apiMocks.updateAntigravityModelEffortSettings.mockImplementationOnce(
      () =>
        new Promise<AntigravityModelEffortSettings>((resolve) => {
          resolveUpdate = resolve;
        }),
    );

    const wrapper = mountPanel();
    await flushPromises();
    await wrapper
      .get('[data-testid="antigravity-model-effort-save"]')
      .trigger("click");

    expect(
      wrapper.get('[data-testid="antigravity-model-effort-save"]').attributes("disabled"),
    ).toBeDefined();
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-levels"]').attributes("disabled"),
    ).toBeDefined();
    expect(
      wrapper.get('[data-testid="antigravity-model-effort-default"]').attributes("disabled"),
    ).toBeDefined();
    expect(wrapper.get('[data-testid="antigravity-model-effort-save"]').text()).toBe(
      "common.saving",
    );

    resolveUpdate({ levels: [...SEED_LEVELS], default_effort: "medium" });
    await flushPromises();

    expect(
      wrapper.get('[data-testid="antigravity-model-effort-save"]').attributes("disabled"),
    ).toBeUndefined();
  });
});
