/**
 * Antigravity model effort settings.
 *
 * `levels` is the globally allowed set of reasoning effort names referenced by
 * `target-{effort}` model base targets; `default_effort` is used when a request
 * does not ask for a specific name. Names are free-form tokens, not a fixed enum,
 * so future names can be added without a frontend release.
 */
export interface AntigravityModelEffortSettings {
  levels: string[];
  default_effort: string;
}

/** Lowercase token: a letter, then alphanumeric/hyphen groups (e.g. `ultra`, `extra-low`). */
export const ANTIGRAVITY_EFFORT_NAME_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;

/** Mirrors the backend validation bounds so the form can reject invalid input before sending. */
export const ANTIGRAVITY_EFFORT_MAX_LEVELS = 64;
export const ANTIGRAVITY_EFFORT_MAX_NAME_LENGTH = 64;
