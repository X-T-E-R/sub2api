import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getCodexSessionAffinitySettings,
  updateCodexSessionAffinitySettings,
  type CodexSessionAffinitySettings,
} from "@/api/admin/settings";

describe("admin Codex session affinity API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads selected groups from the dedicated endpoint", async () => {
    const response: CodexSessionAffinitySettings = { group_ids: [12, 34] };
    get.mockResolvedValueOnce({ data: response });

    await expect(getCodexSessionAffinitySettings()).resolves.toEqual(response);
    expect(get).toHaveBeenCalledWith("/admin/settings/codex-session-affinity");
  });

  it("writes selected groups to the dedicated endpoint", async () => {
    const payload: CodexSessionAffinitySettings = { group_ids: [12] };
    put.mockResolvedValueOnce({ data: payload });

    await expect(updateCodexSessionAffinitySettings(payload)).resolves.toEqual(
      payload,
    );
    expect(put).toHaveBeenCalledWith(
      "/admin/settings/codex-session-affinity",
      payload,
    );
  });
});
