package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// UpstreamRequestIDHeaderNames enumerates canonical header names that carry an
// upstream request identifier across Anthropic, OpenAI, Google, xAI, AWS, and
// common edge gateways.
var UpstreamRequestIDHeaderNames = []string{
	"x-request-id",
	"anthropic-request-id",
	"x-amzn-requestid",
	"xai-request-id",
	"x-goog-request-id",
	"x-ms-request-id",
	"request-id",
	"cf-ray",
}

// UpstreamTraceIDHeaderNames enumerates canonical header names that carry an
// upstream distributed trace identifier.
var UpstreamTraceIDHeaderNames = []string{
	"x-trace-id",
	"trace-id",
	"traceparent",
	"tracestate",
	"x-b3-traceid",
	"x-b3-spanid",
	"x-cloud-trace-context",
	"x-amzn-trace-id",
}

var upstreamRequestIDBodyPaths = []string{
	"error.request_id",
	"error.requestId",
	"response.error.request_id",
	"response.error.requestId",
	"request_id",
	"requestId",
	"response.request_id",
	"response.requestId",
}

var upstreamTraceIDBodyPaths = []string{
	"error.trace_id",
	"error.traceId",
	"response.error.trace_id",
	"response.error.traceId",
	"trace_id",
	"traceId",
	"response.trace_id",
	"response.traceId",
}

func cleanHeaderDiagnosticValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 512 || strings.ContainsAny(v, "\r\n") {
		return ""
	}
	return v
}

// ExtractUpstreamRequestID extracts the primary upstream request identifier
// from response headers or, if absent, from the upstream JSON error body.
func ExtractUpstreamRequestID(headers http.Header, body ...[]byte) string {
	if headers != nil {
		for _, name := range UpstreamRequestIDHeaderNames {
			if val := cleanHeaderDiagnosticValue(headers.Get(name)); val != "" {
				return val
			}
		}
	}
	if len(body) > 0 && len(body[0]) > 0 {
		for _, path := range upstreamRequestIDBodyPaths {
			if res := gjson.GetBytes(body[0], path); res.Exists() && res.Type == gjson.String {
				if val := cleanHeaderDiagnosticValue(res.String()); val != "" {
					return val
				}
			}
		}
	}
	return ""
}

// ExtractUpstreamTraceID extracts the primary upstream trace identifier from
// response headers or, if absent, from the upstream JSON error body.
func ExtractUpstreamTraceID(headers http.Header, body ...[]byte) string {
	if headers != nil {
		for _, name := range UpstreamTraceIDHeaderNames {
			if name == "tracestate" || name == "x-b3-spanid" {
				continue
			}
			if val := cleanHeaderDiagnosticValue(headers.Get(name)); val != "" {
				return val
			}
		}
	}
	if len(body) > 0 && len(body[0]) > 0 {
		for _, path := range upstreamTraceIDBodyPaths {
			if res := gjson.GetBytes(body[0], path); res.Exists() && res.Type == gjson.String {
				if val := cleanHeaderDiagnosticValue(res.String()); val != "" {
					return val
				}
			}
		}
	}
	return ""
}

// PropagateUpstreamRequestAndTraceHeaders copies recognized upstream request-id
// and trace-id headers onto the downstream Gin response writer, and aligns
// x-request-id / x-trace-id when the upstream used a variant header or embedded
// the identifier inside the error body.
func PropagateUpstreamRequestAndTraceHeaders(c *gin.Context, headers http.Header, explicitRequestID string, body ...[]byte) string {
	resolvedReqID := cleanHeaderDiagnosticValue(explicitRequestID)
	if rID := ExtractUpstreamRequestID(headers, body...); rID != "" && resolvedReqID == "" {
		resolvedReqID = rID
	}
	resolvedTraceID := ExtractUpstreamTraceID(headers, body...)

	if c == nil || c.Writer == nil {
		return resolvedReqID
	}
	dst := c.Writer.Header()
	if dst == nil {
		return resolvedReqID
	}

	if headers != nil {
		for _, name := range UpstreamRequestIDHeaderNames {
			if val := cleanHeaderDiagnosticValue(headers.Get(name)); val != "" && dst.Get(name) == "" {
				c.Header(name, val)
			}
		}
		for _, name := range UpstreamTraceIDHeaderNames {
			if val := cleanHeaderDiagnosticValue(headers.Get(name)); val != "" && dst.Get(name) == "" {
				c.Header(name, val)
			}
		}
	}

	if resolvedReqID != "" && dst.Get("x-request-id") == "" {
		c.Header("x-request-id", resolvedReqID)
	}
	if resolvedTraceID != "" && dst.Get("x-trace-id") == "" {
		c.Header("x-trace-id", resolvedTraceID)
	}
	return resolvedReqID
}

