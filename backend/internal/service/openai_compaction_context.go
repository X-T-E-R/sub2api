package service

import "context"

type openAIForwardModelContextKey struct{}

type openAIForwardModel struct {
	model                  string
	useCompactModelMapping bool
}

// WithOpenAIForwardModel records the model present in the forwarded request
// body after channel mapping and whether the legacy /responses/compact-only
// model mapping applies. Native remote compaction v2 keeps this false, so
// channel restriction checks follow the same model chain used by Forward.
func WithOpenAIForwardModel(ctx context.Context, forwardModel string, useCompactModelMapping bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIForwardModelContextKey{}, openAIForwardModel{
		model:                  forwardModel,
		useCompactModelMapping: useCompactModelMapping,
	})
}

func openAIForwardModelFromContext(ctx context.Context) (openAIForwardModel, bool) {
	if ctx == nil {
		return openAIForwardModel{}, false
	}
	forwardModel, ok := ctx.Value(openAIForwardModelContextKey{}).(openAIForwardModel)
	return forwardModel, ok
}

// isOpenAIAccountModelSupportedForContext checks whether an account can serve
// either the client's requestedModel or the channel-mapped forward model
// recorded via WithOpenAIForwardModel. Checking both preserves legacy accounts
// whose whitelist keys match the client-facing alias while also allowing
// accounts whose model_mapping keys match the channel-mapped target sent to
// Forward.
func isOpenAIAccountModelSupportedForContext(ctx context.Context, account *Account, requestedModel string) bool {
	if requestedModel == "" {
		return true
	}
	if account == nil {
		return false
	}
	if account.IsModelSupported(requestedModel) {
		return true
	}
	if fwd, ok := openAIForwardModelFromContext(ctx); ok && fwd.model != "" && fwd.model != requestedModel {
		return account.IsModelSupported(fwd.model)
	}
	return false
}
