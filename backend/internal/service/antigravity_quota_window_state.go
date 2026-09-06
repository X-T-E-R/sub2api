package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

type AntigravityQuotaWindow struct {
	SourceBucketID    string    `json:"source_bucket_id"`
	RemainingFraction float64   `json:"remaining_fraction"`
	ResetTime         string    `json:"reset_time,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
	Stale             bool      `json:"stale,omitempty"`
}

func antigravityWindowKey(id string) string {
	switch id {
	case "3p-5h", "claude:5h":
		return "claude_5h"
	case "3p-weekly", "claude:weekly":
		return "claude_weekly"
	case "gemini-5h", "gemini:5h":
		return "gemini_5h"
	case "gemini-weekly", "gemini:weekly":
		return "gemini_weekly"
	default:
		return ""
	}
}

func applyAntigravitySummary(info *UsageInfo, summary *antigravity.UserQuotaSummary, now time.Time) {
	info.AntigravityWindowCheckedAt = &now
	info.AntigravityWindowState = antigravityObservationUnavailable
	if summary == nil {
		return
	}
	info.AntigravityWindows = make(map[string]*AntigravityQuotaWindow)
	seen := make(map[string]bool)
	invalid := false
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			key := antigravityWindowKey(bucket.BucketID)
			if key == "" {
				continue
			}
			if seen[key] {
				// Ambiguous duplicates (including aliases) have no authoritative winner.
				delete(info.AntigravityWindows, key)
				invalid = true
				continue
			}
			seen[key] = true
			fraction, ok := validRemainingFraction(bucket.RemainingFraction)
			if !ok {
				invalid = true
				continue
			}
			reset, ok := normalizeAntigravityResetTime(bucket.ResetTime)
			if !ok {
				invalid = true
			}
			info.AntigravityWindows[key] = &AntigravityQuotaWindow{
				SourceBucketID: bucket.BucketID, RemainingFraction: fraction,
				ResetTime: reset, ObservedAt: now,
			}
		}
	}
	if len(info.AntigravityWindows) > 0 {
		info.AntigravityWindowState = antigravityObservationPartial
		if len(info.AntigravityWindows) == 4 && !invalid {
			info.AntigravityWindowState = antigravityObservationAvailable
		}
	}
}

// Keep independently observed windows when an optional endpoint omits or fails
// them. Copies prevent a passive read or later merge from mutating cached data.
func retainAntigravityWindows(current, previous *UsageInfo) {
	if current == nil || previous == nil {
		return
	}
	for key, window := range previous.AntigravityWindows {
		if window == nil || current.AntigravityWindows[key] != nil {
			continue
		}
		if current.AntigravityWindows == nil {
			current.AntigravityWindows = make(map[string]*AntigravityQuotaWindow)
		}
		copy := *window
		copy.Stale = true
		current.AntigravityWindows[key] = &copy
	}
}

// The scope is a private in-memory freshness fingerprint, never a log/DTO field.
func antigravityQuotaScope(account *Account) string {
	return antigravityQuotaScopeForOrigin(account, resolveAntigravityForwardBaseURL(account))
}

func antigravityQuotaScopeForOrigin(account *Account, baseURL string) string {
	if account == nil {
		return ""
	}
	data, _ := json.Marshal([]any{account.ID, account.GetCredential("project_id"), account.GetCredential("access_token"), account.GetCredential("refresh_token"), baseURL})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func copyAntigravityUsage(info *UsageInfo, expired bool) *UsageInfo {
	copy := *info
	copy.AntigravityQuotaStale = copy.AntigravityQuotaStale || expired
	copy.AntigravityWindows = make(map[string]*AntigravityQuotaWindow, len(info.AntigravityWindows))
	for key, window := range info.AntigravityWindows {
		if window == nil {
			continue
		}
		w := *window
		w.Stale = w.Stale || expired
		copy.AntigravityWindows[key] = &w
	}
	if info.FiveHour != nil {
		p := *info.FiveHour
		copy.FiveHour = &p
	}
	recalcAntigravityRemainingSeconds(&copy)
	return &copy
}
