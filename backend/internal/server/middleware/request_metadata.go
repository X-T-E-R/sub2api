package middleware

import (
	"strings"
	"unicode/utf8"
)

const (
	maxPersistentRequestIDBytes = 64
	maxPersistentUserAgentBytes = 512
)

// normalizePersistentText bounds attacker-controlled metadata before it reaches
// logs or database columns while preserving valid UTF-8 content.
func normalizePersistentText(value string, maxBytes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func normalizeCorrelationID(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	return value, value != "" && len(value) <= maxPersistentRequestIDBytes
}

// normalizeClientRequestID validates the externally supplied client correlation
// ID without changing the legacy X-Request-ID validator above. Client IDs are
// persisted in VARCHAR(64) columns (usage_logs and ops_error_logs); the wider
// ops_system_logs projection is VARCHAR(128), but inbound IDs remain capped at 64.
func normalizeClientRequestID(value string) (string, bool) {
	if len(value) == 0 || len(value) > maxPersistentRequestIDBytes {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '.' || ch == '_' || ch == ':' || ch == '-' {
			continue
		}
		return "", false
	}
	return value, true
}
