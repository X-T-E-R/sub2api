/**
 * Antigravity thought-only policy (Task24).
 *
 * Independent response switches, all off in source:
 * - `suppress_thought_only_stop` (A): a response that produced thinking only, with no
 *   visible text or tool call, keeps its thinking/signature/usage but does not emit the
 *   successful terminal signal, so the downstream client applies its own policy.
 * - `continue_thought_only` (B): the gateway resends the received thinking/signature as
 *   the assistant tail exactly once, with no added system/user text.
 *
 * - `continue_zero_output`: global-only, appends one user Continue. after clean EOF
 *   with no actual output and no refusal/error terminal. Empty carriers and usage
 *   metadata are not actual content. The single extra-call budget is shared with B.
 *
 * All switches persist server-side in one settings key. Accounts may override only
 * A/B through Account.Extra; missing/null inherits and explicit false wins.
 */
export interface AntigravityThoughtPolicySettings {
  suppress_thought_only_stop: boolean;
  continue_thought_only: boolean;
  /** Global only: one user Continue. after a completed, strictly zero-output response. */
  continue_zero_output: boolean;
}

/** The legacy A/B keys are the only switches with account overrides. */
export type AntigravityThoughtPolicyKey =
  | "suppress_thought_only_stop"
  | "continue_thought_only";

/** Source default: all switches off, preserving existing behavior. */
export const ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS: Readonly<AntigravityThoughtPolicySettings> =
  Object.freeze({
    suppress_thought_only_stop: false,
    continue_thought_only: false,
    continue_zero_output: false,
  });

/**
 * Flat Account.Extra keys, byte-identical to the Go constants
 * `AntigravitySuppressThoughtOnlyStopExtraKey` / `AntigravityContinueThoughtOnlyExtraKey`.
 */
export const ANTIGRAVITY_THOUGHT_POLICY_EXTRA_KEYS: Readonly<
  Record<AntigravityThoughtPolicyKey, string>
> = Object.freeze({
  suppress_thought_only_stop: "antigravity_suppress_thought_only_stop",
  continue_thought_only: "antigravity_continue_thought_only",
});

/** Account-level choice for one switch: inherit the global value, or force it on/off. */
export type AntigravityThoughtPolicyOverride = "inherit" | "on" | "off";

/**
 * Reads a stored account value the same way the backend does: only a real boolean or a
 * boolean-looking string is authoritative, everything else (including JSON null) inherits.
 */
function readOverrideFlag(raw: unknown): boolean | null {
  if (typeof raw === "boolean") return raw;
  if (typeof raw === "string") {
    const normalized = raw.trim().toLowerCase();
    if (normalized === "1" || normalized === "t" || normalized === "true") return true;
    if (normalized === "0" || normalized === "f" || normalized === "false") return false;
  }
  return null;
}

/** Normalizes an API payload; anything that is not a real boolean true reads as off. */
export function normalizeAntigravityThoughtPolicySettings(
  value: unknown,
): AntigravityThoughtPolicySettings {
  const source = (value ?? {}) as Partial<Record<keyof AntigravityThoughtPolicySettings, unknown>>;
  return {
    suppress_thought_only_stop: source.suppress_thought_only_stop === true,
    continue_thought_only: source.continue_thought_only === true,
    continue_zero_output: source.continue_zero_output === true,
  };
}

export function readAntigravityThoughtPolicyOverride(
  extra: Record<string, unknown> | null | undefined,
  key: AntigravityThoughtPolicyKey,
): AntigravityThoughtPolicyOverride {
  const value = readOverrideFlag(extra ? extra[ANTIGRAVITY_THOUGHT_POLICY_EXTRA_KEYS[key]] : null);
  if (value === null) return "inherit";
  return value ? "on" : "off";
}

/**
 * Returns a copy of the account extra with the selected switch set to the chosen
 * tri-state. Inheriting removes the key entirely so the account keeps inheriting if the
 * global switch changes later. Other extra keys are preserved as-is.
 */
export function setAntigravityThoughtPolicyOverride(
  extra: Record<string, unknown> | null | undefined,
  key: AntigravityThoughtPolicyKey,
  override: AntigravityThoughtPolicyOverride,
): Record<string, unknown> {
  const next: Record<string, unknown> = { ...(extra ?? {}) };
  const extraKey = ANTIGRAVITY_THOUGHT_POLICY_EXTRA_KEYS[key];
  if (override === "inherit") {
    delete next[extraKey];
  } else {
    next[extraKey] = override === "on";
  }
  return next;
}

/**
 * Effective values an account actually runs with: the global switch unless the account
 * states otherwise. `global` must be a successfully read global pair; callers that could
 * not read it must show the inherited value as unknown instead of assuming off.
 */
export function resolveAntigravityThoughtPolicy(
  extra: Record<string, unknown> | null | undefined,
  global: AntigravityThoughtPolicySettings,
): AntigravityThoughtPolicySettings {
  const resolved = normalizeAntigravityThoughtPolicySettings(global);
  for (const key of Object.keys(
    ANTIGRAVITY_THOUGHT_POLICY_EXTRA_KEYS,
  ) as AntigravityThoughtPolicyKey[]) {
    const override = readAntigravityThoughtPolicyOverride(extra, key);
    if (override !== "inherit") {
      resolved[key] = override === "on";
    }
  }
  return resolved;
}
