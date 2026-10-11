package admin

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetAntigravityThoughtPolicy(c *gin.Context) {
	value, err := h.settingService.GetAntigravityThoughtPolicySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}

// UpdateAntigravityThoughtPolicy replaces both global switches at once. The pair is
// required and strictly typed: an omitted or non-boolean switch is rejected instead
// of being silently stored as false, because "off" and "not configured" mean
// different things for the persisted global pair.
func (h *SettingHandler) UpdateAntigravityThoughtPolicy(c *gin.Context) {
	var input struct {
		SuppressThoughtOnlyStop *bool `json:"suppress_thought_only_stop"`
		ContinueThoughtOnly     *bool `json:"continue_thought_only"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		response.BadRequest(c, "Invalid Antigravity thought policy settings: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one JSON object")
		return
	}
	if input.SuppressThoughtOnlyStop == nil || input.ContinueThoughtOnly == nil {
		response.BadRequest(c, "suppress_thought_only_stop and continue_thought_only are required booleans")
		return
	}
	value := service.AntigravityThoughtPolicySettings{
		SuppressThoughtOnlyStop: *input.SuppressThoughtOnlyStop,
		ContinueThoughtOnly:     *input.ContinueThoughtOnly,
	}
	if err := h.settingService.SetAntigravityThoughtPolicySettings(c.Request.Context(), value); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetAntigravityThoughtPolicy(c)
}
