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

func (h *SettingHandler) GetCodexSessionAffinity(c *gin.Context) {
	value, err := h.settingService.GetCodexSessionAffinitySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, value)
}

func (h *SettingHandler) UpdateCodexSessionAffinity(c *gin.Context) {
	var input struct {
		GroupIDs *[]int64 `json:"group_ids"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 32768))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		response.BadRequest(c, "Invalid Codex session affinity settings: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one JSON object")
		return
	}
	if input.GroupIDs == nil {
		response.BadRequest(c, "group_ids is required and must be an array")
		return
	}
	if err := h.settingService.SetCodexSessionAffinitySettings(c.Request.Context(), service.CodexSessionAffinitySettings{GroupIDs: *input.GroupIDs}); err != nil {
		if errors.Is(err, service.ErrInvalidCodexSessionAffinity) {
			response.BadRequest(c, err.Error())
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	h.GetCodexSessionAffinity(c)
}
