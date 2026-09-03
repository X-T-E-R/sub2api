// Package codextelemetry retains a bounded allowlist of upstream observations.
// It does not identify public models or participate in routing or billing.
package codextelemetry

import (
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	MaxSnapshotBytes = 4096
	MaxEventBytes    = 16 * 1024
	MaxEvents        = 32
	MaxObservations  = 8
	MaxEngineIDs     = 8
	MaxIDBytes       = 128
	MaxWindowMinutes = 366 * 24 * 60

	HTTP      = "http"
	WebSocket = "websocket"

	HTTPHeaders      = "http_headers"
	WSUpgradeHeaders = "ws_upgrade_headers"
	TimingEvent      = "responsesapi.websocket_timing"
	CodexMetadata    = "codex.response.metadata"
	ResponseMetadata = "response.metadata"
	ErrorHeaders     = "error_headers"

	ExplicitResponse = "response_id"
	ActiveResponse   = "active_response"
	HTTPResponse     = "http_response"
	Connection       = "connection"
	UpstreamAttempt  = "upstream_attempt"
)

// Snapshot is an owned, optional value for one forwarding attempt or WS turn.
// ResponseID identifies the bound response, not an ID supplied by an ID-less
// event. Association on each observation distinguishes those two cases.
type Snapshot struct {
	Version            int           `json:"v"`
	Transport          string        `json:"transport"`
	ResponseID         string        `json:"response_id,omitempty"`
	StreamID           string        `json:"stream_id,omitempty"`
	ConnectionReused   bool          `json:"connection_reused,omitempty"`
	Observations       []Observation `json:"observations"`
	UnassociatedEvents int           `json:"unassociated_events,omitempty"`
	Truncated          bool          `json:"truncated,omitempty"`
}

type Observation struct {
	Source                 string   `json:"source"`
	Association            string   `json:"association"`
	EngineIDs              []string `json:"engine_ids,omitempty"`
	FasterModel            string   `json:"faster_model,omitempty"`
	ActiveLimit            string   `json:"active_limit,omitempty"`
	PrimaryUsedPercent     *float64 `json:"primary_used_percent,omitempty"`
	PrimaryWindowMinutes   *int     `json:"primary_window_minutes,omitempty"`
	SecondaryUsedPercent   *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryWindowMinutes *int     `json:"secondary_window_minutes,omitempty"`
}

func (o Observation) empty() bool {
	return len(o.EngineIDs) == 0 && o.FasterModel == "" && o.ActiveLimit == "" &&
		o.PrimaryUsedPercent == nil && o.PrimaryWindowMinutes == nil &&
		o.SecondaryUsedPercent == nil && o.SecondaryWindowMinutes == nil
}

// Collector is confined to its forwarding goroutine. Snapshot copies all
// mutable fields before the value crosses an asynchronous persistence boundary.
type Collector struct {
	data      Snapshot
	events    [4]int
	active    bool
	closed    bool
	ambiguous bool
}

func New(transport string, connectionReused bool) *Collector {
	return &Collector{
		data:   Snapshot{Version: 1, Transport: transport, ConnectionReused: connectionReused},
		active: transport == HTTP,
	}
}

func IsMetadataEvent(eventType string) bool {
	return eventType == TimingEvent || eventType == CodexMetadata || eventType == ResponseMetadata
}

func metadataBudget(eventType string) (int, int) {
	switch eventType {
	case TimingEvent:
		return 0, MaxEvents / 2
	case CodexMetadata:
		return 1, MaxEvents / 8
	case ResponseMetadata:
		return 2, MaxEvents / 8
	default:
		return 3, MaxEvents / 4
	}
}

// BlockContext invalidates event observations when multiple responses or named
// lanes make the first observed response ambiguous. Actual headers retain their
// HTTP-response or connection scope.
func (c *Collector) BlockContext() {
	if c == nil {
		return
	}
	c.ambiguous = true
	c.data.Observations = slices.DeleteFunc(c.data.Observations, func(o Observation) bool {
		return o.Source != HTTPHeaders && o.Source != WSUpgradeHeaders
	})
}

func (c *Collector) BindResponse(responseID, streamID string) {
	if c == nil || c.closed || !validID(responseID) || (streamID != "" && !validID(streamID)) {
		return
	}
	if c.data.ResponseID != "" && c.data.ResponseID != responseID {
		c.BlockContext()
		return
	}
	if c.data.StreamID != "" && streamID != "" && c.data.StreamID != streamID {
		c.BlockContext()
		return
	}
	c.data.ResponseID = strings.Clone(responseID)
	if streamID != "" {
		c.data.StreamID = strings.Clone(streamID)
	}
	c.active = true
}

