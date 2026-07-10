-- Trace <-> error linking (audit #347).
-- Adds `trace_id`/`span_id` to `error_events` so SDK clients that capture an
-- error inside an active trace can attach the exact trace context. The trace
-- detail view then correlates errors by exact trace_id instead of timestamp
-- overlap; rows without a trace_id (browser errors, older SDKs) keep the
-- timestamp-window fallback.
ALTER TABLE error_events ADD COLUMN IF NOT EXISTS trace_id TEXT NOT NULL DEFAULT '';
ALTER TABLE error_events ADD COLUMN IF NOT EXISTS span_id TEXT NOT NULL DEFAULT '';
