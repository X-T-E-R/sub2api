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

func (h *SettingHandler) GetModelReasoningFloor(c *gin.Context) {
	value, err := h.settingService.GetModelReasoningFloorSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}

func (h *SettingHandler) UpdateModelReasoningFloor(c *gin.Context) {
	var input struct {
		Enabled *bool                              `json:"enabled"`
		Rules   *[]service.ModelReasoningFloorRule `json:"rules"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 32768))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		response.BadRequest(c, "Invalid model reasoning settings: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one JSON object")
		return
	}
	if input.Enabled == nil || input.Rules == nil {
		response.BadRequest(c, "enabled and rules are required")
		return
	}
	value := service.ModelReasoningFloorSettings{Enabled: *input.Enabled, Rules: *input.Rules}
	if err := h.settingService.SetModelReasoningFloorSettings(c.Request.Context(), value); err != nil {
		if errors.Is(err, service.ErrInvalidModelReasoningFloor) {
			response.BadRequest(c, err.Error())
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	h.GetModelReasoningFloor(c)
}