// writeSameProtocolClaudeError writes an upstream error response for a
// Claude-protocol endpoint (/v1/messages). When the upstream returned a valid
// JSON object, all original fields, nested details/param objects, vendor fields,
// and 64-bit integers are preserved verbatim in raw JSON (after credential
// literal sanitization), while ensuring top-level "type":"error" and
// "error.type"/"error.message" exist.
func writeSameProtocolClaudeError(c *gin.Context, statusCode int, body []byte, defaultType, defaultMsg string) {
	if out, ok := prepareSameProtocolClaudeErrorJSON(body, defaultType, defaultMsg); ok {
		c.Data(statusCode, "application/json", out)
		return
	}
	WriteProjectedClaudeError(c, statusCode, body, defaultType, defaultMsg)
}

func prepareSameProtocolClaudeErrorJSON(body []byte, defaultType, defaultMsg string) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || looksLikeHTMLBody(trimmed) || !gjson.ValidBytes(trimmed) {
		return nil, false
	}
	out := sanitizeUpstreamErrorBody(trimmed)
	if !gjson.ValidBytes(out) {
		return nil, false
	}

	errNode := gjson.GetBytes(out, "error")
	if errNode.IsObject() {
		if strings.TrimSpace(gjson.GetBytes(out, "type").String()) == "" {
			if patched, err := sjson.SetBytes(out, "type", "error"); err == nil {
				out = patched
			}
		}
		if strings.TrimSpace(gjson.GetBytes(out, "error.type").String()) == "" && defaultType != "" {
			if patched, err := sjson.SetBytes(out, "error.type", defaultType); err == nil {
				out = patched
			}
		}
		if strings.TrimSpace(gjson.GetBytes(out, "error.message").String()) == "" && defaultMsg != "" {
			if patched, err := sjson.SetBytes(out, "error.message", defaultMsg); err == nil {
				out = patched
			}
		}
		return out, true
	}

	// If top-level JSON object is a wrapped response.error or a flat error object,
	// project all fields into the error envelope while preserving raw JSON numbers
	// and top-level extra fields.
	projected := BuildProjectedErrorMap(out, defaultType, defaultMsg)
	rawErr, err := json.Marshal(projected)
	if err != nil {
		return nil, false
	}
	if strings.TrimSpace(gjson.GetBytes(out, "type").String()) == "" {
		if patched, err := sjson.SetBytes(out, "type", "error"); err == nil {
			out = patched
		}
	}
	if patched, err := sjson.SetRawBytes(out, "error", rawErr); err == nil {
		return patched, true
	}
	return nil, false
}

// writeSameProtocolOpenAIError writes an upstream error response for an
// OpenAI-protocol endpoint. When the upstream returned a valid JSON object, all
// original fields, nested details/param objects, vendor fields, and 64-bit
// integers are preserved verbatim in raw JSON (after credential literal
// sanitization), while ensuring "error.type" and "error.message" exist.
func writeSameProtocolOpenAIError(c *gin.Context, statusCode int, body []byte, defaultType, defaultMsg string, overrideSanitizedMsg ...string) {
	if out, ok := prepareSameProtocolOpenAIErrorJSON(body, defaultType, defaultMsg, overrideSanitizedMsg...); ok {
		c.Data(statusCode, "application/json", out)
		return
	}
	msg := defaultMsg
	if len(overrideSanitizedMsg) > 0 && strings.TrimSpace(overrideSanitizedMsg[0]) != "" {
		msg = strings.TrimSpace(overrideSanitizedMsg[0])
	}
	WriteProjectedOpenAIError(c, statusCode, body, defaultType, msg)
}

