import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put },
}));

import {
  getGrokOAuthHttp5xxCooldownSettings,
  updateGrokOAuthHttp5xxCooldownSettings,
  type GrokOAuthHttp5xxCooldownSettings,
} from "@/api/admin/settings";

describe("admin Grok OAuth HTTP 5xx cooldown API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("reads the effective setting from the dedicated endpoint", async () => {
    const response: GrokOAuthHttp5xxCooldownSettings = {
      enabled: true,
      cooldown_seconds: 120,
      source: "startup_config",
    };
    get.mockResolvedValueOnce({ data: response });

    await expect(getGrokOAuthHttp5xxCooldownSettings()).resolves.toEqual(
      response,
    );
    expect(get).toHaveBeenCalledWith(
      "/admin/settings/grok-oauth-http-5xx-cooldown",
    );
  });

  it("writes only enabled and cooldown_seconds and returns the effective source", async () => {
    const payload = { enabled: false, cooldown_seconds: 45 };
    const response: GrokOAuthHttp5xxCooldownSettings = {
      ...payload,
      source: "runtime_setting",
    };
    put.mockResolvedValueOnce({ data: response });

    await expect(
      updateGrokOAuthHttp5xxCooldownSettings(payload),
    ).resolves.toEqual(response);
    expect(put).toHaveBeenCalledWith(
      "/admin/settings/grok-oauth-http-5xx-cooldown",
      payload,
    );
  });
});
