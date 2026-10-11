import { describe, expect, it } from "vitest";

import en from "../locales/en";
import zh from "../locales/zh";

const enBlock = () => en.admin.settings.antigravityThoughtPolicy;
const zhBlock = () => zh.admin.settings.antigravityThoughtPolicy;

describe("antigravity thought policy locale copy", () => {
  it("defines the same keys in both locales", () => {
    expect(Object.keys(enBlock()).sort()).toEqual(Object.keys(zhBlock()).sort());
    expect(Object.keys(enBlock())).toEqual(
      expect.arrayContaining([
        "title",
        "description",
        "suppressLabel",
        "suppressHint",
        "continueLabel",
        "continueHint",
        "independenceHint",
        "stateOn",
        "stateOff",
        "affectsHint",
        "save",
        "saved",
        "loadFailed",
        "saveFailed",
      ]),
    );
  });

  it("names the two switches concretely without A/B labels", () => {
    expect(zhBlock().suppressLabel).toBe("抑制纯思考完成信号");
    expect(zhBlock().continueLabel).toBe("网关自动续写");
    expect(enBlock().suppressLabel).toContain("thinking-only completion signal");
    expect(enBlock().continueLabel).toContain("continuation");
    for (const copy of [JSON.stringify(enBlock()), JSON.stringify(zhBlock())]) {
      expect(copy).not.toMatch(/\bA\b/);
      expect(copy).not.toMatch(/\bB\b/);
    }
  });

  it("states the single-attempt, no-added-prompt limit of the continuation", () => {
    expect(zhBlock().continueHint).toContain("最多一次");
    expect(zhBlock().continueHint).toContain("不新增");
    expect(enBlock().continueHint).toContain("at most one attempt");
    expect(enBlock().continueHint).toContain("no added prompt text");
    // What happens to an exhausted thinking-only retry depends on the other switch, so
    // this hint must not promise a normal finish for it.
    expect(zhBlock().continueHint).not.toContain("正常结束");
    expect(enBlock().continueHint).not.toContain("ends normally");
    expect(zhBlock().continueHint).not.toContain("补发两次");
  });

  it("explains independence and the off default in one line", () => {
    expect(zhBlock().independenceHint).toBe("只开启抑制不会触发续写；两个都关闭时保持现有行为。");
    expect(enBlock().independenceHint).toContain(
      "Suppression alone never triggers a continuation",
    );
  });
});
