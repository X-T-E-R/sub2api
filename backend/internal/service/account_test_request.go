package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const accountTestPromptKey = "account_test_effective_prompt"
const accountTestEffortKey = "account_test_reasoning_effort"
const accountTestRequestedKey = "account_test_requested_model"
const accountTestEffectiveEffortKey = "account_test_effective_effort"

func accountTestEffectivePrompt(c *gin.Context) string {
	if v, ok := c.Get(accountTestPromptKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return DefaultModelTestPrompt
}
func accountTestReasoningEffort(c *gin.Context) string {
	v, _ := c.Get(accountTestEffortKey)
	s, _ := v.(string)
	return s
}

func isOrdinaryModelTest(account *Account, model, mode string) bool {
	if isOpenAIImageModel(model) || isImageGenerationModel(model) {
		return false
	}
	if account.Platform == PlatformGrok {
		mode = normalizeGrokAccountTestMode(mode)
		if mode != AccountTestModeDefault && mode != AccountTestModeGrokText {
			return false
		}
		return !isGrokVideoGenerationModel(model) && !isGrokImageGenerationModel(model)
	}
	return normalizeAccountTestMode(mode) != AccountTestModeCompact
}

// prepareAccountTestRequest pins the same prompt and effort input for every protocol.
// Native media/compact probes retain their original mode-specific inputs.
func (s *AccountTestService) prepareAccountTestRequest(c *gin.Context, account *Account, model, prompt, mode string, opts AccountTestOptions) (string, error) {
	c.Set(accountTestRequestedKey, model)
	streamMode := "stream"
	if account.IsBedrock() || normalizeAccountTestMode(mode) == AccountTestModeCompact || (isOpenAIImageModel(model) && account.Type == AccountTypeAPIKey) {
		streamMode = accountTestNativeNonStreamMode
	}
	if account.Platform == PlatformGrok && !isOrdinaryModelTest(account, model, mode) {
		streamMode = accountTestNativeNonStreamMode
		if normalizeGrokAccountTestMode(mode) == AccountTestModeGrokRealtime {
			streamMode = "native_websocket"
		}
	}
	c.Set("account_test_stream_mode", streamMode)
	effort := strings.ToLower(strings.TrimSpace(opts.ReasoningEffort))
	if len(effort) > 64 {
		return "", fmt.Errorf("reasoning_effort is too long")
	}
	c.Set(accountTestEffortKey, effort)
	if account.IsBedrock() && effort != "" {
		return "", fmt.Errorf("this native Bedrock invoke test does not support reasoning_effort")
	}
	if !isOrdinaryModelTest(account, model, mode) {
		if effort != "" {
			return "", fmt.Errorf("reasoning_effort is only supported for text generation tests")
		}
		c.Set(accountTestPromptKey, prompt)
		return prompt, nil
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultModelTestPrompt
		if s.settingService != nil {
			effective, err := s.settingService.EffectiveModelTestPrompt(c.Request.Context())
			if err != nil {
				return "", fmt.Errorf("could not read model test prompt: %w", err)
			}
			prompt = effective
		}
	}
	if !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > 8192 {
		return "", fmt.Errorf("test prompt must contain at most 8192 Unicode characters")
	}
	c.Set(accountTestPromptKey, prompt)
	return prompt, nil
}

// Native levels are taken from the account's catalog snapshot, not an invented
// provider-wide enum. A models.dev fallback is not proof of account support.
func accountTestNativeEffortMetadata(account *Account, model string) (UpstreamModelMetadata, bool) {
	metadata, ok := account.GetUpstreamModelMetadata(model)
	if !ok {
		return metadata, false
	}
	// Read provenance from the stored envelope: older DTOs may not yet carry
	// its fields, so re-marshaling the narrowed metadata would erase evidence.
	raw, _ := json.Marshal(account.Extra[UpstreamModelMetadataExtraKey])
	var snapshot struct {
		Models map[string]struct {
			ReasoningLevelsSource string `json:"reasoning_levels_source"`
		} `json:"models"`
	}
	_ = json.Unmarshal(raw, &snapshot)
	provenance := snapshot.Models[model].ReasoningLevelsSource
	if provenance == "models.dev" || provenance == "catalog_suffix" {
		return metadata, false
	}
	return metadata, len(metadata.SupportedReasoningLevels) > 0
}

func (s *AccountTestService) applyAccountTestEffort(c *gin.Context, account *Account, payload map[string]any, protocol string) error {
	effort := accountTestReasoningEffort(c)
	v, _ := c.Get(accountTestRequestedKey)
	model, _ := v.(string)
	// UI and catalog capabilities are keyed by the client request ID. Use the
	// wire ID only when no entry exists for that request (e.g. a default model).
	if _, exists := account.GetUpstreamModelMetadata(model); !exists {
		model, _ = payload["model"].(string)
	}
	metadata, supported := accountTestNativeEffortMetadata(account, model)
	if effort == "" {
		if supported && metadata.DefaultReasoningLevel != "" {
			c.Set(accountTestEffectiveEffortKey, metadata.DefaultReasoningLevel)
		}
		return nil
	}
	found := false
	if supported {
		for _, level := range metadata.SupportedReasoningLevels {
			if level == effort {
				found = true
				break
			}
		}
	}
	if !found {
		return fmt.Errorf("reasoning effort %q is not declared supported for model %q", effort, model)
	}
	switch protocol {
	case "responses":
		payload["reasoning"] = map[string]any{"effort": effort}
	case "chat":
		payload["reasoning_effort"] = effort
	case "anthropic":
		payload["output_config"] = map[string]any{"effort": effort}
		payload["thinking"] = map[string]any{"type": "adaptive"}
	case "gemini":
		config, _ := payload["generationConfig"].(map[string]any)
		if config == nil {
			config = map[string]any{}
			payload["generationConfig"] = config
		}
		config["thinkingConfig"] = map[string]any{"thinkingLevel": strings.ToUpper(effort)}
	default:
		return fmt.Errorf("reasoning effort is not supported by this test protocol")
	}
	c.Set(accountTestEffectiveEffortKey, effort)
	return nil
}