func (c *Collector) Close() {
	if c != nil {
		c.closed = true
	}
}

func (c *Collector) Closed() bool { return c == nil || c.closed }

func (c *Collector) ResponseID() string {
	if c == nil {
		return ""
	}
	return c.data.ResponseID
}

// ObserveHeaders must receive actual HTTP response headers, or a fresh upgrade
// exactly once. Callers omit cached/reused/prewarmed handshake snapshots.
func (c *Collector) ObserveHeaders(headers http.Header, source string) {
	if c == nil || c.closed || (source != HTTPHeaders && source != WSUpgradeHeaders) {
		return
	}
	values := make(map[string]string, 6)
	seen := make(map[string]bool, 6)
	for key, entries := range headers {
		key = strings.ToLower(key)
		if !allowedHeader(key) {
			continue
		}
		if seen[key] || len(entries) != 1 {
			delete(values, key)
			seen[key] = true
			continue
		}
		seen[key] = true
		if len(entries[0]) <= MaxIDBytes {
			values[key] = entries[0]
		}
	}
	o := observationFromHeaders(values)
	o.Source = source
	o.Association = HTTPResponse
	if source == WSUpgradeHeaders {
		o.Association = Connection
	}
	c.add(o)
}

// Observe reads only metadata and response identity. It returns true for a valid
// explicit ID in the caller's completed-response list, which is not retained.
// The caller still owns event forwarding, terminal decisions and transport lifetime.
func (c *Collector) Observe(payload []byte, eventType string, completedResponseIDs ...string) (ignored bool) {
	if c == nil || c.closed {
		return
	}
	metadata := IsMetadataEvent(eventType) || eventType == "error"
	terminal := isTerminal(eventType)
	if terminal {
		defer func() {
			if !ignored {
				c.Close()
			}
		}()
	}
	lifecycle := eventType == "response.created" || eventType == "response.in_progress" || terminal
	if !metadata && !lifecycle {
		return
	}
	if metadata {
		bucket, budget := metadataBudget(eventType)
		if c.events[bucket] >= budget {
			c.data.Truncated = true
			return
		}
		c.events[bucket]++
		if len(payload) > MaxEventBytes {
			c.data.Truncated = true
			return
		}
	}
	if !gjson.ValidBytes(payload) {
		if terminal {
			c.Close()
		}
		return
	}
	root, ok := uniqueObject(gjson.ParseBytes(payload))
	if !ok {
		c.BlockContext()
		if terminal {
			c.Close()
		}
		return
	}
	if typ, exists := root["type"]; exists && (typ.Type != gjson.String || typ.String() != eventType) {
		return
	}
	responseID, streamID, valid := eventIdentity(root)
	if !valid {
		c.BlockContext()
		if metadata {
			c.unassociated()
		}
		if terminal {
			c.Close()
		}
		return
	}
	if responseID != "" && slices.Contains(completedResponseIDs, responseID) {
		return true
	}
	if lifecycle {
		c.BindResponse(responseID, streamID)
		if terminal {
			c.Close()
		}
		return
	}
	if c.ambiguous {
		c.unassociated()
		return
	}
	if (c.data.ResponseID != "" && responseID != "" && responseID != c.data.ResponseID) ||
		(c.data.StreamID != "" && streamID != "" && streamID != c.data.StreamID) {
		c.BlockContext()
		c.unassociated()
		return
	}
	association := ExplicitResponse
	if responseID != "" {
		if c.data.ResponseID == "" {
			c.unassociated()
			return
		}
	} else {
		if eventType == "error" && c.data.ResponseID == "" && streamID == "" {
			association = UpstreamAttempt
		} else if !c.active || (streamID != "" && streamID != c.data.StreamID) {
			c.unassociated()
			return
		} else {
			association = ActiveResponse
			if c.data.Transport == HTTP {
				association = HTTPResponse
			}
		}
	}
	o := Observation{Source: eventType, Association: association}
	if eventType == TimingEvent {
		metrics, ok := uniqueObject(root["timing_metrics"])
		if !ok || !metrics["engine_ids"].IsArray() {
			return
		}
		count := 0
		metrics["engine_ids"].ForEach(func(_, value gjson.Result) bool {
			count++
			if count > 64 {
				c.data.Truncated = true
				return false
			}
			if value.Type != gjson.String || !validID(value.String()) {
				return true
			}
			id := value.String()
			if slices.Contains(o.EngineIDs, id) {
				return true
			}
			if len(o.EngineIDs) >= MaxEngineIDs {
				c.data.Truncated = true
				return true
			}
			o.EngineIDs = append(o.EngineIDs, strings.Clone(id))
			return true
		})
	} else {
		headers, ok := uniqueObject(root["headers"])
		if !ok {
			return
		}
		values := make(map[string]string, 6)
		seen := make(map[string]bool, 6)
		for key, value := range headers {
			key = strings.ToLower(key)
			if !allowedHeader(key) {
				continue
			}
			if seen[key] {
				delete(values, key)
				continue
			}
			seen[key] = true
			if value.Type == gjson.String && len(value.String()) <= MaxIDBytes {
				values[key] = value.String()
			}
		}
		o = observationFromHeaders(values)
		o.Source, o.Association = eventType, association
		if eventType == "error" {
			o.Source = ErrorHeaders
		}
	}
	c.add(o)
	return
}

