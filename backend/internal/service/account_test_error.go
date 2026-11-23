package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// structuredVerificationURLSecretQueryRegex redacts explicit API credentials if
// an upstream ever injects them into a structured validation_url / appeal_url,
// while preserving Google's own verification query tokens (e.g. plt, token).
var structuredVerificationURLSecretQueryRegex = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|client_secret|access[_-]?token|refresh[_-]?token|authorization|x-api-key)=)[^&"\s']+`)

type accountTestErrorDetail struct {
	message       string
	validationURL string
	appealURL     string
}

func formatAccountTestUpstreamError(prefix string, statusCode int, body []byte) string {
	return buildAccountTestUpstreamError(prefix, statusCode, body).message
}

// buildAccountTestUpstreamError extracts root-cause upstream error details for
// Admin Account Test SSE responses while redacting echoed credentials.
//
// Structured Google/Antigravity validation_url and appeal_url fields are
// extracted first and preserve their verification query tokens (such as
// plt/token) while still redacting userinfo and explicit credential literals.
// Prose fallback URLs always undergo full credential sanitization so redaction
// can never be bypassed. The returned struct carries ValidationURL / AppealURL
// directly even when the display text already contains the URL.
func buildAccountTestUpstreamError(prefix string, statusCode int, body []byte) accountTestErrorDetail {
	trimmed := bytes.TrimSpace(body)
	doc := unwrapSingleErrorJSONObject(trimmed)

	actionURL, urlLabel, structuredURL := extractAccountTestValidationURL(doc, trimmed)
	msg := strings.TrimSpace(extractUpstreamErrorMessage(doc))
	qualifiers := extractAccountTestErrorQualifiers(doc, statusCode)

	header := ""
	switch {
	case prefix != "" && statusCode > 0:
		header = fmt.Sprintf("%s returned %d", prefix, statusCode)
	case statusCode > 0:
		header = fmt.Sprintf("Upstream returned %d", statusCode)
	case prefix != "":
		header = prefix
	}
	if len(qualifiers) > 0 {
		tag := "(" + strings.Join(qualifiers, " / ") + ")"
		if header != "" {
			header += " " + tag
		} else {
			header = tag
		}
	}

	detailText := ""
	if msg != "" {
		detailText = SanitizeUpstreamErrorMessage(msg)
	} else if len(trimmed) > 0 {
		raw := string(trimmed)
		if json.Valid(trimmed) {
			var compact bytes.Buffer
			if err := json.Compact(&compact, trimmed); err == nil {
				raw = compact.String()
			}
		}
		detailText = truncateString(SanitizeUpstreamErrorMessage(raw), tempUnschedMessageMaxBytes)
	}

	result := ""
	switch {
	case header != "" && detailText != "":
		result = header + ": " + detailText
	case detailText != "":
		result = detailText
	default:
		result = header
	}

	if actionURL != "" && !strings.Contains(result, actionURL) {
		if result != "" {
			result += " | " + urlLabel + ": " + actionURL
		} else {
			result = urlLabel + ": " + actionURL
		}
	}

	out := accountTestErrorDetail{message: result}
	if structuredURL && actionURL != "" {
		if urlLabel == "appeal_url" {
			out.appealURL = actionURL
		} else {
			out.validationURL = actionURL
		}
	}
	return out
}

// unwrapSingleErrorJSONObject unwraps single-element JSON arrays such as
// Gemini streamGenerateContent error payloads `[{"error": {...}}]`.
func unwrapSingleErrorJSONObject(trimmed []byte) []byte {
	if len(trimmed) == 0 || trimmed[0] != '[' || !gjson.ValidBytes(trimmed) {
		return trimmed
	}
	first := gjson.GetBytes(trimmed, "0")
	if first.Exists() && first.IsObject() {
		return []byte(first.Raw)
	}
	return trimmed
}

func extractAccountTestValidationURL(doc, raw []byte) (string, string, bool) {
	// 1. Extract explicit structured metadata URLs first so they are never
	// conflated with prose fallback matches elsewhere in the error payload.
	if u := firstStructuredMetadataURL(doc, "validation_url"); u != "" {
		return sanitizeStructuredVerificationURL(u), "validation_url", true
	}
	if u := firstStructuredMetadataURL(doc, "appeal_url"); u != "" {
		return sanitizeStructuredVerificationURL(u), "appeal_url", true
	}

	// 2. Fall back to the shared prose extractor, applying full credential
	// sanitization because prose URLs are not guaranteed verification links.
	// Prose fallback matches are never marked structured so uncleaned regex
	// punctuation never bypasses frontend link trimming via exactUrls.
	urlStr := strings.TrimSpace(extractValidationURL(string(doc)))
	if urlStr == "" {
		if respObj := gjson.GetBytes(doc, "response"); respObj.Exists() && respObj.IsObject() {
			urlStr = strings.TrimSpace(extractValidationURL(respObj.Raw))
		}
	}
	if urlStr == "" && !bytes.Equal(doc, raw) {
		urlStr = strings.TrimSpace(extractValidationURL(string(raw)))
	}
	if urlStr == "" || !isSafeHTTPURL(urlStr) {
		return "", "", false
	}
	return SanitizeUpstreamErrorMessage(urlStr), "validation_url", false
}

func isSafeHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return (scheme == "http" || scheme == "https") && parsed.Host != ""
}

