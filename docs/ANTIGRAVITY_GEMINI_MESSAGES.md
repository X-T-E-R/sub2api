# Antigravity Gemini Messages Compatibility

Messages requests routed through native Antigravity accounts to Gemini models use turn compatibility by default. An assistant text prefill stays in the conversation, followed by a marked user instruction to continue it without repeating the existing text. A real final user message receives no continuation instruction.

Text-prefill continuation is instruction-based: a model can still repeat a prefix or change its formatting. For clients that require token-exact assistant-prefill completion, disable this policy or restrict it to models whose output you have checked. Thinking-only continuation is a separate policy and adds no continuation instruction.

## Recover Thinking-Only And Empty Responses

Two independent switches control native Antigravity responses without a usable answer:

- `suppress_thought_only_stop`: after clean upstream EOF, report a retryable provider error for an empty or thinking-only normal STOP, or actual thinking with an absent or `FINISH_REASON_UNSPECIFIED` upstream finish reason. Retain any thinking, signatures and usage, and omit successful completion. Messages streams send an explicit `api_error`; Chat Completions streams send `server_error`; Responses streams end with `response.failed`; native Gemini streams report `UNAVAILABLE`. Non-streaming responses use HTTP 502 and retain partial content and usage alongside the error. Enabling this switch alone sends no additional upstream request: the client must retry according to its normal error policy.
- `continue_thought_only`: only for actual thinking with a normal STOP, make at most one additional generation on the same account and resolved model. Empty or missing-terminal responses never trigger this generation. Append the received thinking/signature carriers as the final model turn of the original upstream request, without adding system/user text or blank turns. If it again produces only thinking, the suppression switch decides whether to report successful completion.

Both switches default to `false`. Usability follows the processed protocol: `functionCall:null` is not a tool. Messages, Chat Completions and Responses count grounding only when their shared renderer produces visible text; native Gemini preserves non-Web grounding chunks for its consumers. Visible text, real tools, usable grounding, refusals, length limits, read errors and cancellations retain their own semantics. Keep automatic continuation off on endpoints that return `Requests ending with a model turn are not supported.`: that rule rejects the final model turn required by zero-prompt continuation. Sub2API does not work around it by dropping thinking/signatures, repeating the original user request, or adding a user turn. Completion-signal suppression remains independent and can be used without automatic continuation. Local fixtures establish request construction, not upstream acceptance.

Administrators can `GET` or `PUT` `/api/v1/admin/settings/antigravity-thought-policy`. PUT requires the complete pair of booleans. The single persistent Settings key is `antigravity_thought_policy`. For example, suppress successful thinking-only completion without issuing another generation:

```json
{
  "suppress_thought_only_stop": true,
  "continue_thought_only": false
}
```

An unusable-answer failure keeps its real partial usage billable and does not retry or switch accounts. Its Ops event is `no_usable_answer`, with `reason` set to `thinking_only`, `empty`, or `missing_terminal`. A rejected appended generation also returns a non-success response and keeps its partial usage billable; it does not retry or switch accounts. Its Ops event is `thought_continuation_error`, with `stage`, HTTP `upstream_status_code`, bounded provider `message`/`reason` and `upstream_request_id` when the provider supplied one. These diagnostics do not record the raw body, headers, thinking or signature carriers. Use the gateway request ID to correlate the event; neighboring requests are not proof of the same failure.

Native Antigravity response paths emit at most one `antigravity.response_summary` system-log entry per client request. It records downstream thinking/text/tool event counts, upstream finish reason, effective switches, processed protocol, detector flags and the suppression decision or skip reason, with existing gateway/client request IDs when available. Streaming counts describe content events; collected counts describe content items, not tokens or text length. The entry contains no prompt, body, thinking, signature, headers or token values and also covers policy-off responses. Compare the decision and protocol before treating a zero text count as a fault: a native Gemini response may still contain usable grounding metadata. Normal logger retention and filtering apply.

A successful PUT persists then publishes the pair for new requests. In-flight requests keep their initial snapshot. Startup loads the saved pair and logs `key=antigravity_thought_policy source=settings` (or `source=seed`) and both booleans; a GET reads persistent state, not the running snapshot. If changing the key directly in the database, restart the gateway to load it. Before enabling, preserve the key's existence and original JSON; restore that value and restart to roll back a direct database change.

Accounts override each switch independently using boolean Extra fields `antigravity_suppress_thought_only_stop` and `antigravity_continue_thought_only`. Missing or `null` inherits the global value; explicit `false` overrides global `true`. Changing the global pair does not overwrite account overrides.

## Configure Models And Effort

For Antigravity OAuth accounts, put one base entry in the existing model-mapping editor:

