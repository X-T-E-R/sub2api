package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageCodexTelemetryIsAdminDetailOnly(t *testing.T) {
	log := &service.UsageLog{RequestID: "fixture", Model: "requested", CodexTelemetryAvailable: true,
		CodexTelemetry: &codextelemetry.Snapshot{Version: 1, Transport: codextelemetry.WebSocket,
			Observations: []codextelemetry.Observation{{Source: codextelemetry.TimingEvent, Association: codextelemetry.ActiveResponse, EngineIDs: []string{"private-engine"}}}},
	}
	user, err := json.Marshal(UsageLogFromService(log))
	require.NoError(t, err)
	require.NotContains(t, string(user), "codex_telemetry")
	require.NotContains(t, string(user), "private-engine")
	admin, err := json.Marshal(UsageLogFromServiceAdmin(log))
	require.NoError(t, err)
	require.Contains(t, string(admin), `"codex_telemetry_available":true`)
	require.NotContains(t, string(admin), "private-engine", "list/export DTO stays compact; raw metadata is lazy admin detail")
}