func prepareSameProtocolOpenAIErrorJSON(body []byte, defaultType, defaultMsg string, overrideSanitizedMsg ...string) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || looksLikeHTMLBody(trimmed) || !gjson.ValidBytes(trimmed) {
		return nil, false
	}
	out := sanitizeUpstreamErrorBody(trimmed)
	if !gjson.ValidBytes(out) {
		return nil, false
	}

	explicitMsg := ""
	if len(overrideSanitizedMsg) > 0 {
		explicitMsg = strings.TrimSpace(overrideSanitizedMsg[0])
	}

	errNode := gjson.GetBytes(out, "error")
	if errNode.IsObject() {
		if strings.TrimSpace(gjson.GetBytes(out, "error.type").String()) == "" && defaultType != "" {
			if patched, err := sjson.SetBytes(out, "error.type", defaultType); err == nil {
				out = patched
			}
		}
		if explicitMsg != "" {
			if patched, err := sjson.SetBytes(out, "error.message", explicitMsg); err == nil {
				out = patched
			}
		} else if strings.TrimSpace(gjson.GetBytes(out, "error.message").String()) == "" && defaultMsg != "" {
			if patched, err := sjson.SetBytes(out, "error.message", defaultMsg); err == nil {
				out = patched
			}
		}
		return out, true
	}

	msgFallback := defaultMsg
	if explicitMsg != "" {
		msgFallback = explicitMsg
	}
	projected := BuildProjectedErrorMap(out, defaultType, msgFallback)
	if explicitMsg != "" {
		projected["message"] = explicitMsg
	}
	rawErr, err := json.Marshal(projected)
	if err != nil {
		return nil, false
	}
	if patched, err := sjson.SetRawBytes(out, "error", rawErr); err == nil {
		return patched, true
	}
	return nil, false
}