```json
{
  "gemini-3.8-flash": "gemini-3.8-flash-{effort}",
  "opus5.5": "opus5.5-{effort}",
  "custom-model": "custom-model-{effort}"
}
```

A template explicitly opts that model into suffix routing; it is not restricted to Gemini. Configure a template only when its target uses this naming convention. These examples demonstrate configuration syntax, not account availability. Literal targets retain their existing behavior, and other providers do not expand Antigravity templates.

Send the level in Chat Completions `reasoning_effort`, Responses `reasoning.effort`, Messages `output_config.effort`, or native Gemini `generationConfig.thinkingConfig.thinkingLevel`. A base request with `low`, `medium`, or `high` selects the corresponding upstream ID. Clients may also send the complete `base-level` ID; its level wins over a conflicting body field. Thinking budgets are preserved without guessing a model level.

### Set Global Levels And The Default

In **Settings**, use the **Antigravity model effort names** card beside the model reasoning floor settings to edit the shared names and default, then save. Authenticated administrators can also `GET` or `PUT` `/api/v1/admin/settings/antigravity-model-effort`. The persistent Settings key is `antigravity_model_effort`; update the complete object:

```json
{
  "levels": ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  "default_effort": "medium"
}
```

This is the initial seed, not a capability ceiling. Add a future name such as `ultra` or an existing composite name such as `extra-low` here once; model entries do not change. Names are unique lowercase tokens, up to 64 characters, with internal hyphens allowed. The default must be in the list. A successful PUT persists and atomically publishes the new policy for new requests without restarting; an in-flight request retains its original snapshot. Startup loads the saved setting and fails rather than using an invalid saved policy.

A template without a body level uses the global default, initially `medium`. This replaces the old fixed-high default for the new `gemini-3.8-flash` configuration. If a model needs a known naming exception, use one rule such as `gemini-3.5-flash-{effort:low}`. The explicit default must also be globally enabled. Disabled or unknown levels are rejected for template requests; they never silently become `high`.

Exact suffix overrides, literal aliases, and wildcard exceptions remain available when deliberately configured. The new default catalog and migrated short table do not carry obsolete preview/date/cross-model aliases. Keep independent `tiered`, `agent`, `image`, and `lite` IDs as separate entries; they are not stripped as effort names. Back up the old table before rebuilding it so configuration and image rollback can restore the old behavior.

## Configure Messages Compatibility Scope

The Messages conversion policy is separate from the account whitelist. Use `config.yaml` to select exact **final** model IDs, including their effort suffixes:

```yaml
gateway:
  antigravity_gemini_messages:
    disabled: false
    models: [gemini-3.8-flash-high, gemini-3.8-flash-medium]
```

`models: []` applies to all `gemini-` targets. Matching is case-sensitive and happens after account model mapping and the existing web-search model fallback. Other model families and the Responses, Chat Completions, and native Gemini routes retain their existing conversion behavior. Upstream passthrough accounts also retain their existing behavior.

Environment variables override YAML:

```sh
GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_DISABLED=false
GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_MODELS=gemini-3.8-flash-high,gemini-3.8-flash-medium
```

Set `disabled: true` or `GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_DISABLED=true` to restore legacy Messages conversion. Restart Sub2API after changing these settings.

## Send Content And Tool Results

Substantive text keeps its original whitespace; the literal text `(no content)` is retained. An empty or whitespace-only final user message becomes a marked `[sub2api:empty-user]` placeholder. Assistant-prefill continuations carry the separate `[sub2api:assistant-prefill]` marker.

Send images with `source.type: base64`, `source.media_type`, and `source.data`. For Gemini 3+ final targets, base64 images inside a tool result are attached to the matching `functionResponse.parts`, preserving image order, bytes, MIME type, call ID, and function name. Text and error status stay attached to that function response.

Image-tool support has a narrower model range than the Gemini-wide tail policy: older or unversioned Gemini targets return a precise capability error for image tool results. Select a Gemini 3+ final target to use image-returning tools.

Terminal URL images, documents, unknown content blocks, and unsupported tool-result blocks return `400 invalid_request_error` with an input location such as `messages[2].content[0]`. This also applies when an empty assistant envelope follows that user input. Supply image bytes as base64 and document text as ordinary text blocks.

Text tool results keep their call ID, function name, and output. Empty output stays empty, including image-only results without text; `is_error: true` is sent as an error in the Gemini function response.

Before adding a synthetic text-prefill continuation or empty-user turn, Sub2API requires complete tool results and supported history. Return each actual result under its original `tool_use_id` and resolve duplicate or mismatched IDs. A media-only assistant tail requires a user message. With automatic thinking-only continuation enabled, a nonempty thinking-only assistant tail retains its real carriers instead of being trimmed; without it, the existing user-boundary recovery remains.
