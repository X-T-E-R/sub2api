# Antigravity Gemini Messages Compatibility

Messages requests routed through native Antigravity accounts to Gemini models use turn compatibility by default. An assistant text prefill stays in the conversation, followed by a marked user instruction to continue it without repeating the existing text. A real final user message receives no continuation instruction.

Continuation is instruction-based: a model can still repeat a prefix or change its formatting. For clients that require token-exact assistant-prefill completion, disable this policy or restrict it to models whose output you have checked.

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

Before adding a synthetic continuation or empty-user turn, Sub2API requires complete tool results and supported history. Return each actual result under its original `tool_use_id`, resolve duplicate or mismatched IDs, and send a user message after a media-only or thinking-only assistant tail.
