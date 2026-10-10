import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getAntigravityModelEffortSettings,
  updateAntigravityModelEffortSettings,
  type AntigravityModelEffortSettings,
} from "@/api/admin/settings";

describe("admin antigravity model effort API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads effort names from the dedicated endpoint", async () => {
    const response: AntigravityModelEffortSettings = {
      levels: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
      default_effort: "medium",
    };
    get.mockResolvedValueOnce({ data: response });

    await expect(getAntigravityModelEffortSettings()).resolves.toEqual(response);
    expect(get).toHaveBeenCalledWith(
      "/admin/settings/antigravity-model-effort",
    );
  });

  it("writes both levels and the default effort to the dedicated endpoint", async () => {
    const payload: AntigravityModelEffortSettings = {
      levels: ["medium", "ultra", "extra-low"],
      default_effort: "ultra",
    };
    put.mockResolvedValueOnce({ data: payload });

    await expect(updateAntigravityModelEffortSettings(payload)).resolves.toEqual(
      payload,
    );
    expect(put).toHaveBeenCalledWith(
      "/admin/settings/antigravity-model-effort",
      payload,
    );
  });
});
