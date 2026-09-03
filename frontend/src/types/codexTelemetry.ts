export interface CodexTelemetryObservation {
  source: string
  association: string
  engine_ids?: string[]
  faster_model?: string
  active_limit?: string
  primary_used_percent?: number
  primary_window_minutes?: number
  secondary_used_percent?: number
  secondary_window_minutes?: number
}

export interface CodexTelemetrySnapshot {
  v: 1
  transport: 'http' | 'websocket'
  response_id?: string
  stream_id?: string
  connection_reused?: boolean
  observations: CodexTelemetryObservation[]
  unassociated_events?: number
  truncated?: boolean
}

function isSnapshot(value: unknown): value is CodexTelemetrySnapshot {
  if (!value || typeof value !== 'object') return false
  const snapshot = value as Record<string, unknown>
  if (snapshot.v !== 1 || !['http', 'websocket'].includes(String(snapshot.transport))) return false
  if (!Array.isArray(snapshot.observations) || snapshot.observations.length === 0 || snapshot.observations.length > 8) return false
  for (const key of ['response_id', 'stream_id']) {
    if (snapshot[key] != null && (typeof snapshot[key] !== 'string' || snapshot[key].length > 128)) return false
  }
  for (const key of ['connection_reused', 'truncated']) {
    if (snapshot[key] != null && typeof snapshot[key] !== 'boolean') return false
  }
  if (snapshot.unassociated_events != null && (typeof snapshot.unassociated_events !== 'number' || !Number.isInteger(snapshot.unassociated_events) || snapshot.unassociated_events < 0 || snapshot.unassociated_events > 255)) return false
  return snapshot.observations.every((item: unknown) => {
    if (!item || typeof item !== 'object') return false
    const observation = item as Record<string, unknown>
    if (typeof observation.source !== 'string' || typeof observation.association !== 'string') return false
    if (!['http_headers', 'ws_upgrade_headers', 'responsesapi.websocket_timing', 'codex.response.metadata', 'response.metadata', 'error_headers'].includes(observation.source)) return false
    if (!['response_id', 'active_response', 'http_response', 'connection', 'upstream_attempt'].includes(observation.association)) return false
    if (observation.engine_ids != null && (!Array.isArray(observation.engine_ids) || observation.engine_ids.length > 8 || !observation.engine_ids.every(id => typeof id === 'string' && id.length <= 128))) return false
    for (const key of ['faster_model', 'active_limit']) {
      if (observation[key] != null && (typeof observation[key] !== 'string' || observation[key].length > 128)) return false
    }
    for (const key of ['primary_used_percent', 'primary_window_minutes', 'secondary_used_percent', 'secondary_window_minutes']) {
      if (observation[key] != null && (typeof observation[key] !== 'number' || !Number.isFinite(observation[key]))) return false
    }
    for (const key of ['primary_used_percent', 'secondary_used_percent']) {
      const value = observation[key]
      if (typeof value === 'number' && (value < 0 || value > 100)) return false
    }
    for (const key of ['primary_window_minutes', 'secondary_window_minutes']) {
      const value = observation[key]
      if (typeof value === 'number' && (!Number.isInteger(value) || value < 1 || value > 527040)) return false
    }
    return true
  })
}

// Ops stores attempt evidence as JSON; keep its typed projection in one place.
export function codexTelemetryFromOpsErrors(raw?: string | null): CodexTelemetrySnapshot[] {
  if (!raw) return []
  try {
    const events: unknown = JSON.parse(raw)
    if (!Array.isArray(events)) return []
    return events.slice(-16).flatMap(event => {
      const snapshot: unknown = event?.codex_telemetry
      if (!isSnapshot(snapshot)) return []
      return [{
        v: snapshot.v, transport: snapshot.transport, response_id: snapshot.response_id,
        stream_id: snapshot.stream_id, connection_reused: snapshot.connection_reused,
        unassociated_events: snapshot.unassociated_events, truncated: snapshot.truncated,
        observations: snapshot.observations.map(item => ({
          source: item.source, association: item.association, engine_ids: item.engine_ids,
          faster_model: item.faster_model, active_limit: item.active_limit,
          primary_used_percent: item.primary_used_percent, primary_window_minutes: item.primary_window_minutes,
          secondary_used_percent: item.secondary_used_percent, secondary_window_minutes: item.secondary_window_minutes
        }))
      }]
    })
  } catch {
    return []
  }
}
