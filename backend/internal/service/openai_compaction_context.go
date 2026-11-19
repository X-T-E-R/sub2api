package service

import (
	"context"
	"strings"
)

type openAIForwardModelContextKey struct{}

type openAIForwardModel struct {
	model                            string
	requestedModel                   string
	useCompactModelMapping           bool
	applyAccountMappingOnPassthrough bool
}

// WithOpenAIForwardModel records the model present in the forwarded request
// body after channel mapping and whether the legacy /responses/compact-only
// model mapping applies. Native remote compaction v2 keeps this false, so
// channel restriction checks follow the same model chain used by Forward.
func WithOpenAIForwardModel(ctx context.Context, forwardModel string, useCompactModelMapping bool) context.Context {
	return WithOpenAIForwardModelRoute(ctx, "", forwardModel, useCompactModelMapping)
}

// WithOpenAIForwardModelRoute records both the pre-channel-mapping client
// requested model alias and the post-channel-mapping forward model so account
// selection, channel upstream-model restriction checks, and forwarding all
// resolve account-level model_mapping identically.
func WithOpenAIForwardModelRoute(ctx context.Context, requestedModel, forwardModel string, useCompactModelMapping bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIForwardModelContextKey{}, openAIForwardModel{
		model:                  strings.TrimSpace(forwardModel),
		requestedModel:         strings.TrimSpace(requestedModel),
		useCompactModelMapping: useCompactModelMapping,
	})
}

// WithOpenAIChatForwardModelRoute records the /v1/chat/completions forward model
// route where translated chat forwarding still applies explicit account
// model_mapping even on passthrough-enabled accounts.
func WithOpenAIChatForwardModelRoute(ctx context.Context, requestedModel, forwardModel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIForwardModelContextKey{}, openAIForwardModel{
		model:                            strings.TrimSpace(forwardModel),
		requestedModel:                   strings.TrimSpace(requestedModel),
		applyAccountMappingOnPassthrough: true,
	})
}

func openAIForwardModelFromContext(ctx context.Context) (openAIForwardModel, bool) {
	if ctx == nil {
		return openAIForwardModel{}, false
	}
	forwardModel, ok := ctx.Value(openAIForwardModelContextKey{}).(openAIForwardModel)
	return forwardModel, ok
}

// resolveOpenAIAccountModelForContext is the canonical per-account model
// resolution helper shared across OpenAI account selection, channel upstream
// restriction checks, and forwarding (/v1/responses, raw /v1/chat/completions,
// and chat->responses bridge).
//
// Resolution order when channel mapping rewrote clientAlias -> channelMapped:
//  1. Passthrough accounts always accept and keep channelMapped untouched.
//  2. Accounts with an explicit non-empty model_mapping whitelist match
//     channelMapped FIRST (taking precedence when both keys exist), and fall
//     back to clientAlias when only the client-facing alias is mapped.
//  3. Accounts without a model_mapping whitelist (including OpenAI OAuth
//     accounts) evaluate ONLY channelMapped so OAuth foreign-model checks
//     cannot falsely admit an unsupported channelMapped target via clientAlias.
func resolveOpenAIAccountModelForContext(
	ctx context.Context,
	account *Account,
	modelInput string,
	messagesDispatchMappedModel string,
) (supported bool, effectiveModel string, mappedModel string) {
	modelInput = strings.TrimSpace(modelInput)
	messagesDispatchMappedModel = strings.TrimSpace(messagesDispatchMappedModel)

	effective := modelInput
	var originalAlias string
	if fwd, ok := openAIForwardModelFromContext(ctx); ok {
		if fwd.model != "" {
			effective = fwd.model
		}
		if fwd.requestedModel != "" && fwd.requestedModel != effective {
			originalAlias = fwd.requestedModel
		} else if modelInput != "" && modelInput != effective {
			originalAlias = modelInput
		}
	}

	if effective == "" {
		if messagesDispatchMappedModel != "" {
			return true, "", messagesDispatchMappedModel
		}
		return true, "", ""
	}
	if account == nil {
		if messagesDispatchMappedModel != "" {
			return false, effective, messagesDispatchMappedModel
		}
		return false, effective, effective
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		out := effective
		if messagesDispatchMappedModel != "" {
			out = messagesDispatchMappedModel
		}
		if account.IsOpenAIPassthroughEnabled() && !shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
			return true, effective, out
		}
		return account.IsModelSupported(effective), effective, out
	}

	if mapped, matched := account.ResolveMappedModel(effective); matched {
		mapped = strings.TrimSpace(mapped)
		if mapped == "" {
			mapped = effective
		}
		return true, effective, mapped
	}
	if originalAlias != "" {
		if mapped, matched := account.ResolveMappedModel(originalAlias); matched {
			mapped = strings.TrimSpace(mapped)
			if mapped == "" {
				mapped = originalAlias
			}
			return true, originalAlias, mapped
		}
	}

	out := effective
	if messagesDispatchMappedModel != "" {
		out = messagesDispatchMappedModel
	}
	if account.IsOpenAIPassthroughEnabled() && !shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return true, effective, out
	}
	return false, effective, out
}

func isOpenAIAccountModelSupportedForContext(ctx context.Context, account *Account, requestedModel string) bool {
	if strings.TrimSpace(requestedModel) == "" {
		return true
	}
	supported, _, _ := resolveOpenAIAccountModelForContext(ctx, account, requestedModel, "")
	return supported
}

// IsOpenAIAccountModelSupportedForContext exposes the canonical context-aware
// model support check for multi-turn handlers (such as Responses WebSocket).
func IsOpenAIAccountModelSupportedForContext(ctx context.Context, account *Account, requestedModel string) bool {
	return isOpenAIAccountModelSupportedForContext(ctx, account, requestedModel)
}