// extractUpstreamRawDiagnosticFields extracts structured fields from an
// upstream error body (supporting {"response":{"error":{...}}}, {"error":{...}},
// or flat JSON error objects) with values kept as sanitized json.RawMessage so
// arbitrary nested objects and 64-bit integers are preserved losslessly.
func extractUpstreamRawDiagnosticFields(body []byte) map[string]json.RawMessage {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || looksLikeHTMLBody(trimmed) || !gjson.ValidBytes(trimmed) {
		return nil
	}
	sanitized := sanitizeUpstreamErrorBody(trimmed)
	if !gjson.ValidBytes(sanitized) {
		return nil
	}

	fields := make(map[string]json.RawMessage)
	collectObject := func(obj gjson.Result, overwrite bool) {
		if !obj.IsObject() {
			return
		}
		obj.ForEach(func(k, v gjson.Result) bool {
			key := strings.TrimSpace(k.String())
			if key == "" {
				return true
			}
			if !overwrite {
				if _, exists := fields[key]; exists {
					return true
				}
			}
			raw := bytes.TrimSpace([]byte(v.Raw))
			if len(raw) > 0 && gjson.ValidBytes(raw) {
				fields[key] = json.RawMessage(raw)
			}
			return true
		})
	}

	root := gjson.ParseBytes(sanitized)
	respErr := root.Get("response.error")
	topErr := root.Get("error")

	switch {
	case respErr.IsObject():
		collectObject(respErr, true)
	case topErr.IsObject():
		collectObject(topErr, true)
	default:
		root.ForEach(func(k, v gjson.Result) bool {
			key := strings.TrimSpace(k.String())
			if key == "" || key == "response" || (key == "type" && v.Type == gjson.String && v.String() == "error") {
				return true
			}
			raw := bytes.TrimSpace([]byte(v.Raw))
			if len(raw) > 0 && gjson.ValidBytes(raw) {
				fields[key] = json.RawMessage(raw)
			}
			return true
		})
	}

	// If error.message itself contains an embedded JSON object, backfill any
	// missing diagnostic fields from that inner JSON object.
	if msgRaw, ok := fields["message"]; ok {
		var msgStr string
		if json.Unmarshal(msgRaw, &msgStr) == nil {
			innerTrimmed := strings.TrimSpace(msgStr)
			if strings.HasPrefix(innerTrimmed, "{") && gjson.Valid(innerTrimmed) {
				innerRoot := gjson.Parse(innerTrimmed)
				if innerErr := innerRoot.Get("error"); innerErr.IsObject() {
					collectObject(innerErr, false)
				} else {
					collectObject(innerRoot, false)
				}
			}
		}
	}

	// Also backfill top-level or response-level alignment fields (such as
	// request_id, requestId, trace_id, traceId, details, param, code) when placed
	// outside the inner error object.
	for _, outer := range []gjson.Result{root.Get("response"), root} {
		if !outer.IsObject() {
			continue
		}
		outer.ForEach(func(k, v gjson.Result) bool {
			key := strings.TrimSpace(k.String())
			switch key {
			case "", "error", "response", "object", "id", "created_at", "output", "candidates", "choices", "usage":
				return true
			case "type":
				if v.Type == gjson.String && (v.String() == "error" || v.String() == "response.failed") {
					return true
				}
			}
			if _, exists := fields[key]; !exists {
				raw := bytes.TrimSpace([]byte(v.Raw))
				if len(raw) > 0 && gjson.ValidBytes(raw) {
					fields[key] = json.RawMessage(raw)
				}
			}
			return true
		})
	}

	return fields
}

// ResolveUpstreamErrorType resolves the most specific error type from the
// upstream body, preferring an explicit error.type / response.error.type, then
// mapping Google status codes if present, and finally falling back to defaultType.
func ResolveUpstreamErrorType(body []byte, defaultType string) string {
	for _, path := range []string{"error.type", "response.error.type"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.String {
			if s := strings.TrimSpace(v.String()); s != "" && s != "error" {
				return s
			}
		}
	}
	for _, path := range []string{"error.status", "response.error.status", "status"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.String {
			if st := strings.TrimSpace(v.String()); st != "" {
				if mapped := mapGeminiStatusToClaudeErrorType(st); mapped != "" {
					return mapped
				}
				if defaultType == "" || defaultType == "upstream_error" || defaultType == "server_error" {
					return st
				}
			}
		}
	}
	if upstreamType := strings.TrimSpace(extractUpstreamErrorType(body)); upstreamType != "" && upstreamType != "error" {
		if defaultType == "" || defaultType == "upstream_error" || defaultType == "server_error" {
			return upstreamType
		}
	}
	return defaultType
}

// BuildProjectedErrorMap builds a cross-protocol error object map that
// preserves all structured diagnostic fields (code, param, details, status,
// requestId/request_id, traceId/trace_id, vendor fields, and 64-bit numbers)
// alongside normalized "type" and "message".
func BuildProjectedErrorMap(body []byte, defaultType, defaultMsg string) gin.H {
	rawFields := extractUpstreamRawDiagnosticFields(body)
	out := make(gin.H, len(rawFields)+2)
	for k, v := range rawFields {
		out[k] = v
	}

	errType := ResolveUpstreamErrorType(body, defaultType)
	if errType == "" {
		errType = "upstream_error"
	}
	out["type"] = errType

	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	errMsg := getPassthroughOrDefault(upstreamMsg, defaultMsg)
	if errMsg == "" {
		errMsg = "Upstream request failed"
	}
	out["message"] = errMsg

	if _, hasCode := out["code"]; !hasCode {
		if code := strings.TrimSpace(extractUpstreamErrorCode(body)); code != "" {
			out["code"] = code
		}
	}
	return out
}

