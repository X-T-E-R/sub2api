import type { AntigravityThoughtPolicyOverride } from "@/types/antigravityThoughtPolicy";

/**
 * Copy for the account-level three-state control.
 *
 * Kept with the component so the tri-state labels and their explanations change
 * together. The explicit shape keeps both locales key-identical at compile time; a
 * type alias (not an interface) also keeps it assignable to vue-i18n's message type.
 */
export type AntigravityThoughtPolicyMessages = {
  title: string;
  retry: string;
  loadingGlobal: string;
  globalUnknown: string;
  choices: Record<AntigravityThoughtPolicyOverride, string>;
  effectiveLabel: string;
  effective: {
    onOverride: string;
    offOverride: string;
    inheritedOn: string;
    inheritedOff: string;
    unknown: string;
  };
  suppressLabel: string;
  suppressHint: string;
  continueLabel: string;
  continueHint: string;
};

export const ANTIGRAVITY_THOUGHT_POLICY_MESSAGES: Record<
  "en" | "zh",
  AntigravityThoughtPolicyMessages
> = {
  en: {
    title: "Thought-only responses",
    retry: "Retry",
    loadingGlobal: "Loading the global settings…",
    globalUnknown:
      "The global setting could not be read, so inherited values are unknown; the gateway still resolves them against it.",
    choices: {
      inherit: "Inherit",
      on: "On",
      off: "Off",
    },
    effectiveLabel: "Effective:",
    effective: {
      onOverride: "On — set by this account",
      offOverride: "Off — set by this account",
      inheritedOn: "On — inherited from the global setting",
      inheritedOff: "Off — inherited from the global setting",
      unknown: "Unknown — global value not loaded",
    },
    suppressLabel: "Suppress the thinking-only completion signal",
    suppressHint:
      "Keeps thinking, signature and usage without reporting a successful finish.",
    continueLabel: "Automatic gateway continuation",
    continueHint:
      "Resends the received thinking and signature once, with no added prompt text.",
  },
  zh: {
    title: "纯思考响应",
    retry: "重试",
    loadingGlobal: "正在读取全局设置……",
    globalUnknown: "未读取到全局设置，继承的生效值未知；网关仍按全局值解析。",
    choices: {
      inherit: "继承全局",
      on: "开启",
      off: "关闭",
    },
    effectiveLabel: "生效：",
    effective: {
      onOverride: "开启（本账号设置）",
      offOverride: "关闭（本账号设置）",
      inheritedOn: "开启（继承全局）",
      inheritedOff: "关闭（继承全局）",
      unknown: "未知（未读取到全局值）",
    },
    suppressLabel: "抑制纯思考完成信号",
    suppressHint: "保留思考、签名与用量，但不报告成功完成。",
    continueLabel: "网关自动续写",
    continueHint: "把收到的思考与签名补发一次，不新增提示词文本。",
  },
};
