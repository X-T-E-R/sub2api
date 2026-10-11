import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";

const apiMocks = vi.hoisted(() => ({
  getAntigravityThoughtPolicySettings: vi.fn(),
}));

vi.mock("@/api/admin/antigravityThoughtPolicy", () => apiMocks);

const locale = vi.hoisted(() => ({ current: "en" as "en" | "zh" }));

// The repo aliases vue-i18n to its runtime-only build and turns on JIT message
// compilation only in the app build, so component tests translate through the real
// copy objects: a typo or a missing key renders the key itself and fails the test.
vi.mock("vue-i18n", async () => {
  const { ANTIGRAVITY_THOUGHT_POLICY_MESSAGES } = await import(
    "@/components/account/antigravityThoughtPolicyMessages"
  );
  return {
    useI18n: () => ({
      t: (key: string): string => {
        let node: unknown = ANTIGRAVITY_THOUGHT_POLICY_MESSAGES[locale.current];
        for (const part of key.split(".")) {
          node =
            typeof node === "object" && node !== null
              ? (node as Record<string, unknown>)[part]
              : undefined;
        }
        return typeof node === "string" ? node : key;
      },
    }),
  };
});

import AntigravityThoughtPolicyOverride from "../AntigravityThoughtPolicyOverride.vue";

const SUPPRESS_KEY = "antigravity_suppress_thought_only_stop";
const CONTINUE_KEY = "antigravity_continue_thought_only";

function optionTestId(
  key: "suppress_thought_only_stop" | "continue_thought_only",
  choice: "inherit" | "on" | "off",
): string {
  return `[data-testid="antigravity-thought-policy-override-${key}-${choice}"]`;
}

function effectiveTestId(
  key: "suppress_thought_only_stop" | "continue_thought_only",
): string {
  return `[data-testid="antigravity-thought-policy-override-${key}-effective"]`;
}

function isChecked(wrapper: VueWrapper, testId: string): boolean {
  return wrapper.get(testId).attributes("aria-checked") === "true";
}

function emittedExtra(wrapper: VueWrapper): Record<string, unknown> {
  const emitted = wrapper.emitted("update:modelValue");
  expect(emitted).toBeTruthy();
  return emitted![emitted!.length - 1][0] as Record<string, unknown>;
}