func firstStructuredMetadataURL(doc []byte, field string) string {
	if len(doc) == 0 || !gjson.ValidBytes(doc) {
		return ""
	}
	for _, path := range []string{
		"error.details.#.metadata." + field,
		"response.error.details.#.metadata." + field,
	} {
		for _, item := range gjson.GetBytes(doc, path).Array() {
			if u := strings.TrimSpace(item.String()); u != "" && isSafeHTTPURL(u) {
				return u
			}
		}
	}
	return ""
}

func sanitizeStructuredVerificationURL(urlStr string) string {
	urlStr = structuredVerificationURLSecretQueryRegex.ReplaceAllString(urlStr, `${1}***`)
	urlStr = sensitiveURLUserinfoRegex.ReplaceAllString(urlStr, `${1}***@`)
	urlStr = sensitiveBearerTokenRegex.ReplaceAllString(urlStr, `${1}***`)
	for _, p := range sensitiveAPIKeyPatterns {
		urlStr = p.pattern.ReplaceAllString(urlStr, p.replace)
	}
	return urlStr
}

func extractAccountTestErrorQualifiers(doc []byte, statusCode int) []string {
	if len(doc) == 0 || !gjson.ValidBytes(doc) {
		return nil
	}
	errStatus := firstNonEmpty(
		strings.TrimSpace(gjson.GetBytes(doc, "error.status").String()),
		strings.TrimSpace(gjson.GetBytes(doc, "response.error.status").String()),
	)
	errReason := firstNonEmptyArrayString(doc, "error.details.#.reason", "response.error.details.#.reason")
	errType := firstNonEmpty(
		strings.TrimSpace(gjson.GetBytes(doc, "error.type").String()),
		strings.TrimSpace(gjson.GetBytes(doc, "response.error.type").String()),
	)
	errCode := firstNonEmpty(
		strings.TrimSpace(gjson.GetBytes(doc, "error.code").String()),
		strings.TrimSpace(gjson.GetBytes(doc, "response.error.code").String()),
		strings.TrimSpace(gjson.GetBytes(doc, "code").String()),
	)
	if statusCode > 0 && errCode == strconv.Itoa(statusCode) {
		errCode = ""
	}

	qualifiers := make([]string, 0, 3)
	appendUnique := func(candidate string) {
		candidate = strings.TrimSpace(SanitizeUpstreamErrorMessage(candidate))
		if candidate == "" {
			return
		}
		for _, existing := range qualifiers {
			if strings.EqualFold(existing, candidate) {
				return
			}
		}
		qualifiers = append(qualifiers, candidate)
	}

	appendUnique(errStatus)
	appendUnique(errReason)
	if errStatus == "" {
		appendUnique(errType)
	}
	appendUnique(errCode)
	return qualifiers
}

func firstNonEmptyArrayString(doc []byte, paths ...string) string {
	for _, path := range paths {
		for _, item := range gjson.GetBytes(doc, path).Array() {
			if v := strings.TrimSpace(item.String()); v != "" {
				return v
			}
		}
	}
	return ""
}

func buildAntigravityAccountTestError(err error, state *accountTestRawStreamState, writer *accountTestForwardWriter) accountTestErrorDetail {
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) && failoverErr != nil {
		if len(bytes.TrimSpace(failoverErr.ResponseBody)) > 0 {
			return buildAccountTestUpstreamError("Upstream", failoverErr.StatusCode, failoverErr.ResponseBody)
		}
		if state != nil {
			status, body, transportErr := state.lastUpstreamError()
			if status >= 400 && len(bytes.TrimSpace(body)) > 0 {
				return buildAccountTestUpstreamError("Upstream", status, body)
			}
			if transportErr != nil {
				sanitized := SanitizeUpstreamErrorMessage(transportErr.Error())
				if failoverErr.StatusCode > 0 {
					return accountTestErrorDetail{message: fmt.Sprintf("Upstream transport error (%d): %s", failoverErr.StatusCode, sanitized)}
				}
				return accountTestErrorDetail{message: "Upstream transport error: " + sanitized}
			}
		}
		if msg := strings.TrimSpace(failoverErr.TransportError); msg != "" {
			sanitized := SanitizeUpstreamErrorMessage(msg)
			if failoverErr.StatusCode > 0 {
				return accountTestErrorDetail{message: fmt.Sprintf("Upstream transport error (%d): %s", failoverErr.StatusCode, sanitized)}
			}
			return accountTestErrorDetail{message: "Upstream transport error: " + sanitized}
		}
		if msg := strings.TrimSpace(failoverErr.ClientMessage); msg != "" {
			sanitized := SanitizeUpstreamErrorMessage(msg)
			if failoverErr.StatusCode > 0 {
				return accountTestErrorDetail{message: fmt.Sprintf("Upstream returned %d: %s", failoverErr.StatusCode, sanitized)}
			}
			return accountTestErrorDetail{message: sanitized}
		}
		return accountTestErrorDetail{message: SanitizeUpstreamErrorMessage(err.Error())}
	}

	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if state != nil {
			status, body, transportErr := state.lastUpstreamError()
			if status >= 400 && len(bytes.TrimSpace(body)) > 0 {
				return buildAccountTestUpstreamError("Upstream", status, body)
			}
			if transportErr != nil && !errors.Is(transportErr, context.Canceled) && !errors.Is(transportErr, context.DeadlineExceeded) {
				return accountTestErrorDetail{message: "Upstream transport error: " + SanitizeUpstreamErrorMessage(transportErr.Error())}
			}
		}
		if writer != nil && writer.statusCode >= 400 && writer.errorBody.Len() > 0 {
			return writer.errorDetail()
		}
	}
	return accountTestErrorDetail{message: SanitizeUpstreamErrorMessage(err.Error())}
}
