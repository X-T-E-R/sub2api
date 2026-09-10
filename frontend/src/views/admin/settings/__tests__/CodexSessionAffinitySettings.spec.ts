import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import type { AdminGroup } from "@/types";
import type { CodexSessionAffinitySettings } from "@/types/codexSessionAffinity";

const apiMocks = vi.hoisted(() => ({
  getCodexSessionAffinitySettings: vi.fn(),
  updateCodexSessionAffinitySettings: vi.fn(),
  getAllIncludingInactive: vi.fn(),
}));
const toastMocks = vi.hoisted(() => ({
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api", () => ({
  adminAPI: {
    settings: apiMocks,
    groups: { getAllIncludingInactive: apiMocks.getAllIncludingInactive },
  },
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

import CodexSessionAffinitySettings from "../CodexSessionAffinitySettings.vue";

const groups = [
  { id: 12, name: "Codex Primary", platform: "openai", status: "active" },
  { id: 34, name: "Codex Backup", platform: "openai", status: "inactive" },
  { id: 56, name: "Claude Only", platform: "anthropic", status: "active" },
] as unknown as AdminGroup[];

function mountPanel() {
  return mount(CodexSessionAffinitySettings);
}

describe("CodexSessionAffinitySettings", () => {
  beforeEach(() => {
    apiMocks.getCodexSessionAffinitySettings.mockReset();
    apiMocks.updateCodexSessionAffinitySettings.mockReset();
    apiMocks.getAllIncludingInactive.mockReset();
    toastMocks.showError.mockReset();
    toastMocks.showSuccess.mockReset();
    apiMocks.getCodexSessionAffinitySettings.mockResolvedValue({ group_ids: [12] });
    apiMocks.getAllIncludingInactive.mockResolvedValue(groups);
    apiMocks.updateCodexSessionAffinitySettings.mockImplementation(
      async (settings: CodexSessionAffinitySettings) => settings,
    );
  });

  it("loads active and inactive groups, edits selection, and saves", async () => {
    const wrapper = mountPanel();
    await flushPromises();

    expect(apiMocks.getCodexSessionAffinitySettings).toHaveBeenCalledTimes(1);
    expect(apiMocks.getAllIncludingInactive).toHaveBeenCalledTimes(1);
    expect(
      (wrapper.get('[data-testid="codex-session-affinity-group-12"] input').element as HTMLInputElement)
        .checked,
    ).toBe(true);
    expect(
      wrapper.get('[data-testid="codex-session-affinity-group-34"] input'),
    ).toBeTruthy();

    await wrapper
      .get('[data-testid="codex-session-affinity-group-34"] input')
      .setValue(true);
    await wrapper.get('[data-testid="codex-session-affinity-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateCodexSessionAffinitySettings).toHaveBeenCalledWith({
      group_ids: [12, 34],
    });
    expect(toastMocks.showSuccess).toHaveBeenCalledWith(
      "admin.settings.codexSessionAffinity.saved",
    );
  });

  it("keeps existing binding IDs visible when the group list no longer includes them", async () => {
    apiMocks.getCodexSessionAffinitySettings.mockResolvedValueOnce({ group_ids: [99] });
    apiMocks.getAllIncludingInactive.mockResolvedValueOnce([]);
    const wrapper = mountPanel();
    await flushPromises();

    expect(
      wrapper.find('[data-testid="codex-session-affinity-group-99"]').exists(),
    ).toBe(true);
    expect(
      (wrapper.get('[data-testid="codex-session-affinity-group-99"] input').element as HTMLInputElement)
        .checked,
    ).toBe(true);
  });

  it("hides ineligible groups while retaining selected ineligible IDs for removal", async () => {
    apiMocks.getCodexSessionAffinitySettings.mockResolvedValueOnce({ group_ids: [56] });
    const wrapper = mountPanel();
    await flushPromises();

    expect(wrapper.find('[data-testid="codex-session-affinity-group-12"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="codex-session-affinity-group-56"]').exists()).toBe(true);
    expect(
      (wrapper.get('[data-testid="codex-session-affinity-group-56"] input').element as HTMLInputElement)
        .checked,
    ).toBe(true);
    await wrapper
      .get('[data-testid="codex-session-affinity-group-56"] input')
      .setValue(false);
    await wrapper.get('[data-testid="codex-session-affinity-save"]').trigger("click");
    await flushPromises();

    expect(apiMocks.updateCodexSessionAffinitySettings).toHaveBeenCalledWith({
      group_ids: [],
    });
  });

  it("blocks save after a load failure until retry succeeds", async () => {
    apiMocks.getCodexSessionAffinitySettings.mockRejectedValueOnce(new Error("load failed"));
    const wrapper = mountPanel();
    await flushPromises();

    expect(wrapper.find('[data-testid="codex-session-affinity-load-error"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-testid="codex-session-affinity-save"]').attributes("disabled"),
    ).toBeDefined();
    expect(apiMocks.updateCodexSessionAffinitySettings).not.toHaveBeenCalled();

    apiMocks.getCodexSessionAffinitySettings.mockResolvedValueOnce({ group_ids: [] });
    apiMocks.getAllIncludingInactive.mockResolvedValueOnce(groups);
    await wrapper.get('[data-testid="codex-session-affinity-retry"]').trigger("click");
    await flushPromises();

    expect(wrapper.find('[data-testid="codex-session-affinity-load-error"]').exists()).toBe(false);
    expect(
      wrapper.get('[data-testid="codex-session-affinity-save"]').attributes("disabled"),
    ).toBeUndefined();
  });
});
