package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type codexTelemetryUsageRepo struct{ service.UsageLogRepository }

func (codexTelemetryUsageRepo) GetByID(context.Context, int64) (*service.UsageLog, error) {
	return &service.UsageLog{ID: 12, CodexTelemetry: &codextelemetry.Snapshot{Version: 1, Transport: codextelemetry.WebSocket,
		Observations: []codextelemetry.Observation{{Source: codextelemetry.TimingEvent, Association: codextelemetry.ActiveResponse, EngineIDs: []string{"admin-engine"}}}}}, nil
}

func TestAdminUsageObservabilityReturnsCodexTelemetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewUsageHandler(service.NewUsageService(codexTelemetryUsageRepo{}, nil, nil, nil), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/usage/records/12/observability", nil)
	c.Params = gin.Params{{Key: "id", Value: "12"}}
	handler.GetObservability(c)
	require.Equal(t, 200, recorder.Code)
	require.Contains(t, recorder.Body.String(), "admin-engine")
	require.Contains(t, recorder.Body.String(), `"association":"active_response"`)
}