describe("AntigravityThoughtPolicyOverride", () => {
  beforeEach(() => {
    apiMocks.getAntigravityThoughtPolicySettings.mockReset();
    locale.current = "en";
  });

  it("shows the inherited global value when the account states nothing", async () => {
    const wrapper = mount(AntigravityThoughtPolicyOverride, {
      props: {
        modelValue: {},
        globalPolicy: { suppress_thought_only_stop: true, continue_thought_only: false },
      },
    });
    await flushPromises();

    expect(apiMocks.getAntigravityThoughtPolicySettings).not.toHaveBeenCalled();
    expect(
      isChecked(wrapper, optionTestId("suppress_thought_only_stop", "inherit")),
    ).toBe(true);
    expect(
      isChecked(wrapper, optionTestId("continue_thought_only", "inherit")),
    ).toBe(true);
    expect(wrapper.get(effectiveTestId("suppress_thought_only_stop")).text()).toContain(
      "On — inherited from the global setting",
    );
    expect(wrapper.get(effectiveTestId("continue_thought_only")).text()).toContain(
      "Off — inherited from the global setting",
    );
  });

  it("lets an explicit account value override the global value and keeps other extra keys", async () => {
    const wrapper = mount(AntigravityThoughtPolicyOverride, {
      props: {
        modelValue: { [SUPPRESS_KEY]: false, mixed_scheduling: true },
        globalPolicy: { suppress_thought_only_stop: true, continue_thought_only: true },
      },
    });
    await flushPromises();

    expect(isChecked(wrapper, optionTestId("suppress_thought_only_stop", "off"))).toBe(true);
    expect(wrapper.get(effectiveTestId("suppress_thought_only_stop")).text()).toContain(
      "Off — set by this account",
    );

    await wrapper.get(optionTestId("suppress_thought_only_stop", "on")).trigger("click");

    expect(emittedExtra(wrapper)).toEqual({
      [SUPPRESS_KEY]: true,
      mixed_scheduling: true,
    });
  });

  it("removes the override key when the account switches back to inheriting", async () => {
    const wrapper = mount(AntigravityThoughtPolicyOverride, {
      props: {
        modelValue: { [CONTINUE_KEY]: true },
        globalPolicy: { suppress_thought_only_stop: false, continue_thought_only: false },
      },
    });
    await flushPromises();

    expect(isChecked(wrapper, optionTestId("continue_thought_only", "on"))).toBe(true);
    expect(wrapper.get(effectiveTestId("continue_thought_only")).text()).toContain(
      "On — set by this account",
    );

    await wrapper.get(optionTestId("continue_thought_only", "inherit")).trigger("click");
    const next = emittedExtra(wrapper);
    expect(CONTINUE_KEY in next).toBe(false);

    // The parent hands the emitted extra back through v-model: the row now inherits.
    await wrapper.setProps({ modelValue: next });
    expect(isChecked(wrapper, optionTestId("continue_thought_only", "inherit"))).toBe(true);
    expect(wrapper.get(effectiveTestId("continue_thought_only")).text()).toContain(
      "Off — inherited from the global setting",
    );
  });

  it("keeps inherited values visibly unknown when the global pair cannot be read", async () => {
    apiMocks.getAntigravityThoughtPolicySettings.mockRejectedValueOnce(
      new Error("offline"),
    );

    const wrapper = mount(AntigravityThoughtPolicyOverride, {
      props: { modelValue: {} },
    });
    await flushPromises();

    expect(
      wrapper
        .find('[data-testid="antigravity-thought-policy-override-global-unknown"]')
        .exists(),
    ).toBe(true);
    expect(wrapper.get(effectiveTestId("suppress_thought_only_stop")).text()).toContain(
      "Unknown — global value not loaded",
    );

    apiMocks.getAntigravityThoughtPolicySettings.mockResolvedValueOnce({
      suppress_thought_only_stop: true,
      continue_thought_only: false,
    });
    await wrapper
      .get('[data-testid="antigravity-thought-policy-override-global-retry"]')
      .trigger("click");
    await flushPromises();

    expect(apiMocks.getAntigravityThoughtPolicySettings).toHaveBeenCalledTimes(2);
    expect(
      wrapper
        .find('[data-testid="antigravity-thought-policy-override-global-unknown"]')
        .exists(),
    ).toBe(false);
    expect(wrapper.get(effectiveTestId("suppress_thought_only_stop")).text()).toContain(
      "On — inherited from the global setting",
    );
  });

  it("loads the global pair itself and renders the Chinese copy for zh", async () => {
    locale.current = "zh";
    apiMocks.getAntigravityThoughtPolicySettings.mockResolvedValue({
      suppress_thought_only_stop: false,
      continue_thought_only: true,
    });

    const wrapper = mount(AntigravityThoughtPolicyOverride, {
      props: { modelValue: {} },
    });
    await flushPromises();

    expect(apiMocks.getAntigravityThoughtPolicySettings).toHaveBeenCalledTimes(1);
    expect(wrapper.text()).toContain("继承全局");
    expect(wrapper.get(effectiveTestId("continue_thought_only")).text()).toContain(
      "开启（继承全局）",
    );
  });

  it("blocks changes while the account is being saved", async () => {
    const wrapper = mount(AntigravityThoughtPolicyOverride, {
      props: {
        modelValue: {},
        globalPolicy: { suppress_thought_only_stop: false, continue_thought_only: false },
        disabled: true,
      },
    });
    await flushPromises();

    await wrapper.get(optionTestId("suppress_thought_only_stop", "on")).trigger("click");

    expect(wrapper.emitted("update:modelValue")).toBeFalsy();
  });
});
