package admin

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Warmup handles POST /api/v1/admin/accounts/:id/warmup.
// An empty JSON body ({}) or omitted body is valid and applies defaults while overriding account_ids to [id].
func (h *AccountHandler) Warmup(c *gin.Context) {
	if h.accountWarmup == nil {
		response.ErrorFrom(c, service.ErrAccountWarmupUnavailable)
		return
	}

	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}

	var req service.WarmupAccountsRequest
	if c.Request != nil && c.Request.Body != nil {
		raw, readErr := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			response.BadRequest(c, "Invalid request body: "+readErr.Error())
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if unmarshalErr := json.Unmarshal(raw, &req); unmarshalErr != nil {
				response.BadRequest(c, "Invalid request: "+unmarshalErr.Error())
				return
			}
		}
	}

	req.AccountIDs = []int64{accountID}

	resp, err := h.accountWarmup.WarmupAccounts(c.Request.Context(), &req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, resp)
}

// BatchWarmup handles POST /api/v1/admin/accounts/batch-warmup.
func (h *AccountHandler) BatchWarmup(c *gin.Context) {
	if h.accountWarmup == nil {
		response.ErrorFrom(c, service.ErrAccountWarmupUnavailable)
		return
	}

	var req service.WarmupAccountsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	resp, err := h.accountWarmup.WarmupAccounts(c.Request.Context(), &req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, resp)
}
