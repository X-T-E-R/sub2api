import { describe, expect, it } from "vitest";

import {
  ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS,
  ANTIGRAVITY_THOUGHT_POLICY_EXTRA_KEYS,
  readAntigravityThoughtPolicyOverride,
  resolveAntigravityThoughtPolicy,
  setAntigravityThoughtPolicyOverride,
  type AntigravityThoughtPolicySettings,
} from "../antigravityThoughtPolicy";

const SUPPRESS_KEY = "antigravity_suppress_thought_only_stop";
const CONTINUE_KEY = "antigravity_continue_thought_only";

describe("antigravity thought policy contract", () => {
  it("keeps the flat extra keys the gateway reads", () => {
    expect(ANTIGRAVITY_THOUGHT_POLICY_EXTRA_KEYS).toEqual({
      suppress_thought_only_stop: SUPPRESS_KEY,
      continue_thought_only: CONTINUE_KEY,
    });
    expect(ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS).toEqual({
      suppress_thought_only_stop: false,
      continue_thought_only: false,
    });
  });

  it("treats only real booleans and boolean strings as an account decision", () => {
    expect(readAntigravityThoughtPolicyOverride({}, "continue_thought_only")).toBe("inherit");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: null }, "continue_thought_only"),
    ).toBe("inherit");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: 1 }, "continue_thought_only"),
    ).toBe("inherit");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: "yes" }, "continue_thought_only"),
    ).toBe("inherit");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: true }, "continue_thought_only"),
    ).toBe("on");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: " TRUE " }, "continue_thought_only"),
    ).toBe("on");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: false }, "continue_thought_only"),
    ).toBe("off");
    expect(
      readAntigravityThoughtPolicyOverride({ [CONTINUE_KEY]: "0" }, "continue_thought_only"),
    ).toBe("off");
  });

  it("resolves each switch independently and lets an explicit false win", () => {
    const globalOn: AntigravityThoughtPolicySettings = {
      suppress_thought_only_stop: true,
      continue_thought_only: true,
    };

    expect(resolveAntigravityThoughtPolicy({}, globalOn)).toEqual(globalOn);
    expect(
      resolveAntigravityThoughtPolicy({ [SUPPRESS_KEY]: false }, globalOn),
    ).toEqual({ suppress_thought_only_stop: false, continue_thought_only: true });
    expect(
      resolveAntigravityThoughtPolicy({ [CONTINUE_KEY]: false }, globalOn),
    ).toEqual({ suppress_thought_only_stop: true, continue_thought_only: false });
    expect(
      resolveAntigravityThoughtPolicy(
        { [SUPPRESS_KEY]: false, [CONTINUE_KEY]: false },
        globalOn,
      ),
    ).toEqual(ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS);
    expect(
      resolveAntigravityThoughtPolicy(
        {},
        { suppress_thought_only_stop: false, continue_thought_only: false },
      ),
    ).toEqual(ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS);
    expect(
      resolveAntigravityThoughtPolicy({ [CONTINUE_KEY]: true }, ANTIGRAVITY_THOUGHT_POLICY_DEFAULTS),
    ).toEqual({ suppress_thought_only_stop: false, continue_thought_only: true });
  });

  it("writes a tri-state without touching other account extra keys", () => {
    const extra = { mixed_scheduling: true, [SUPPRESS_KEY]: true };

    const off = setAntigravityThoughtPolicyOverride(extra, "suppress_thought_only_stop", "off");
    expect(off).toEqual({ mixed_scheduling: true, [SUPPRESS_KEY]: false });
    expect(extra).toEqual({ mixed_scheduling: true, [SUPPRESS_KEY]: true });

    const inherit = setAntigravityThoughtPolicyOverride(off, "suppress_thought_only_stop", "inherit");
    expect(inherit).toEqual({ mixed_scheduling: true });
    expect(SUPPRESS_KEY in inherit).toBe(false);

    const on = setAntigravityThoughtPolicyOverride(null, "continue_thought_only", "on");
    expect(on).toEqual({ [CONTINUE_KEY]: true });
  });
});
