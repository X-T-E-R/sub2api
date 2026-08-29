//go:build unit

package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAppendOpenAITransportFailoverFieldDistinguishesEOFAndHTTP502(t *testing.T) {
	tests := []struct {
		name          string
		failoverErr   *service.UpstreamFailoverError
		wantTransport string
	}{
		{
			name: "transport EOF",
			failoverErr: &service.UpstreamFailoverError{
				StatusCode:     http.StatusBadGateway,
				TransportError: `Post "https://cli-chat-proxy.grok.com/v1/responses": EOF`,
			},
			wantTransport: `Post "https://cli-chat-proxy.grok.com/v1/responses": EOF`,
		},
		{
			name:        "real HTTP 502",
			failoverErr: &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			log := zap.New(core)
			fields := appendOpenAITransportFailoverField([]zap.Field{zap.Int("upstream_status", tt.failoverErr.StatusCode)}, tt.failoverErr)
			log.Warn("openai_messages.upstream_failover_switching", fields...)

			entries := logs.All()
			require.Len(t, entries, 1)
			contextMap := entries[0].ContextMap()
			if tt.wantTransport == "" {
				require.NotContains(t, contextMap, "transport_error")
				return
			}
			require.Equal(t, tt.wantTransport, contextMap["transport_error"])
		})
	}
}
