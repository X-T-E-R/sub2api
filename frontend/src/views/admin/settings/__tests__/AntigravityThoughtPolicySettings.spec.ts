import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import type { AntigravityThoughtPolicySettings as ThoughtPolicySettings } from "@/types/antigravityThoughtPolicy";

const apiMocks = vi.hoisted(() => ({
  getAntigravityThoughtPolicySettings: vi.fn(),
  updateAntigravityThoughtPolicySettings: vi.fn(),
}));
const toastMocks = vi.hoisted(() => ({
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api/admin/antigravityThoughtPolicy", () => apiMocks);

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

import ThoughtPolicyCard from "../AntigravityThoughtPolicySettings.vue";

const SUPPRESS = '[data-testid="antigravity-thought-policy-suppress"]';
const CONTINUE = '[data-testid="antigravity-thought-policy-continue"]';
const SAVE = '[data-testid="antigravity-thought-policy-save"]';
const ZERO = '[data-testid="antigravity-thought-policy-zero-output"]';

function mountCard(): VueWrapper {
  return mount(ThoughtPolicyCard);
}

function isOn(wrapper: VueWrapper, selector: string): boolean {
  return wrapper.get(selector).attributes("aria-checked") === "true";
}

describe("AntigravityThoughtPolicySettings", () => {
  beforeEach(() => {
    apiMocks.getAntigravityThoughtPolicySettings.mockReset();
    apiMocks.updateAntigravityThoughtPolicySettings.mockReset();
    toastMocks.showError.mockReset();
    toastMocks.showSuccess.mockReset();
    i18nMocks.t.mockReset();
    i18nMocks.t.mockImplementation(
      (key: string, params?: Record<string, string | number>) =>
        params
          ? key.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ""))
          : key,
    );
    apiMocks.getAntigravityThoughtPolicySettings.mockResolvedValue({
      suppress_thought_only_stop: false,
      continue_thought_only: true,
    });
    apiMocks.updateAntigravityThoughtPolicySettings.mockImplementation(
      async (settings: ThoughtPolicySettings) => settings,
    );
  });

  it("loads both switches and saves the edited pair", async () => {
    const wrapper = mountCard();
    await flushPromises();

    expect(apiMocks.getAntigravityThoughtPolicySettings).toHaveBeenCalledTimes(1);
    expect(isOn(wrapper, SUPPRESS)).toBe(false);
    expect(isOn(wrapper, CONTINUE)).toBe(true);
    expect(
      wrapper.get('[data-testid="antigravity-thought-policy-hint"]').text(),
    ).toBe("admin.settings.antigravityThoughtPolicy.independenceHint");

    await wrapper.get(SUPPRESS).trigger("click");
    expect(isOn(wrapper, SUPPRESS)).toBe(true);
    expect(isOn(wrapper, CONTINUE)).toBe(true);

    await wrapper.get(SAVE).trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityThoughtPolicySettings).toHaveBeenCalledWith({
      suppress_thought_only_stop: true,
      continue_thought_only: true,
      continue_zero_output: false,
    });
    expect(toastMocks.showSuccess).toHaveBeenCalledWith(
      "admin.settings.antigravityThoughtPolicy.saved",
    );
    expect(toastMocks.showError).not.toHaveBeenCalled();
  });

  it("turns a single switch off without touching the other one", async () => {
    const wrapper = mountCard();
    await flushPromises();

    await wrapper.get(CONTINUE).trigger("click");
    await wrapper.get(SAVE).trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityThoughtPolicySettings).toHaveBeenCalledWith({
      suppress_thought_only_stop: false,
      continue_thought_only: false,
      continue_zero_output: false,
    });
  });

  it("saves completely zero-output continuation without enabling legacy A or B", async () => {
    apiMocks.getAntigravityThoughtPolicySettings.mockResolvedValueOnce({
      suppress_thought_only_stop: false, continue_thought_only: false,
    });
    const wrapper = mountCard();
    await flushPromises();
    expect(isOn(wrapper, ZERO)).toBe(false);
    await wrapper.get(ZERO).trigger("click");
    await wrapper.get(SAVE).trigger("click");
    await flushPromises();
    expect(apiMocks.updateAntigravityThoughtPolicySettings).toHaveBeenCalledWith({
      suppress_thought_only_stop: false, continue_thought_only: false, continue_zero_output: true,
    });
    expect(isOn(wrapper, ZERO)).toBe(true);
    expect(isOn(wrapper, SUPPRESS)).toBe(false);
    expect(isOn(wrapper, CONTINUE)).toBe(false);
  });

  it("stays read-only after a load failure until the retry succeeds", async () => {
    apiMocks.getAntigravityThoughtPolicySettings.mockReset();
    apiMocks.getAntigravityThoughtPolicySettings.mockRejectedValueOnce(
      new Error("load failed"),
    );

    const wrapper = mountCard();
    await flushPromises();

    expect(
      wrapper.find('[data-testid="antigravity-thought-policy-load-error"]').exists(),
    ).toBe(true);
    expect(wrapper.get(SAVE).attributes("disabled")).toBeDefined();
    expect(wrapper.get(SUPPRESS).attributes("disabled")).toBeDefined();

    await wrapper.get(SAVE).trigger("click");
    await wrapper.get(SUPPRESS).trigger("click");
    await flushPromises();

    expect(apiMocks.updateAntigravityThoughtPolicySettings).not.toHaveBeenCalled();
    expect(isOn(wrapper, SUPPRESS)).toBe(false);
    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.antigravityThoughtPolicy.loadFailed",
    );

    apiMocks.getAntigravityThoughtPolicySettings.mockResolvedValueOnce({
      suppress_thought_only_stop: true,
      continue_thought_only: true,
      continue_zero_output: false,
    });
    await wrapper.get('[data-testid="antigravity-thought-policy-retry"]').trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="antigravity-thought-policy-load-error"]').exists(),
    ).toBe(false);
    expect(isOn(wrapper, SUPPRESS)).toBe(true);
    expect(isOn(wrapper, CONTINUE)).toBe(true);
    expect(wrapper.get(SAVE).attributes("disabled")).toBeUndefined();
  });

  it("keeps the edited pair when saving fails", async () => {
    apiMocks.updateAntigravityThoughtPolicySettings.mockRejectedValueOnce(
      new Error("save failed"),
    );

    const wrapper = mountCard();
    await flushPromises();
    await wrapper.get(SUPPRESS).trigger("click");
    await wrapper.get(SAVE).trigger("click");
    await flushPromises();

    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.antigravityThoughtPolicy.saveFailed",
    );
    expect(isOn(wrapper, SUPPRESS)).toBe(true);
    expect(wrapper.get(SAVE).attributes("disabled")).toBeUndefined();
  });

  it("disables both switches while saving and applies the server readback", async () => {
    let resolveUpdate!: (value: ThoughtPolicySettings) => void;
    apiMocks.updateAntigravityThoughtPolicySettings.mockImplementationOnce(
      () =>
        new Promise<ThoughtPolicySettings>((resolve) => {
          resolveUpdate = resolve;
        }),
    );

    const wrapper = mountCard();
    await flushPromises();
    await wrapper.get(SUPPRESS).trigger("click");
    await wrapper.get(SAVE).trigger("click");

    expect(wrapper.get(SAVE).attributes("disabled")).toBeDefined();
    expect(wrapper.get(SUPPRESS).attributes("disabled")).toBeDefined();
    expect(wrapper.get(CONTINUE).attributes("disabled")).toBeDefined();
    expect(wrapper.get(SAVE).text()).toBe("common.saving");

    resolveUpdate({ suppress_thought_only_stop: false, continue_thought_only: true, continue_zero_output: false });
    await flushPromises();

    expect(isOn(wrapper, SUPPRESS)).toBe(false);
    expect(isOn(wrapper, CONTINUE)).toBe(true);
    expect(wrapper.get(SAVE).attributes("disabled")).toBeUndefined();
  });
});
