#!/bin/sh
# Import a quiesced Umami PostgreSQL website_event/session source into Observe.
# Required: UMAMI_DB, OBSERVE_ENDPOINT, OBSERVE_API_KEY.
# Optional: OBSERVE_SITE_ID (default), UMAMI_WEBSITE_ID (UUID filter), BATCH
# (1-100, default 100), STATE_FILE (.umami-migrate.state), STOP_AFTER (default 0).
# Requires psql, jq, curl. Keep source/destination configuration fixed on resume.
# Random UUID pagination is for a static historical source, not live tailing.
set -eu
umask 077
fail() { printf '%s\n' "$1" >&2; exit 1; }
: "${UMAMI_DB:?UMAMI_DB is required}"
: "${OBSERVE_ENDPOINT:?OBSERVE_ENDPOINT is required}"
: "${OBSERVE_API_KEY:?OBSERVE_API_KEY is required}"
OBSERVE_SITE_ID=${OBSERVE_SITE_ID:-default}
UMAMI_WEBSITE_ID=${UMAMI_WEBSITE_ID:-}
BATCH=${BATCH:-100}
STATE_FILE=${STATE_FILE:-.umami-migrate.state}
STOP_AFTER=${STOP_AFTER:-0}
# Prefix relative paths so file utilities never interpret them as options.
case "$STATE_FILE" in /*|./*|../*) ;; *) STATE_FILE=./$STATE_FILE ;; esac
for tool in psql jq curl mktemp; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
uuid() { printf '%s' "$1" | jq -Re 'test("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")' >/dev/null; }
[ -z "$UMAMI_WEBSITE_ID" ] || uuid "$UMAMI_WEBSITE_ID" || fail 'Invalid UMAMI_WEBSITE_ID UUID'
printf '%s' "$BATCH" | jq -Re 'test("^[1-9][0-9]{0,2}$") and (tonumber <= 100)' >/dev/null || fail 'BATCH must be 1-100'
printf '%s' "$STOP_AFTER" | jq -Re 'test("^(0|[1-9][0-9]{0,9})$") and (tonumber <= 2147483647)' >/dev/null || fail 'Invalid STOP_AFTER'
# Refuse header/URL injection; credentials stay out of argv and diagnostic output.
printf '%s' "$OBSERVE_API_KEY" | jq -Rse 'length > 0 and (test("[\u0000-\u0020\u007f]") | not)' >/dev/null || fail 'Invalid API key format'
printf '%s' "$OBSERVE_SITE_ID" | jq -Rse 'length > 0 and (test("[\u0000-\u001f\u007f]") | not)' >/dev/null || fail 'Invalid site ID'
OBSERVE_ENDPOINT=${OBSERVE_ENDPOINT%/}
printf '%s' "$OBSERVE_ENDPOINT" | jq -Rse 'test("^https?://[^/@?#[:space:]]+(/[^?#[:space:]]*)?$")' >/dev/null || fail 'Invalid Observe endpoint'
[ ! -L "$STATE_FILE" ] || fail 'Checkpoint must not be a symlink'
[ ! -e "$STATE_FILE" ] || [ -f "$STATE_FILE" ] || fail 'Checkpoint must be a regular file'
# Lock this checkpoint before reading it; concurrent writers cannot regress it.
LOCK=${STATE_FILE}.lock
mkdir "$LOCK" 2>/dev/null || fail 'Checkpoint is locked or its directory is unavailable'
WORK=
CHECKPOINT_TMP=
cleanup() {
    [ -z "$CHECKPOINT_TMP" ] || rm -f "$CHECKPOINT_TMP"
    [ -z "$WORK" ] || rm -rf "$WORK"
    rmdir "$LOCK" 2>/dev/null || :
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
WORK=$(mktemp -d "${TMPDIR:-/tmp}/observe-umami.XXXXXX") || fail 'Cannot create private temporary directory'
printf 'X-API-Key: %s\n' "$OBSERVE_API_KEY" > "$WORK/headers"
printf 'Content-Type: application/json\n' >> "$WORK/headers"
AFTER_ID=00000000-0000-0000-0000-000000000000
if [ -f "$STATE_FILE" ]; then
    # Legacy UUID-only checkpoints remain readable, but are never interpolated
    # before validation. New checkpoints bind the filter and destination.
    if jq -e --arg website "$UMAMI_WEBSITE_ID" --arg site "$OBSERVE_SITE_ID" --arg endpoint "$OBSERVE_ENDPOINT" \
        'type == "object" and .website_id == $website and .site_id == $site and .endpoint == $endpoint and (.event_id | type == "string")' \
        "$STATE_FILE" >/dev/null 2>&1; then
        AFTER_ID=$(jq -r .event_id "$STATE_FILE")
    else
        AFTER_ID=$(cat "$STATE_FILE")
    fi
    uuid "$AFTER_ID" || fail 'Invalid checkpoint or configuration differs from checkpoint'
fi
TOTAL=0
while :; do
    LIMIT=$BATCH
    if [ "$STOP_AFTER" -gt 0 ]; then
        REMAINING=$((STOP_AFTER - TOTAL))
        [ "$REMAINING" -ge "$LIMIT" ] || LIMIT=$REMAINING
    fi
    # Use psql literal variables, not shell SQL interpolation. JSON framing
    # handles empty/null values, tabs, newlines, quotes and Unicode losslessly.
    # Session enrichment lives in Umami's session table. to_jsonb lookups allow
    # optional columns across Umami versions without SELECTing absent columns.
    if ! PGDATABASE="$UMAMI_DB" psql -X -q -A -t -v ON_ERROR_STOP=1 \
        -v after_id="$AFTER_ID" -v website_id="$UMAMI_WEBSITE_ID" -v batch="$LIMIT" \
        > "$WORK/rows" 2> "$WORK/psql-error" <<'SQL'
SELECT jsonb_build_object(
    'event_id', e.event_id::text,
    'event_type', COALESCE(NULLIF(e.event_name, ''), 'pageview'),
    'session_id', e.session_id::text,
    'timestamp', FLOOR(EXTRACT(EPOCH FROM e.created_at) * 1000)::bigint,
    'pathname', COALESCE(e.url_path, ''),
    'url', COALESCE(e.url_path, '') || CASE WHEN COALESCE(e.url_query, '') = '' THEN '' ELSE '?' || e.url_query END,
    'referrer', CASE WHEN COALESCE(e.referrer_domain, '') = '' THEN '' ELSE 'https://' || e.referrer_domain || COALESCE(e.referrer_path, '') END,
    'utm_source', COALESCE(to_jsonb(e)->>'utm_source', ''),
    'utm_medium', COALESCE(to_jsonb(e)->>'utm_medium', ''),
    'utm_campaign', COALESCE(to_jsonb(e)->>'utm_campaign', ''),
    'utm_term', COALESCE(to_jsonb(e)->>'utm_term', ''),
    'utm_content', COALESCE(to_jsonb(e)->>'utm_content', ''),
    'browser', COALESCE(to_jsonb(s)->>'browser', ''),
    'os', COALESCE(to_jsonb(s)->>'os', ''),
    'device', COALESCE(to_jsonb(s)->>'device', ''),
    'country', COALESCE(to_jsonb(s)->>'country', '')
)
FROM website_event e
LEFT JOIN session s ON s.session_id = e.session_id AND s.website_id = e.website_id
WHERE e.event_id > :'after_id'::uuid
  AND (NULLIF(:'website_id', '')::uuid IS NULL OR e.website_id = NULLIF(:'website_id', '')::uuid)
ORDER BY e.event_id ASC
LIMIT :batch;
SQL
    then
        fail 'Source query failed; checkpoint unchanged'
    fi
    if ! jq -se --arg site "$OBSERVE_SITE_ID" --arg after "$AFTER_ID" --argjson limit "$LIMIT" '
        def uuid: type == "string" and test("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$");
        length <= $limit and all(.[]; type == "object" and (.event_id | uuid) and (.session_id | uuid)
          and (.timestamp | type == "number" and . > 0 and . == floor)
          and (.pathname | type == "string" and (length == 0 or startswith("/"))))
        and ([.[].event_id | ascii_downcase] == ([.[].event_id | ascii_downcase] | sort | unique))
        and all(.[]; (.event_id | ascii_downcase) > ($after | ascii_downcase))
        ' "$WORK/rows" >/dev/null 2>&1; then
        fail 'Invalid source batch; checkpoint unchanged'
    fi
    jq -s --arg site "$OBSERVE_SITE_ID" '{events: map(. + {site_id: $site})}' "$WORK/rows" > "$WORK/payload"
    COUNT=$(jq '.events | length' "$WORK/payload")
    [ "$COUNT" -gt 0 ] || { printf 'Done. Total migrated: %s\n' "$TOTAL"; exit 0; }
    # No redirects: do not forward a site key to an unconfigured destination.
    if ! HTTP=$(curl --disable --silent --show-error --connect-timeout 15 --max-time 120 \
        --output "$WORK/response" --write-out '%{http_code}' \
        --request POST "$OBSERVE_ENDPOINT/api/v1/events/import" \
        --header "@$WORK/headers" --data-binary "@$WORK/payload" 2> "$WORK/curl-error"); then
        fail 'Import transport failed; checkpoint unchanged (safe to retry stable IDs)'
    fi
    case "$HTTP" in 2[0-9][0-9]) ;; *) fail 'Import HTTP refusal; checkpoint unchanged' ;; esac
    # rejected is omitted by HistoricalHandler on zero; explicit null is invalid.
    if ! jq -se --argjson count "$COUNT" 'length == 1 and (.[0] | type == "object" and .ok == true and .accepted == $count and
        (if has("rejected") then .rejected == 0 else true end))' "$WORK/response" >/dev/null 2>&1; then
        fail 'Import admission not confirmed; checkpoint unchanged'
    fi
    LAST_ID=$(jq -r '.events[-1].event_id' "$WORK/payload")
    CHECKPOINT_TMP=$(mktemp "${STATE_FILE}.tmp.XXXXXX") || fail 'Cannot create checkpoint temporary file'
    jq -n --arg id "$LAST_ID" --arg website "$UMAMI_WEBSITE_ID" --arg site "$OBSERVE_SITE_ID" --arg endpoint "$OBSERVE_ENDPOINT" \
        '{event_id: $id, website_id: $website, site_id: $site, endpoint: $endpoint}' > "$CHECKPOINT_TMP"
    mv -f "$CHECKPOINT_TMP" "$STATE_FILE" || fail 'Cannot save checkpoint'
    CHECKPOINT_TMP=
    AFTER_ID=$LAST_ID
    TOTAL=$((TOTAL + COUNT))
    printf 'Migrated %s (total: %s)\n' "$COUNT" "$TOTAL"
    if [ "$STOP_AFTER" -gt 0 ] && [ "$TOTAL" -ge "$STOP_AFTER" ]; then
        printf 'Reached STOP_AFTER. Checkpoint saved.\n'
        exit 0
    fi
done
