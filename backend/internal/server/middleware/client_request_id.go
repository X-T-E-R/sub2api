package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const clientRequestIDHeader = "X-Client-Request-ID"

// ClientRequestID ensures every request has a unique client_request_id in request.Context().
// RequestLogger should run after it so the request-scoped logger binds this final ID once.
//
// This is used by the Ops monitoring module for end-to-end request correlation.
func ClientRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		ctx := c.Request.Context()
		id, valid := ctx.Value(ctxkey.ClientRequestID).(string)
		if valid {
			id, valid = normalizeClientRequestID(id)
		}
		if !valid {
			var present bool
			id, present, valid = clientRequestIDHeaderValue(c.Request.Header, clientRequestIDHeader)
			if present {
				// A present but invalid primary ID is deliberately terminal. Do not
				// silently fall back to X-Request-ID, which could correlate this
				// request with an unrelated upstream request.
				if !valid {
					id = uuid.NewString()
				}
			} else {
				id, present, valid = clientRequestIDHeaderValue(c.Request.Header, requestIDHeader)
				if !present || !valid {
					id = uuid.NewString()
				}
			}
		}

		c.Header(clientRequestIDHeader, id)
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, id)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func clientRequestIDHeaderValue(headers http.Header, name string) (string, bool, bool) {
	var values []string
	present := false
	for key, headerValues := range headers {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, headerValues...)
		}
	}
	if !present {
		return "", false, false
	}
	if len(values) != 1 {
		return "", true, false
	}
	id, valid := normalizeClientRequestID(values[0])
	return id, true, valid
}
