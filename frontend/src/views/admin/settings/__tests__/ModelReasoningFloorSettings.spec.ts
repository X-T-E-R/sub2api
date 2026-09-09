import { beforeEach, describe, expect, it, vi } from "vitest";
import { defineComponent } from "vue";
import { flushPromises, mount } from "@vue/test-utils";
import type { ModelReasoningFloorSettings } from "@/types/modelReasoningFloor";

const apiMocks = vi.hoisted(() => ({
  getModelReasoningFloorSettings: vi.fn(),
  updateModelReasoningFloorSettings: vi.fn(),
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

vi.mock("vue-i18n", () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, string | number>) =>
      params
        ? key.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ""))
        : key,
  }),
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

const ToggleStub = defineComponent({
  props: {
    modelValue: { type: Boolean, default: false },
  },
  emits: ["update:modelValue"],
  template: `<input type="checkbox" :checked="modelValue" data-testid="model-reasoning-floor-enabled" @change="onChange" />`,
  setup(_, { emit }) {
    function onChange(event: Event): void {
      emit("update:modelValue", (event.target as HTMLInputElement).checked);
    }
    return { onChange };
  },
});

import ModelReasoningFloorSettings from "../ModelReasoningFloorSettings.vue";

function mountPanel() {
  return mount(ModelReasoningFloorSettings, {
    global: {
      stubs: {
        Select: SelectStub,
        Toggle: ToggleStub,
        Icon: true,
      },
    },
  });
}

describe("ModelReasoningFloorSettings", () => {
  beforeEach(() => {
    apiMocks.getModelReasoningFloorSettings.mockReset();
    apiMocks.updateModelReasoningFloorSettings.mockReset();
    toastMocks.showError.mockReset();
    toastMocks.showSuccess.mockReset();
    apiMocks.getModelReasoningFloorSettings.mockResolvedValue({
      enabled: true,
      rules: [{ model: "gpt-5.6-sol", min_effort: "high" }],
    });
    apiMocks.updateModelReasoningFloorSettings.mockImplementation(
      async (settings: unknown) => settings,
    );
  });

  it("loads rules, edits the minimum, and saves both fields", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    expect(apiMocks.getModelReasoningFloorSettings).toHaveBeenCalledTimes(1);
    expect(
      (wrapper.get('[data-testid="model-reasoning-floor-model-0"]').element as HTMLInputElement)
        .value,
    ).toBe("gpt-5.6-sol");

    const effort = wrapper.get('[data-testid="model-reasoning-floor-effort-0"]');
    await effort.setValue("max");
    await wrapper.get('[data-testid="model-reasoning-floor-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateModelReasoningFloorSettings).toHaveBeenCalledWith({
      enabled: true,
      rules: [{ model: "gpt-5.6-sol", min_effort: "max" }],
    });
    expect(toastMocks.showSuccess).toHaveBeenCalledWith(
      "admin.settings.modelReasoningFloor.saved",
    );
  });

  it("retains rules while disabled and blocks invalid duplicate model IDs", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper.get('[data-testid="model-reasoning-floor-enabled"]').setValue(false);
    await wrapper.get('[data-testid="model-reasoning-floor-add"]').trigger("click");
    await wrapper
      .get('[data-testid="model-reasoning-floor-model-1"]')
      .setValue("gpt-5.6-sol");
    await wrapper.get('[data-testid="model-reasoning-floor-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateModelReasoningFloorSettings).not.toHaveBeenCalled();
    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.modelReasoningFloor.validationFailed",
    );

    await wrapper
      .get('[data-testid="model-reasoning-floor-model-1"]')
      .setValue("gpt-5.6-new");
    await wrapper.get('[data-testid="model-reasoning-floor-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateModelReasoningFloorSettings).toHaveBeenCalledWith({
      enabled: false,
      rules: [
        { model: "gpt-5.6-sol", min_effort: "high" },
        { model: "gpt-5.6-new", min_effort: "minimal" },
      ],
    });
  });

  it("rejects whitespace and wildcard model IDs before saving", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="model-reasoning-floor-model-0"]')
      .setValue("gpt 5.6*");
    await wrapper.get('[data-testid="model-reasoning-floor-save"]').trigger("click");

    expect(apiMocks.updateModelReasoningFloorSettings).not.toHaveBeenCalled();
    expect(wrapper.get('[data-testid="model-reasoning-floor-error-0"]').text()).toBe(
      "admin.settings.modelReasoningFloor.validation.invalid",
    );
  });

  it("keeps saving disabled after a load failure until retry succeeds", async () => {
    apiMocks.getModelReasoningFloorSettings.mockReset();
    apiMocks.getModelReasoningFloorSettings.mockRejectedValueOnce(
      new Error("load failed"),
    );

    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.find('[data-testid="model-reasoning-floor-load-error"]').exists(),
    ).toBe(true);
    expect(
      wrapper.get('[data-testid="model-reasoning-floor-save"]').attributes("disabled"),
    ).toBeDefined();
    expect(apiMocks.updateModelReasoningFloorSettings).not.toHaveBeenCalled();
    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.modelReasoningFloor.loadFailed",
    );

    apiMocks.getModelReasoningFloorSettings.mockResolvedValueOnce({
      enabled: true,
      rules: [{ model: "o3", min_effort: "medium" }],
    });
    await wrapper.get('[data-testid="model-reasoning-floor-retry"]').trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="model-reasoning-floor-load-error"]').exists(),
    ).toBe(false);
    expect(
      wrapper.get('[data-testid="model-reasoning-floor-save"]').attributes("disabled"),
    ).toBeUndefined();
    expect(
      (wrapper.get('[data-testid="model-reasoning-floor-model-0"]').element as HTMLInputElement)
        .value,
    ).toBe("o3");
  });

  it("disables editing while a save is pending", async () => {
    let resolveUpdate!: (value: ModelReasoningFloorSettings) => void;
    apiMocks.updateModelReasoningFloorSettings.mockImplementationOnce(
      () =>
        new Promise<ModelReasoningFloorSettings>((resolve) => {
          resolveUpdate = resolve;
        }),
    );

    const wrapper = mountPanel();
    await flushPromises();
    await wrapper.get('[data-testid="model-reasoning-floor-save"]').trigger("click");

    expect(
      wrapper.get('[data-testid="model-reasoning-floor-save"]').attributes("disabled"),
    ).toBeDefined();
    expect(
      wrapper.get('[data-testid="model-reasoning-floor-model-0"]').attributes("disabled"),
    ).toBeDefined();
    expect(
      wrapper.get('[data-testid="model-reasoning-floor-add"]').attributes("disabled"),
    ).toBeDefined();

    resolveUpdate({
      enabled: true,
      rules: [{ model: "gpt-5.6-sol", min_effort: "high" }],
    });
    await flushPromises();

    expect(
      wrapper.get('[data-testid="model-reasoning-floor-save"]').attributes("disabled"),
    ).toBeUndefined();
  });
});
