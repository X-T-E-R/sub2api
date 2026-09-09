import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getModelReasoningFloorSettings,
  updateModelReasoningFloorSettings,
  type ModelReasoningFloorSettings,
} from "@/api/admin/settings";

describe("admin model reasoning floor API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads model floors from the dedicated endpoint", async () => {
    const response: ModelReasoningFloorSettings = {
      enabled: true,
      rules: [{ model: "gpt-5.6-sol", min_effort: "high" }],
    };
    get.mockResolvedValueOnce({ data: response });

    await expect(getModelReasoningFloorSettings()).resolves.toEqual(response);
    expect(get).toHaveBeenCalledWith("/admin/settings/model-reasoning-floor");
  });

  it("writes both enabled and rules to the dedicated endpoint", async () => {
    const payload: ModelReasoningFloorSettings = {
      enabled: false,
      rules: [{ model: "o3", min_effort: "medium" }],
    };
    put.mockResolvedValueOnce({ data: payload });

    await expect(updateModelReasoningFloorSettings(payload)).resolves.toEqual(
      payload,
    );
    expect(put).toHaveBeenCalledWith(
      "/admin/settings/model-reasoning-floor",
      payload,
    );
  });
});
