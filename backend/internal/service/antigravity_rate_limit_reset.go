package service

import (
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// A detail-less resource error is not evidence of exhausted account quota.
// Keep only a short anti-burst window after the request's retries are exhausted;
// account failover for that request remains unchanged.
const antigravityUnspecified429Cooldown = 3 * time.Second

// Explicit upstream delays take precedence over local fallback cooldowns. Keep
// the longest signal when Retry-After, RetryInfo and quota metadata coexist.
func antigravityExplicitRetryDelay(headers http.Header, body []byte) (time.Duration, bool) {
	now := time.Now()
	var reset time.Time
	if ts := ParseGeminiRateLimitResetTime(body); ts != nil {
		reset = time.Unix(*ts, 0)
	}
	if ts := parseRetryAfterResetTime(headers, now); ts != nil && ts.After(reset) {
		reset = *ts
	}
	gjson.GetBytes(body, "error.details").ForEach(func(_, detail gjson.Result) bool {
		if detail.Get("@type").String() == googleRPCTypeRetryInfo {
			if delay, err := time.ParseDuration(detail.Get("retryDelay").String()); err == nil {
				if ts := now.Add(delay); ts.After(reset) {
					reset = ts
				}
			}
		}
		return true
	})
	return reset.Sub(now), !reset.IsZero()
}

func (s *AntigravityGatewayService) antigravity429Cooldown(headers http.Header, body []byte) time.Duration {
	if delay, explicit := antigravityExplicitRetryDelay(headers, body); explicit {
		return delay
	}
	// Restrict the short window to a valid Google resource envelope with no
	// structured details or header signal. Unknown/malformed errors and real
	// ErrorInfo responses retain the configured fallback.
	if gjson.ValidBytes(body) {
		err := gjson.GetBytes(body, "error")
		details := err.Get("details")
		if err.Get("code").Int() == 429 && err.Get("status").String() == googleRPCStatusResourceExhausted &&
			(!details.Exists() || details.Type == gjson.Null || details.IsArray() && len(details.Array()) == 0) &&
			strings.TrimSpace(headers.Get("Retry-After")) == "" {
			return antigravityUnspecified429Cooldown
		}
	}
	return s.getDefaultRateLimitDuration()
}
