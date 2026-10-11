package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// maxModelTestPromptBodyBytes caps the PUT body of the model test prompt.
// The service limit is 8192 Unicode characters, i.e. at most 32768 bytes of
// UTF-8 text plus the JSON envelope, so a legitimate 8192-emoji prompt still
// fits while an oversized body is rejected before it is parsed.
const maxModelTestPromptBodyBytes = 64 << 10

// GetModelTestPrompt returns the stored prompt and the built-in default that
// ordinary account tests fall back to.
func (h *SettingHandler) GetModelTestPrompt(c *gin.Context) {
	value, err := h.settingService.GetModelTestSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}

// UpdateModelTestPrompt stores the prompt and answers with the settings a
// following GET reports. An empty prompt clears the stored value, which restores
// the built-in default for later tests.
func (h *SettingHandler) UpdateModelTestPrompt(c *gin.Context) {
	var input struct {
		Prompt *string `json:"prompt"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxModelTestPromptBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		response.BadRequest(c, "Invalid model test prompt settings: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one JSON object")
		return
	}
	// A missing key and an explicit null both leave the pointer nil: the request
	// never says what to store, so it is not the "empty prompt resets" case.
	if input.Prompt == nil {
		response.BadRequest(c, "prompt is required and must be a string")
		return
	}
	value, err := h.settingService.SetModelTestPrompt(c.Request.Context(), *input.Prompt)
	if err != nil {
		if errors.Is(err, service.ErrInvalidModelTestPrompt) {
			response.BadRequest(c, err.Error())
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	response.Success(c, value)
}
