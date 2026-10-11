import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getAntigravityThoughtPolicySettings,
  updateAntigravityThoughtPolicySettings,
  type AntigravityThoughtPolicySettings,
} from "@/api/admin/antigravityThoughtPolicy";

describe("admin antigravity thought policy API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads the global switch pair from the dedicated endpoint", async () => {
    const response: AntigravityThoughtPolicySettings = {
      suppress_thought_only_stop: true,
      continue_thought_only: false,
    };
    get.mockResolvedValueOnce({ data: response });

    await expect(getAntigravityThoughtPolicySettings()).resolves.toEqual(response);
    expect(get).toHaveBeenCalledWith("/admin/settings/antigravity-thought-policy");
  });

  it("writes both switches to the dedicated endpoint and returns the readback", async () => {
    const payload: AntigravityThoughtPolicySettings = {
      suppress_thought_only_stop: false,
      continue_thought_only: true,
    };
    put.mockResolvedValueOnce({ data: payload });

    await expect(updateAntigravityThoughtPolicySettings(payload)).resolves.toEqual(
      payload,
    );
    expect(put).toHaveBeenCalledWith("/admin/settings/antigravity-thought-policy", {
      suppress_thought_only_stop: false,
      continue_thought_only: true,
    });
  });
});