// WriteProjectedClaudeError writes a Claude-compatible {"type":"error","error":{...}}
// payload that retains all upstream diagnostic fields.
func WriteProjectedClaudeError(c *gin.Context, statusCode int, body []byte, defaultType, defaultMsg string) {
	errorObj := BuildProjectedErrorMap(body, defaultType, defaultMsg)
	c.JSON(statusCode, gin.H{
		"type":  "error",
		"error": errorObj,
	})
}

// WriteProjectedOpenAIError writes an OpenAI-compatible {"error":{...}} payload
// that retains all upstream diagnostic fields.
func WriteProjectedOpenAIError(c *gin.Context, statusCode int, body []byte, defaultType, defaultMsg string) {
	errorObj := BuildProjectedErrorMap(body, defaultType, defaultMsg)
	c.JSON(statusCode, gin.H{
		"error": errorObj,
	})
}

// BuildProjectedResponsesErrorMap builds an OpenAI Responses API error object
// {"code":..., "message":..., ...} that retains all upstream diagnostic fields.
func BuildProjectedResponsesErrorMap(body []byte, defaultCode, defaultMsg string) gin.H {
	rawFields := extractUpstreamRawDiagnosticFields(body)
	out := make(gin.H, len(rawFields)+2)
	for k, v := range rawFields {
		out[k] = v
	}

	code := strings.TrimSpace(extractUpstreamErrorCode(body))
	if code == "" {
		code = ResolveUpstreamErrorType(body, defaultCode)
	}
	if code == "" {
		code = defaultCode
	}
	if code == "" {
		code = "server_error"
	}
	out["code"] = code

	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	errMsg := getPassthroughOrDefault(upstreamMsg, defaultMsg)
	if errMsg == "" {
		errMsg = "Upstream request failed"
	}
	out["message"] = errMsg
	return out
}

// WriteProjectedResponsesError writes an OpenAI Responses API {"error":{...}}
// payload that retains all upstream diagnostic fields.
func WriteProjectedResponsesError(c *gin.Context, statusCode int, body []byte, defaultCode, defaultMsg string) {
	errorObj := BuildProjectedResponsesErrorMap(body, defaultCode, defaultMsg)
	c.JSON(statusCode, gin.H{
		"error": errorObj,
	})
}

// WriteProjectedGoogleError writes a Google/Gemini RPC error envelope while
// preserving upstream structured details, param, requestId/traceId, and vendor
// diagnostic fields.
func WriteProjectedGoogleError(c *gin.Context, statusCode int, body []byte, defaultStatus, defaultMsg string) {
	rawFields := extractUpstreamRawDiagnosticFields(body)
	errorObj := make(gin.H, len(rawFields)+3)
	for k, v := range rawFields {
		errorObj[k] = v
	}
	errorObj["code"] = statusCode

	googleStatus := defaultStatus
	for _, path := range []string{"error.status", "response.error.status", "status"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.String {
			if st := strings.TrimSpace(v.String()); st != "" {
				googleStatus = st
				break
			}
		}
	}
	if googleStatus == "" {
		googleStatus = streamPreheaderGoogleStatus(statusCode, "")
	}
	errorObj["status"] = googleStatus

	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	errMsg := getPassthroughOrDefault(upstreamMsg, defaultMsg)
	if errMsg == "" {
		errMsg = "Upstream request failed"
	}
	errorObj["message"] = errMsg

	c.JSON(statusCode, gin.H{
		"error": errorObj,
	})
}

