/**
 * Antigravity thought-only policy endpoints.
 *
 * Independent from the shared admin settings API so the two switches can be wired
 * into the gateway settings card and the account form without touching that file.
 */

import { apiClient } from "../client";
import type { AntigravityThoughtPolicySettings } from "@/types/antigravityThoughtPolicy";

export type { AntigravityThoughtPolicySettings } from "@/types/antigravityThoughtPolicy";

export async function getAntigravityThoughtPolicySettings(): Promise<AntigravityThoughtPolicySettings> {
  const { data } = await apiClient.get<AntigravityThoughtPolicySettings>(
    "/admin/settings/antigravity-thought-policy",
  );
  return data;
}

export async function updateAntigravityThoughtPolicySettings(
  settings: AntigravityThoughtPolicySettings,
): Promise<AntigravityThoughtPolicySettings> {
  const { data } = await apiClient.put<AntigravityThoughtPolicySettings>(
    "/admin/settings/antigravity-thought-policy",
    settings,
  );
  return data;
}
