import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import type { ModelTestPromptSettings } from "@/types/modelTestPrompt";

const apiMocks = vi.hoisted(() => ({
  getModelTestPromptSettings: vi.fn(),
  updateModelTestPromptSettings: vi.fn(),
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

import ModelTestPromptSettings from "../ModelTestPromptSettings.vue";

const STORED: ModelTestPromptSettings = {
  prompt: "Reply with the single word: hi",
  default_prompt: "hi",
};

function mountPanel(): VueWrapper {
  return mount(ModelTestPromptSettings);
}

function promptValue(wrapper: VueWrapper): string {
  return (wrapper.get('[data-testid="model-test-prompt-input"]')
    .element as HTMLTextAreaElement).value;
}

function saveDisabled(wrapper: VueWrapper): boolean {
  return (
    wrapper.get('[data-testid="model-test-prompt-save"]').attributes("disabled") !==
    undefined
  );
}

describe("ModelTestPromptSettings", () => {
  beforeEach(() => {
    apiMocks.getModelTestPromptSettings.mockReset();
    apiMocks.updateModelTestPromptSettings.mockReset();
    toastMocks.showError.mockReset();
    toastMocks.showSuccess.mockReset();
    i18nMocks.t.mockReset();
    i18nMocks.t.mockImplementation(
      (key: string, params?: Record<string, string | number>) =>
        params
          ? key.replace(/\{(\w+)\}/g, (_, name: string) => String(params[name] ?? ""))
          : key,
    );
    apiMocks.getModelTestPromptSettings.mockResolvedValue({ ...STORED });
    apiMocks.updateModelTestPromptSettings.mockImplementation(
      async (prompt: string) => ({ prompt, default_prompt: STORED.default_prompt }),
    );
  });

  it("loads the stored prompt, its length and the built-in default it would fall back to", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    expect(apiMocks.getModelTestPromptSettings).toHaveBeenCalledTimes(1);
    expect(promptValue(wrapper)).toBe(STORED.prompt);
    expect(
      wrapper.get('[data-testid="model-test-prompt-default"]').text(),
    ).toContain("hi");
    expect(
      wrapper.get('[data-testid="model-test-prompt-counter"]').text(),
    ).toBe("admin.settings.modelTestPrompt.counter");
    expect(i18nMocks.t).toHaveBeenCalledWith(
      "admin.settings.modelTestPrompt.counter",
      { count: STORED.prompt.length, max: 8192 },
    );
  });

  it("saves the trimmed prompt and adopts the stored value from the response", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="model-test-prompt-input"]')
      .setValue("  Answer with the single word: pong  ");
    await wrapper.get('[data-testid="model-test-prompt-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateModelTestPromptSettings).toHaveBeenCalledWith(
      "Answer with the single word: pong",
    );
    expect(toastMocks.showSuccess).toHaveBeenCalledWith(
      "admin.settings.modelTestPrompt.saved",
    );
    expect(promptValue(wrapper)).toBe("Answer with the single word: pong");
  });

  it("stores an empty prompt when the field is cleared, restoring the built-in default", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper.get('[data-testid="model-test-prompt-input"]').setValue("   ");
    await wrapper.get('[data-testid="model-test-prompt-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateModelTestPromptSettings).toHaveBeenCalledWith("");
    expect(promptValue(wrapper)).toBe("");
  });

  it("rejects a prompt beyond the backend limit and blocks the save", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="model-test-prompt-input"]')
      .setValue("x".repeat(8193));

    expect(wrapper.find('[data-testid="model-test-prompt-error"]').exists()).toBe(true);
    expect(i18nMocks.t).toHaveBeenCalledWith(
      "admin.settings.modelTestPrompt.tooLong",
      { count: 8193, max: 8192 },
    );
    expect(saveDisabled(wrapper)).toBe(true);

    await wrapper.get('[data-testid="model-test-prompt-save"]').trigger("click");
    await flushPromises();
    expect(apiMocks.updateModelTestPromptSettings).not.toHaveBeenCalled();
  });

  it("counts astral characters once, like the backend rune limit", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    const input = wrapper.get('[data-testid="model-test-prompt-input"]');
    await input.setValue("😀".repeat(8192));
    expect(wrapper.find('[data-testid="model-test-prompt-error"]').exists()).toBe(false);
    expect(saveDisabled(wrapper)).toBe(false);

    await input.setValue("😀".repeat(8193));
    expect(wrapper.find('[data-testid="model-test-prompt-error"]').exists()).toBe(true);
    expect(saveDisabled(wrapper)).toBe(true);
  });

  it("resets a stored prompt to the built-in default and reports it", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.get('[data-testid="model-test-prompt-reset"]').attributes("disabled"),
    ).toBeUndefined();
    await wrapper.get('[data-testid="model-test-prompt-reset"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateModelTestPromptSettings).toHaveBeenCalledWith("");
    expect(toastMocks.showSuccess).toHaveBeenCalledWith(
      "admin.settings.modelTestPrompt.resetSaved",
    );
    expect(promptValue(wrapper)).toBe("");
    // Nothing stored means nothing left to reset.
    expect(
      wrapper.get('[data-testid="model-test-prompt-reset"]').attributes("disabled"),
    ).toBeDefined();
  });

  it("keeps the edited prompt when saving fails", async () => {
    apiMocks.updateModelTestPromptSettings.mockRejectedValueOnce(
      new Error("save failed"),
    );

    const wrapper = mountPanel();
    await flushPromises();

    await wrapper
      .get('[data-testid="model-test-prompt-input"]')
      .setValue("Keep me");
    await wrapper.get('[data-testid="model-test-prompt-save"]').trigger("click");
    await flushPromises();

    expect(toastMocks.showError).toHaveBeenCalledWith(
      "admin.settings.modelTestPrompt.saveFailed",
    );
    expect(promptValue(wrapper)).toBe("Keep me");
    expect(saveDisabled(wrapper)).toBe(false);
  });

  it("stays read-only after a load failure until a retry succeeds", async () => {
    apiMocks.getModelTestPromptSettings.mockReset();
    apiMocks.getModelTestPromptSettings.mockRejectedValueOnce(
      new Error("load failed"),
    );

    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.find('[data-testid="model-test-prompt-load-error"]').exists(),
    ).toBe(true);
    expect(
      wrapper.get('[data-testid="model-test-prompt-input"]').attributes("disabled"),
    ).toBeDefined();
    expect(saveDisabled(wrapper)).toBe(true);
    // An unread panel must not blame the administrator for its own empty state.
    expect(wrapper.find('[data-testid="model-test-prompt-error"]').exists()).toBe(
      false,
    );

    apiMocks.getModelTestPromptSettings.mockResolvedValueOnce({ ...STORED });
    await wrapper.get('[data-testid="model-test-prompt-retry"]').trigger("click");
    await flushPromises();

    expect(
      wrapper.find('[data-testid="model-test-prompt-load-error"]').exists(),
    ).toBe(false);
    expect(promptValue(wrapper)).toBe(STORED.prompt);
    expect(saveDisabled(wrapper)).toBe(false);
  });
});
