# Codex Upstream Metadata

Open **Ops → Settings → Advanced settings** and enable **Capture passive Codex metadata**. The switch is off by default and applies to new requests and new WebSocket turns. An in-progress turn keeps its starting setting. Capture reads metadata already returned by the upstream; it sends no probes or extra model requests.

In **Usage**, filter by account, model or time as usual, then open **Codex upstream metadata** on a record. **Download JSON** exports the retained observation. The usage spreadsheet includes a metadata-availability column. Failed requests use the existing Ops error detail page and require Ops monitoring to be enabled. Raw telemetry is visible only to administrators.

## Reading An Observation

| Field | Meaning |
| --- | --- |
| `engine_ids` | Opaque identifiers reported in `responsesapi.websocket_timing`. They have no built-in mapping to public model names. |
| `faster_model` | The upstream's buffering/retry hint. It does not identify the engine that executed the response. |
| `active_limit` | The upstream's limit identifier. |
| `primary_*`, `secondary_*` | Reported quota percentage and window duration, kept in the upstream's primary/secondary order. A value of 100 describes quota usage, not a routing decision. |
| `source` | The event or header source from which the fields were read. |
| `association` | How the observation is associated with the record, as described below. |

Requested, sent-upstream and returned public model names remain in their existing usage fields. The metadata above is separate from those names.

`response_id` association means an event explicitly matched the established response ID. `active_response` means an ID-less event was associated with the unique active WebSocket response; `http_response` uses the current HTTP response. These contextual associations remain uncertain on reused connections because a late event may belong to an earlier turn. `upstream_attempt` identifies error headers received for a pending request before a response ID was established.

`connection` observations come from a fresh WebSocket handshake. They are recorded once, not copied into later turns. Reused and prewarmed pool handshakes are omitted. Unestablished, conflicting, idle and post-terminal timing events are omitted; capture does not wait for events after the existing completion boundary. Missing fields remain unknown.

## Size And Retention

Each usage record contains at most **4 KiB of compact telemetry JSON**. All telemetry within one Ops error row shares the same 4 KiB budget. PostgreSQL JSONB and row overhead can differ from the compact JSON size.

A snapshot retains up to eight distinct observations and eight unique engine IDs per observation. IDs are limited to 128 visible ASCII bytes. Metadata events larger than 16 KiB are omitted; at most 32 metadata events are parsed, with a separate budget reserved for timing events. `truncated` marks a parser, count or size limit; newer retained observations take precedence when space runs out. Invalid values, percentages outside 0–100 and window durations outside 1–527040 minutes are omitted.

Telemetry is deleted with its parent record. Existing usage retention uses `dashboard_aggregation.retention.usage_logs_days` when that cleanup is enabled (90 days by default); manual usage cleanup also removes it. Ops uses its own cleanup switch, schedule and error-log retention (30 days by default). Enable the existing cleanup controls for the history you want to retain. Turning capture off leaves previously saved records intact.

A billable partial failure can already have both a usage record and an Ops error record. When both contain telemetry for the same account and response ID, use the usage record for that turn and Ops for the failed-attempt context. Their retention periods are independent.