// mergeStreamPreheaderFailureDiagnostics merges upstream structured diagnostic
// fields (details, param, requestId/traceId, vendor fields, and upstream code)
// into a preheader terminal failure object while keeping the gateway's terminal
// contract fields (code="gateway_stream_terminal_error", type, message, status,
// retry_after) authoritative.
func mergeStreamPreheaderFailureDiagnostics(failure gin.H, body []byte, headers http.Header) {
	if failure == nil {
		return
	}
	rawFields := extractUpstreamRawDiagnosticFields(body)
	for k, v := range rawFields {
		switch k {
		case "message", "retry_after":
			continue
		case "code":
			var codeStr string
			if json.Unmarshal(v, &codeStr) == nil {
				if strings.TrimSpace(codeStr) != "" && codeStr != streamPreheaderTerminalCode {
					failure["upstream_code"] = codeStr
				}
			} else {
				failure["upstream_code"] = v
			}
		case "type":
			var typeStr string
			if json.Unmarshal(v, &typeStr) == nil {
				if strings.TrimSpace(typeStr) != "" && typeStr != "error" && typeStr != failure["type"] {
					failure["upstream_type"] = typeStr
				}
			}
		case "status":
			var statusStr string
			if json.Unmarshal(v, &statusStr) == nil && strings.TrimSpace(statusStr) != "" {
				failure["upstream_status"] = statusStr
			}
		default:
			if _, exists := failure[k]; !exists {
				failure[k] = v
			}
		}
	}

	if len(rawFields) > 0 {
		if _, hasReqID := failure["request_id"]; !hasReqID {
			if _, hasCamelReqID := failure["requestId"]; !hasCamelReqID {
				if reqID := ExtractUpstreamRequestID(headers, body); reqID != "" {
					failure["request_id"] = reqID
				}
			}
		}
		if _, hasTraceID := failure["trace_id"]; !hasTraceID {
			if _, hasCamelTraceID := failure["traceId"]; !hasCamelTraceID {
				if traceID := ExtractUpstreamTraceID(headers, body); traceID != "" {
					failure["trace_id"] = traceID
				}
			}
		}
	}
}

// mergeStreamPreheaderGeminiDiagnostics merges upstream structured details and
// diagnostic fields into a Gemini preheader terminal error envelope while
// keeping the gateway's ErrorInfo (reason="GATEWAY_STREAM_TERMINAL_ERROR") and
// RetryInfo authoritative.
func mergeStreamPreheaderGeminiDiagnostics(errorObj gin.H, details []any, body []byte, headers http.Header) []any {
	rawFields := extractUpstreamRawDiagnosticFields(body)
	if len(rawFields) == 0 {
		return details
	}

	if rawDetails, ok := rawFields["details"]; ok {
		parsedDetails := gjson.ParseBytes(rawDetails)
		if parsedDetails.IsArray() {
			parsedDetails.ForEach(func(_, item gjson.Result) bool {
				itemType := strings.TrimSpace(item.Get("@type").String())
				if itemType == googleRPCTypeRetryInfo {
					return true
				}
				if itemType == "type.googleapis.com/google.rpc.ErrorInfo" &&
					strings.TrimSpace(item.Get("domain").String()) == "sub2api.gateway" {
					return true
				}
				rawItem := bytes.TrimSpace([]byte(item.Raw))
				if len(rawItem) > 0 && gjson.ValidBytes(rawItem) {
					details = append(details, json.RawMessage(rawItem))
				}
				return true
			})
		}
	}

	for k, v := range rawFields {
		switch k {
		case "code", "message", "status", "details":
			continue
		default:
			if _, exists := errorObj[k]; !exists {
				errorObj[k] = v
			}
		}
	}

	if _, hasReqID := errorObj["request_id"]; !hasReqID {
		if _, hasCamelReqID := errorObj["requestId"]; !hasCamelReqID {
			if reqID := ExtractUpstreamRequestID(headers, body); reqID != "" {
				errorObj["request_id"] = reqID
			}
		}
	}
	if _, hasTraceID := errorObj["trace_id"]; !hasTraceID {
		if _, hasCamelTraceID := errorObj["traceId"]; !hasCamelTraceID {
			if traceID := ExtractUpstreamTraceID(headers, body); traceID != "" {
				errorObj["trace_id"] = traceID
			}
		}
	}
	return details
}
