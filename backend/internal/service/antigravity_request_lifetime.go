package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"

// releaseAcceptedAntigravityClaudeHistory drops the decoded Claude history once the
// upstream has accepted a request envelope.
//
// The decoded messages/system/tools are only consumed while building the envelope and
// by the pre-response signature and budget retries. After a 2xx the response path,
// usage, Task24 continuation and malformed-function recovery read the accepted wire
// body, stream flag and scalar request fields instead. Keeping the decoded history
// alive for the whole stream would hold another full copy of every message, including
// base64 screenshots, for minutes per request.
func releaseAcceptedAntigravityClaudeHistory(req *antigravity.ClaudeRequest) {
	if req == nil {
		return
	}
	req.Messages = nil
	req.System = nil
	req.Tools = nil
}
