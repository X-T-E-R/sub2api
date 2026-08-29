//go:build unit

package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// These compatibility wrappers are used only by unit tests. Production paths
// select the explicit compatibility mode at their protocol boundary.
func patchGrokResponsesBodyWithClientTools(body []byte, upstreamModel string) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	patched, mapping, _, err := patchGrokResponsesBodyWithClientToolsCompatibility(body, upstreamModel, true)
	return patched, mapping, err
}

func explicitGrokCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	if identity := explicitGrokNativeCacheIdentity(c, body); identity != "" {
		return identity
	}
	return grokBridgeCacheSeed(c, body, explicitKey)
}

func grokChatResponsesBridgeEligibility(body []byte) (bool, string) {
	return grokChatResponsesBridgeEligibilityWithCompatibility(body, true)
}

func snapshotGrokResponsesCompatibilityMetrics() GrokResponsesCompatibilityMetricsSnapshot {
	return SnapshotGrokResponsesCompatibilityMetrics()
}
