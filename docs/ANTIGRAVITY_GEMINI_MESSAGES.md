# Antigravity Gemini Messages Compatibility

Messages requests routed through native Antigravity accounts to Gemini models use turn compatibility by default. An assistant text prefill stays in the conversation, followed by a marked user instruction to continue it without repeating the existing text. A real final user message receives no continuation instruction.

Continuation is instruction-based: a model can still repeat a prefix or change its formatting. For clients that require token-exact assistant-prefill completion, disable this policy or restrict it to models whose output you have checked.

## Configure Model Scope

Use `config.yaml` to select exact final model IDs:

```yaml
gateway:
  antigravity_gemini_messages:
    disabled: false
    models: [gemini-3.8-flash]
```

`models: []` applies to all `gemini-` targets. Matching is case-sensitive and happens after account model mapping and the existing web-search model fallback. Other model families and the Responses, Chat Completions, and native Gemini routes retain their existing conversion behavior. Upstream passthrough accounts also retain their existing behavior.

Environment variables override YAML:

```sh
GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_DISABLED=false
GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_MODELS=gemini-3.8-flash,gemini-2.5-flash
```

Set `disabled: true` or `GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_DISABLED=true` to restore legacy Messages conversion. Restart Sub2API after changing these settings.

## Send Content And Tool Results

Substantive text keeps its original whitespace; the literal text `(no content)` is retained. An empty or whitespace-only final user message becomes a marked `[sub2api:empty-user]` placeholder. Assistant-prefill continuations carry the separate `[sub2api:assistant-prefill]` marker.

Send images with `source.type: base64`, `source.media_type`, and `source.data`. For Gemini 3+ final targets, base64 images inside a tool result are attached to the matching `functionResponse.parts`, preserving image order, bytes, MIME type, call ID, and function name. Text and error status stay attached to that function response.

Image-tool support has a narrower model range than the Gemini-wide tail policy: older or unversioned Gemini targets return a precise capability error for image tool results. Select a Gemini 3+ final target to use image-returning tools.

Terminal URL images, documents, unknown content blocks, and unsupported tool-result blocks return `400 invalid_request_error` with an input location such as `messages[2].content[0]`. This also applies when an empty assistant envelope follows that user input. Supply image bytes as base64 and document text as ordinary text blocks.

Text tool results keep their call ID, function name, and output. Empty output stays empty, including image-only results without text; `is_error: true` is sent as an error in the Gemini function response.

Before adding a synthetic continuation or empty-user turn, Sub2API requires complete tool results and supported history. Return each actual result under its original `tool_use_id`, resolve duplicate or mismatched IDs, and send a user message after a media-only or thinking-only assistant tail.
