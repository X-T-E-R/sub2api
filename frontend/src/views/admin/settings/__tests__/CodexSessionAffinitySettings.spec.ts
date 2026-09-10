import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import type { AdminGroup } from "@/types";
import type { CodexSessionAffinitySettings } from "@/types/codexSessionAffinity";
import enSettings from "@/i18n/locales/en/admin/settings";
import zhSettings from "@/i18n/locales/zh/admin/settings";

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

  it("describes durable preference, bounded waits, and eligible failover in both locales", () => {
    const enCopy = enSettings.settings.codexSessionAffinity;
    const zhCopy = zhSettings.settings.codexSessionAffinity;

    expect(enCopy.description).toContain("preferred current account");
    expect(enCopy.behaviorEnrollment).toContain("durable current-account preference");
    expect(enCopy.behaviorCandidates).toContain("wait for a bounded time");
    expect(enCopy.behaviorCandidates).toContain("genuine quota");
    expect(enCopy.behaviorCandidates).toContain("Daily session allocation rules are unchanged");
    expect(enCopy.behaviorRemoval).toContain("existing preferences remain eligible to move");
    expect(enCopy.groupsHint).toContain("does not clear existing session preferences");
    expect(enCopy.behaviorCandidates).not.toContain("does not switch accounts automatically");

    expect(zhCopy.description).toContain("当前账号偏好");
    expect(zhCopy.behaviorEnrollment).toContain("持久化的当前账号偏好");
    expect(zhCopy.behaviorCandidates).toContain("有界时间内等待");
    expect(zhCopy.behaviorCandidates).toContain("真实配额、认证、账号禁用或不兼容失败");
    expect(zhCopy.behaviorCandidates).toContain("每日会话分配规则不变");
    expect(zhCopy.behaviorRemoval).toContain("已有偏好仍可");
    expect(zhCopy.groupsHint).toContain("不会清除已有会话偏好");
    expect(zhCopy.behaviorCandidates).not.toContain("不会自动切换账号");
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
