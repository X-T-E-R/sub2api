package admin

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
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

// The legacy A/B pair remains required. Older clients may omit the new global
// switch, in which case its persisted value is preserved rather than reset.
func (h *SettingHandler) UpdateAntigravityThoughtPolicy(c *gin.Context) {
	var input struct {
		SuppressThoughtOnlyStop *bool           `json:"suppress_thought_only_stop"`
		ContinueThoughtOnly     *bool           `json:"continue_thought_only"`
		ContinueZeroOutput      json.RawMessage `json:"continue_zero_output"`
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
	value, err := h.settingService.GetAntigravityThoughtPolicySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if len(input.ContinueZeroOutput) > 0 {
		var flag *bool
		if err := json.Unmarshal(input.ContinueZeroOutput, &flag); err != nil || flag == nil {
			response.BadRequest(c, "continue_zero_output must be a boolean")
			return
		}
		value.ContinueZeroOutput = *flag
	}
	value.SuppressThoughtOnlyStop = *input.SuppressThoughtOnlyStop
	value.ContinueThoughtOnly = *input.ContinueThoughtOnly
	if err := h.settingService.SetAntigravityThoughtPolicySettings(c.Request.Context(), value); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetAntigravityThoughtPolicy(c)
}
