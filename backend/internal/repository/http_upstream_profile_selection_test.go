package repository

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProfileSelectsH2WhileDefaultRemainsGeneric(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{Enabled: true},
	}}
	svc := &httpUpstreamService{cfg: cfg, clients: make(map[string]*upstreamClientEntry)}

	generic, err := svc.getClientEntry("", 8101, 1, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	h2, err := svc.getClientEntry("", 8101, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)

	genericTransport, ok := generic.client.Transport.(*http.Transport)
	require.True(t, ok)
	h2Transport, ok := h2.client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, upstreamProtocolModeDefault, generic.protocolMode)
	require.False(t, genericTransport.ForceAttemptHTTP2, "generic profile must not claim eager HTTP/2 enablement")
	require.Equal(t, upstreamProtocolModeOpenAIH2, h2.protocolMode)
	require.True(t, h2Transport.ForceAttemptHTTP2, "OpenAI/Grok Messages profile must select the existing HTTP/2 transport")
}
