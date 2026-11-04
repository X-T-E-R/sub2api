package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
)

// Keep the scheduler's explicit conversation anchor through the final Messages
// conversion. Content-derived fallback IDs cannot represent image-only sessions
// and must not merge the same opener across authenticated keys or accounts.
func antigravityWireSessionID(c *gin.Context, account *Account, request *antigravity.ClaudeRequest) string {
	if c == nil || account == nil || account.ID <= 0 || request == nil {
		return ""
	}
	value, _ := c.Get("api_key")
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.ID <= 0 {
		return ""
	}
	parsed := &ParsedRequest{SessionContext: &SessionContext{ClientSessionID: GatewaySessionIDFromHeaders(c)}}
	if request.Metadata != nil {
		parsed.MetadataUserID = request.Metadata.UserID
	}
	source, seed := gatewayExplicitSessionIdentity(parsed)
	if seed == "" {
		return "" // Preserve legacy converter callers without an explicit session.
	}
	scope := strconv.FormatInt(key.ID, 10) + ":" + strconv.FormatInt(derefGroupID(key.GroupID), 10) + ":" +
		strconv.FormatInt(account.ID, 10) + ":" + strconv.Quote(source) + ":" + strconv.Quote(seed)
	digest := sha256.Sum256([]byte(scope))
	return "ag-v1:" + hex.EncodeToString(digest[:])
}
