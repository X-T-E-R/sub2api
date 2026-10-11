import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getModelTestPromptSettings,
  updateModelTestPromptSettings,
  type ModelTestPromptSettings,
} from "@/api/admin/settings";

describe("admin model test prompt API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads the stored prompt and the built-in default from the dedicated endpoint", async () => {
    const response: ModelTestPromptSettings = {
      prompt: "Reply with the single word: hi",
      default_prompt: "hi",
    };
    get.mockResolvedValueOnce({ data: response });

    await expect(getModelTestPromptSettings()).resolves.toEqual(response);
    expect(get).toHaveBeenCalledWith("/admin/settings/model-test");
  });

  it("writes the prompt, and an empty prompt requests the built-in default", async () => {
    put.mockResolvedValueOnce({ data: { prompt: "", default_prompt: "hi" } });

    await expect(updateModelTestPromptSettings("")).resolves.toEqual({
      prompt: "",
      default_prompt: "hi",
    });
    expect(put).toHaveBeenCalledWith("/admin/settings/model-test", { prompt: "" });
  });
});
