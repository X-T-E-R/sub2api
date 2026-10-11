/**
 * Global prompt settings for the account connectivity test.
 *
 * `prompt` is the operator-configured text; when it is empty the backend falls
 * back to `default_prompt`. Only ordinary text tests use this value — image,
 * video, search, TTS and STT tests keep their own specific input.
 */
export interface ModelTestPromptSettings {
  prompt: string;
  default_prompt: string;
}

/** Backend limit: 8192 Unicode characters. */
export const MODEL_TEST_PROMPT_MAX_CHARS = 8192;

/**
 * Counts Unicode code points, so an astral character (emoji) counts once, the
 * same way the backend counts runes when it enforces the limit.
 */
export function countModelTestPromptChars(value: string): number {
  return Array.from(value).length;
}

/** Truncates to the backend limit without splitting a surrogate pair. */
export function clampModelTestPrompt(value: string, max = MODEL_TEST_PROMPT_MAX_CHARS): string {
  const chars = Array.from(value);
  if (chars.length <= max) return value;
  return chars.slice(0, max).join("");
}
