export const MODEL_REASONING_FLOOR_EFFORTS = [
  "minimal",
  "low",
  "medium",
  "high",
  "xhigh",
  "max",
] as const;

export type ModelReasoningFloorEffort =
  (typeof MODEL_REASONING_FLOOR_EFFORTS)[number];

export interface ModelReasoningFloorRule {
  model: string;
  min_effort: ModelReasoningFloorEffort;
}

export interface ModelReasoningFloorSettings {
  enabled: boolean;
  rules: ModelReasoningFloorRule[];
}
