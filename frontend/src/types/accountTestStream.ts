/**
 * Account connectivity test stream contract (`POST /admin/accounts/:id/test`).
 *
 * The backend emits Server-Sent Events. Older responses only carried `model`;
 * the current contract reports what was requested, what was sent upstream and
 * what the upstream response declared, plus the prompt/effort that were
 * actually applied and whether the response was streamed or arrived whole.
 * Every metadata field is optional: a field a run does not report is shown as
 * "not provided" instead of being guessed.
 */

import type { ClaudeModel } from '@/types'

/** How the upstream response reached the admin UI. */
export type AccountTestStreamMode = 'stream' | 'native_non_stream'

/** Metadata attached to `test_start` / `test_metadata` events. */
export interface AccountTestMetadata {
  /** Model the operator asked for (dropdown selection or platform default). */
  requested_model?: string
  /** Model actually sent to the upstream provider after mapping/effort rules. */
  upstream_sent_model?: string
  /** Model the upstream response itself declared, when it declares one. */
  upstream_response_model?: string
  /** True when the response model differs from the model sent upstream. */
  upstream_response_model_conflict?: boolean
  /** Prompt text the test actually sent. */
  effective_prompt?: string
  /** Reasoning effort the test actually applied. */
  effective_effort?: string
  /** `stream` or `native_non_stream`; unknown values stay verbatim. */
  stream_mode?: string
  /** Legacy upstream model field, still honoured so older runs stay readable. */
  model?: string
}

/** A single decoded SSE event from the test stream. */
export interface AccountTestStreamEvent extends AccountTestMetadata {
  type: string
  text?: string
  success?: boolean
  error?: string
  image_url?: string
  audio_url?: string
  video_url?: string
  mime_type?: string
}

/** Request body accepted by the test endpoint. */
export interface AccountTestStreamRequest {
  model_id: string
  prompt: string
  mode?: string
  /** Optional; omitted entirely when the operator keeps the provider default. */
  reasoning_effort?: string
  image_data_url?: string
  audio_data_url?: string
}

/** Model entry as returned by the account model list, including optional capability metadata. */
export interface AccountTestModelOption extends ClaudeModel {
  /**
   * The list endpoint is provider-specific, and the select passes entries
   * through as open records, so unknown capability fields stay addressable.
   */
  [key: string]: unknown
  /**
   * Reasoning levels published for this model. Accepts `string[]` or
   * `{ effort?: string }[]` so the modal keeps working while the backend
   * capability contract is still being rolled out.
   */
  supported_reasoning_levels?: unknown
  /** Level used when a request does not ask for one. */
  default_reasoning_level?: unknown
}