func (c *Collector) unassociated() {
	if c.data.UnassociatedEvents < 255 {
		c.data.UnassociatedEvents++
	}
}

func (c *Collector) add(o Observation) {
	if o.empty() {
		return
	}
	for _, prior := range c.data.Observations {
		if equalObservation(prior, o) {
			return
		}
	}
	if len(c.data.Observations) >= MaxObservations {
		c.data.Truncated = true
		c.data.Observations = c.data.Observations[1:]
	}
	c.data.Observations = append(c.data.Observations, o)
}

// Snapshot returns nil when there is no allowed observation. Its JSON encoding
// is at most MaxSnapshotBytes, including flags, identities and JSON escaping.
func (c *Collector) Snapshot() *Snapshot {
	if c == nil {
		return nil
	}
	snapshot := c.data
	if c.ambiguous {
		snapshot.ResponseID, snapshot.StreamID = "", ""
	}
	return Clone(&snapshot)
}

// Clone and Marshal recheck the storage boundary as well as owning the data.
// They may also be used by Ops queue sanitization before asynchronous writes.
func Clone(snapshot *Snapshot) *Snapshot {
	raw := Marshal(snapshot)
	if len(raw) == 0 {
		return nil
	}
	var owned Snapshot
	if json.Unmarshal(raw, &owned) != nil {
		return nil
	}
	return &owned
}

func Marshal(snapshot *Snapshot) []byte {
	return MarshalLimit(snapshot, MaxSnapshotBytes)
}

// MarshalLimit gives an existing Ops row a shared budget across failed attempts.
// Newer observations survive before older observations when space is limited.
func MarshalLimit(input *Snapshot, limit int) []byte {
	if input == nil || input.Version != 1 || (input.Transport != HTTP && input.Transport != WebSocket) || limit <= 0 {
		return nil
	}
	if limit > MaxSnapshotBytes {
		limit = MaxSnapshotBytes
	}
	snapshot := *input
	if !validID(snapshot.ResponseID) {
		snapshot.ResponseID = ""
	}
	if !validID(snapshot.StreamID) {
		snapshot.StreamID = ""
	}
	snapshot.UnassociatedEvents = min(max(snapshot.UnassociatedEvents, 0), 255)
	snapshot.Observations = nil
	start := max(0, len(input.Observations)-MaxObservations)
	snapshot.Truncated = snapshot.Truncated || start > 0
	for _, observation := range input.Observations[start:] {
		o := normalizeObservation(observation)
		if !o.empty() {
			snapshot.Observations = append(snapshot.Observations, o)
		}
	}
	for len(snapshot.Observations) > 0 {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return nil
		}
		if len(raw) <= limit {
			return raw
		}
		snapshot.Truncated = true
		if len(snapshot.Observations) == 1 && len(snapshot.Observations[0].EngineIDs) > 1 {
			snapshot.Observations[0].EngineIDs = snapshot.Observations[0].EngineIDs[1:]
		} else {
			snapshot.Observations = snapshot.Observations[1:]
		}
	}
	return nil
}

