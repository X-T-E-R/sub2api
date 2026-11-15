package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestClientRequestIDGeneratesAndExposesID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, w.Body.String())
	require.Equal(t, w.Body.String(), w.Header().Get(clientRequestIDHeader))
}

func TestClientRequestIDBoundsExistingContextID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.ClientRequestID, strings.Repeat("x", 200)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Len(t, w.Body.String(), 36)
	require.NotEqual(t, strings.Repeat("x", maxPersistentRequestIDBytes), w.Body.String())
	require.Equal(t, w.Body.String(), w.Header().Get(clientRequestIDHeader))
}

func TestClientRequestIDPreservesExistingContextID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.ClientRequestID, "existing-client-request-id"))
	req.Header.Set(clientRequestIDHeader, "inbound-client-id")
	req.Header.Set(requestIDHeader, "legacy-request-id")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "existing-client-request-id", w.Body.String())
	require.Equal(t, "existing-client-request-id", w.Header().Get(clientRequestIDHeader))
}

func TestClientRequestIDUsesInboundClientHeaderBeforeLegacyHeader(t *testing.T) {
	got, echo := serveClientRequestID(t, nil, map[string][]string{
		clientRequestIDHeader: {"primary-client-id"},
		requestIDHeader:       {"legacy-request-id"},
	})
	require.Equal(t, "primary-client-id", got)
	require.Equal(t, got, echo)
}

func TestClientRequestIDFallsBackToLegacyRequestHeader(t *testing.T) {
	got, echo := serveClientRequestID(t, nil, map[string][]string{
		requestIDHeader: {"legacy-request-id"},
	})
	require.Equal(t, "legacy-request-id", got)
	require.Equal(t, got, echo)
}

func TestClientRequestIDUsesInboundHeaderWhenExistingContextIDIsInvalid(t *testing.T) {
	got, echo := serveClientRequestID(t, strings.Repeat("x", maxPersistentRequestIDBytes+1), map[string][]string{
		clientRequestIDHeader: {"inbound-client-id"},
		requestIDHeader:       {"legacy-request-id"},
	})
	require.Equal(t, "inbound-client-id", got)
	require.Equal(t, got, echo)
}

func TestClientRequestIDRejectsInvalidPresentPrimaryWithoutLegacyFallback(t *testing.T) {
	invalidValues := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "leading whitespace", value: " client-id"},
		{name: "trailing whitespace", value: "client-id "},
		{name: "tab", value: "client\tid"},
		{name: "control", value: "client\x00id"},
		{name: "newline", value: "client\nid"},
		{name: "comma", value: "first,second"},
		{name: "non-ascii", value: "client-id-雪"},
		{name: "too long", value: strings.Repeat("x", maxPersistentRequestIDBytes+1)},
	}
	for _, tc := range invalidValues {
		t.Run(tc.name, func(t *testing.T) {
			got, echo := serveClientRequestID(t, nil, map[string][]string{
				clientRequestIDHeader: {tc.value},
				requestIDHeader:       {"must-not-fallback"},
			})
			requireGeneratedClientRequestID(t, got)
			require.NotEqual(t, "must-not-fallback", got)
			require.Equal(t, got, echo)
		})
	}
}

func TestClientRequestIDRejectsInvalidLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace", value: " client-id"},
		{name: "tab", value: "client\tid"},
		{name: "control", value: "client\x00id"},
		{name: "non-ascii", value: "client-id-雪"},
		{name: "too long", value: strings.Repeat("x", maxPersistentRequestIDBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, echo := serveClientRequestID(t, nil, map[string][]string{
				requestIDHeader: {tc.value},
			})
			requireGeneratedClientRequestID(t, got)
			require.Equal(t, got, echo)
		})
	}
}

func TestClientRequestIDRejectsDuplicateHeaderValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		values []string
		legacy []string
	}{
		{name: "primary", header: clientRequestIDHeader, values: []string{"first", "second"}, legacy: []string{"must-not-fallback"}},
		{name: "legacy fallback", header: requestIDHeader, values: []string{"first", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string][]string{tc.header: tc.values}
			if tc.legacy != nil {
				headers[requestIDHeader] = tc.legacy
			}
			got, echo := serveClientRequestID(t, nil, headers)
			requireGeneratedClientRequestID(t, got)
			require.Equal(t, got, echo)
		})
	}
}

func TestClientRequestIDAccepts64CharacterASCIIValue(t *testing.T) {
	want := "AZaz09._:-" + strings.Repeat("x", maxPersistentRequestIDBytes-len("AZaz09._:-"))
	got, echo := serveClientRequestID(t, nil, map[string][]string{
		clientRequestIDHeader: {want},
	})
	require.Equal(t, want, got)
	require.Equal(t, got, echo)
}

func TestClientRequestIDIsAvailableToRequestLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := initMiddlewareTestLogger(t)
	router := gin.New()
	router.Use(ClientRequestID(), RequestLogger(), Logger())
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(clientRequestIDHeader, "logger-client-id")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "logger-client-id", w.Header().Get(clientRequestIDHeader))
	for _, event := range sink.list() {
		if event != nil && event.Message == "http request completed" {
			require.Equal(t, "logger-client-id", event.Fields["client_request_id"])
			return
		}
	}
	t.Fatal("correlation logger event not found")
}

func serveClientRequestID(t *testing.T, contextID any, headers map[string][]string) (string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if contextID != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxkey.ClientRequestID, contextID))
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.String(), w.Header().Get(clientRequestIDHeader)
}

func requireGeneratedClientRequestID(t *testing.T, value string) {
	t.Helper()
	parsed, err := uuid.Parse(value)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, parsed)
}
