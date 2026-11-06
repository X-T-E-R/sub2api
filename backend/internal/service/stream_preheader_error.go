package service

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var streamPreheaderGoogleDuration = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,9})?s$`)

func streamPreheaderBodyRetryDelay(body []byte) string {
	var delay string
	gjson.GetBytes(body, "error.details").ForEach(func(_, detail gjson.Result) bool {
		if detail.Get("@type").String() != googleRPCTypeRetryInfo {
			return true
		}
		value := detail.Get("retryDelay").String()
		if !streamPreheaderGoogleDuration.MatchString(value) {
			return true
		}
		seconds, err := strconv.ParseFloat(strings.TrimSuffix(value, "s"), 64)
		if err == nil && seconds <= 315576000000 {
			delay = value
			return false
		}
		return true
	})
	return delay
}

// Replace the upstream error event before it reaches the client. It remains a
// single protocol terminal with the original type, status and available backoff.
func finishStreamPreheaderSSEError(c *gin.Context, payload []byte, headers http.Header, response ...streamPreheaderResponseMeta) error {
	if StreamPreheaderFinalized(c) {
		return errStreamPreheaderTerminal
	}
	meta := streamPreheaderResponseMeta{}
	if len(response) > 0 {
		meta = response[0]
	}
	if meta.id == "" {
		meta.id = gjson.GetBytes(payload, "response.id").String()
	}
	if nested := gjson.GetBytes(payload, "response.error"); nested.Exists() {
		payload, _ = json.Marshal(gin.H{"error": json.RawMessage(nested.Raw)})
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(payload)))
	status := openAIStreamFailedEventSemanticStatus(payload, message)
	for _, path := range []string{"error.code", "error.status", "error.status_code", "status_code"} {
		value := gjson.GetBytes(payload, path)
		if value.Type == gjson.Number && value.Int() >= 400 && value.Int() <= 599 {
			status = int(value.Int())
			break
		}
	}
	errType := streamPreheaderErrorType(status, payload)
	policy, _ := c.Get(streamPreheaderPolicyKey)
	protocol := policy.(streamPreheaderPolicy).protocol
	if protocol == streamPreheaderGemini {
		errType = gjson.GetBytes(payload, "error.status").String()
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if !c.Writer.Written() {
		return writeConfiguredStreamFailure(c, status, errType, message, meta)
	}
	meta.retryDelay = streamPreheaderBodyRetryDelay(payload)
	retryAfter := headers.Get("Retry-After")
	if value, _ := streamPreheaderRetryAfter(retryAfter, time.Now()); value == "" {
		retryAfter = gjson.GetBytes(payload, "error.retry_after").String()
	}
	frame := recordStreamPreheaderFailure(c, protocol, status, errType, message, retryAfter, meta)
	return writeStreamPreheaderFailureFrame(c, frame)
}

func streamPreheaderSSEErrorPayload(line string) ([]byte, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	parsed := gjson.Parse(payload)
	if inner := parsed.Get("response"); inner.IsObject() && inner.Get("error").Exists() {
		parsed = inner
	}
	if !parsed.Get("error").Exists() {
		return nil, false
	}
	return []byte(parsed.Raw), true
}

func openAIStreamHasObservedUsage(usage *OpenAIUsage, imageCount, searchCount int) bool {
	return (usage != nil && *usage != (OpenAIUsage{})) || imageCount > 0 || searchCount > 0
}
