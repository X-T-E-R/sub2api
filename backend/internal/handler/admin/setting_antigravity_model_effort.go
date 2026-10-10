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

func (h *SettingHandler) GetAntigravityModelEffort(c *gin.Context) {
	value, err := h.settingService.GetAntigravityModelEffortSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}

func (h *SettingHandler) UpdateAntigravityModelEffort(c *gin.Context) {
	var input struct {
		Levels        *[]string `json:"levels"`
		DefaultEffort *string   `json:"default_effort"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		response.BadRequest(c, "Invalid Antigravity model effort settings: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one JSON object")
		return
	}
	if input.Levels == nil || input.DefaultEffort == nil {
		response.BadRequest(c, "levels and default_effort are required")
		return
	}
	value := service.AntigravityModelEffortSettings{Levels: *input.Levels, DefaultEffort: *input.DefaultEffort}
	if err := h.settingService.SetAntigravityModelEffortSettings(c.Request.Context(), value); err != nil {
		if errors.Is(err, service.ErrInvalidAntigravityModelEffort) {
			response.BadRequest(c, err.Error())
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	h.GetAntigravityModelEffort(c)
}