func normalizeObservation(input Observation) Observation {
	switch input.Association {
	case ExplicitResponse, ActiveResponse, HTTPResponse, Connection, UpstreamAttempt:
	default:
		return Observation{}
	}
	o := Observation{Source: input.Source, Association: input.Association}
	if input.Source == TimingEvent {
		if input.Association != ExplicitResponse && input.Association != ActiveResponse && input.Association != HTTPResponse {
			return Observation{}
		}
		for _, id := range input.EngineIDs[:min(len(input.EngineIDs), 64)] {
			if validID(id) && !slices.Contains(o.EngineIDs, id) && len(o.EngineIDs) < MaxEngineIDs {
				o.EngineIDs = append(o.EngineIDs, id)
			}
		}
		return o
	}
	switch input.Source {
	case HTTPHeaders:
		if input.Association != HTTPResponse {
			return Observation{}
		}
	case WSUpgradeHeaders:
		if input.Association != Connection {
			return Observation{}
		}
	case CodexMetadata, ResponseMetadata, ErrorHeaders:
	default:
		return Observation{}
	}
	if validID(input.FasterModel) {
		o.FasterModel = input.FasterModel
	}
	if validID(input.ActiveLimit) {
		o.ActiveLimit = input.ActiveLimit
	}
	percent := func(value *float64) *float64 {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100 {
			return nil
		}
		copy := *value
		return &copy
	}
	window := func(value *int) *int {
		if value == nil || *value < 1 || *value > MaxWindowMinutes {
			return nil
		}
		copy := *value
		return &copy
	}
	o.PrimaryUsedPercent, o.SecondaryUsedPercent = percent(input.PrimaryUsedPercent), percent(input.SecondaryUsedPercent)
	o.PrimaryWindowMinutes, o.SecondaryWindowMinutes = window(input.PrimaryWindowMinutes), window(input.SecondaryWindowMinutes)
	return o
}

func equalObservation(a, b Observation) bool {
	return a.Source == b.Source && a.Association == b.Association &&
		slices.Equal(a.EngineIDs, b.EngineIDs) && a.FasterModel == b.FasterModel && a.ActiveLimit == b.ActiveLimit &&
		equalPointer(a.PrimaryUsedPercent, b.PrimaryUsedPercent) && equalPointer(a.PrimaryWindowMinutes, b.PrimaryWindowMinutes) &&
		equalPointer(a.SecondaryUsedPercent, b.SecondaryUsedPercent) && equalPointer(a.SecondaryWindowMinutes, b.SecondaryWindowMinutes)
}

func equalPointer[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func uniqueObject(object gjson.Result) (map[string]gjson.Result, bool) {
	if !object.IsObject() {
		return nil, false
	}
	fields := make(map[string]gjson.Result)
	valid := true
	object.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, exists := fields[name]; exists || len(fields) >= 128 {
			valid = false
			return false
		}
		fields[name] = value
		return true
	})
	return fields, valid
}

func eventIdentity(root map[string]gjson.Result) (string, string, bool) {
	read := func(value gjson.Result) (string, bool) {
		if !value.Exists() {
			return "", true
		}
		return value.String(), value.Type == gjson.String && validID(value.String())
	}
	id, ok := read(root["response_id"])
	stream, streamOK := read(root["stream_id"])
	if !ok || !streamOK {
		return "", "", false
	}
	if response, exists := root["response"]; exists {
		fields, valid := uniqueObject(response)
		if !valid {
			return "", "", false
		}
		nestedID, valid := read(fields["id"])
		if !valid || (id != "" && nestedID != "" && id != nestedID) {
			return "", "", false
		}
		if nestedID != "" {
			id = nestedID
		}
	}
	return id, stream, true
}

func validID(value string) bool {
	if value == "" || len(value) > MaxIDBytes {
		return false
	}
	for _, ch := range value {
		if ch < 0x21 || ch > 0x7e {
			return false
		}
	}
	return true
}

func allowedHeader(key string) bool {
	switch key {
	case "x-codex-safety-buffering-faster-model", "x-codex-active-limit",
		"x-codex-primary-used-percent", "x-codex-primary-window-minutes",
		"x-codex-secondary-used-percent", "x-codex-secondary-window-minutes":
		return true
	default:
		return false
	}
}

func observationFromHeaders(values map[string]string) Observation {
	token := func(key string) string {
		value := strings.TrimSpace(values[key])
		if !validID(value) {
			return ""
		}
		return strings.Clone(value)
	}
	percent := func(key string) *float64 {
		value, err := strconv.ParseFloat(strings.TrimSpace(values[key]), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return nil
		}
		return &value
	}
	window := func(key string) *int {
		value, err := strconv.Atoi(strings.TrimSpace(values[key]))
		if err != nil || value < 1 || value > MaxWindowMinutes {
			return nil
		}
		return &value
	}
	return Observation{
		FasterModel:            token("x-codex-safety-buffering-faster-model"),
		ActiveLimit:            token("x-codex-active-limit"),
		PrimaryUsedPercent:     percent("x-codex-primary-used-percent"),
		PrimaryWindowMinutes:   window("x-codex-primary-window-minutes"),
		SecondaryUsedPercent:   percent("x-codex-secondary-used-percent"),
		SecondaryWindowMinutes: window("x-codex-secondary-window-minutes"),
	}
}

func isTerminal(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}
