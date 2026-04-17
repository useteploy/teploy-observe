#!/bin/sh
# Migrate historical events from Umami (Postgres) into Observe.
#
# Usage (all env required):
#   UMAMI_DB="postgres://user:pass@host:5432/umami" \
#   OBSERVE_ENDPOINT="https://observe.example.com" \
#   OBSERVE_API_KEY="obs_xxx" \
#   OBSERVE_SITE_ID="default" \
#   ./scripts/migrate-umami.sh
#
# Optional:
#   UMAMI_WEBSITE_ID   filter source to a specific Umami website id
#   BATCH              batch size (default 100)
#   STATE_FILE         checkpoint file (default .umami-migrate.state)
#   STOP_AFTER         stop after N events (useful for a test run)

set -eu

: "${UMAMI_DB:?UMAMI_DB is required}"
: "${OBSERVE_ENDPOINT:?OBSERVE_ENDPOINT is required}"
: "${OBSERVE_API_KEY:?OBSERVE_API_KEY is required}"
: "${OBSERVE_SITE_ID:=default}"
BATCH="${BATCH:-100}"
STATE_FILE="${STATE_FILE:-.umami-migrate.state}"
STOP_AFTER="${STOP_AFTER:-0}"

command -v psql >/dev/null 2>&1 || { echo "psql is required" >&2; exit 1; }
command -v jq   >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }

if [ -f "$STATE_FILE" ]; then
  AFTER_ID=$(cat "$STATE_FILE")
  echo "Resuming from event_id > $AFTER_ID"
else
  AFTER_ID="00000000-0000-0000-0000-000000000000"
  echo "Starting from the beginning"
fi

TOTAL=0
FILTER=""
[ -n "${UMAMI_WEBSITE_ID:-}" ] && FILTER="AND website_id = '$UMAMI_WEBSITE_ID'"

while :; do
  ROWS=$(psql "$UMAMI_DB" -At -F $'\t' -c "
    SELECT
      event_id,
      COALESCE(event_name, 'pageview') AS event_type,
      session_id,
      EXTRACT(EPOCH FROM created_at) * 1000 AS ts_ms,
      url_path,
      url_query,
      referrer_domain,
      referrer_path,
      utm_source, utm_medium, utm_campaign, utm_term, utm_content,
      browser, os, device, country
    FROM website_event
    WHERE event_id > '$AFTER_ID' $FILTER
    ORDER BY event_id ASC
    LIMIT $BATCH;
  ")

  [ -z "$ROWS" ] && { echo "Done. Total migrated: $TOTAL"; exit 0; }

  # Build a JSON array of events.
  PAYLOAD=$(
    printf '%s\n' "$ROWS" | while IFS=$'\t' read -r id etype sid ts path qs ref_dom ref_path utm_s utm_m utm_c utm_t utm_cn browser os device country; do
      jq -cn \
        --arg site_id   "$OBSERVE_SITE_ID" \
        --arg event_type "$etype" \
        --arg session   "$sid" \
        --arg ts        "$ts" \
        --arg pathname  "$path" \
        --arg qs        "$qs" \
        --arg referrer  "$ref_dom$ref_path" \
        --arg utm_source "$utm_s" \
        --arg utm_medium "$utm_m" \
        --arg utm_campaign "$utm_c" \
        --arg utm_term  "$utm_t" \
        --arg utm_content "$utm_cn" \
        --arg browser   "$browser" \
        --arg os        "$os" \
        --arg device    "$device" \
        --arg country   "$country" \
      '{
        site_id: $site_id,
        event_type: $event_type,
        session_id: $session,
        timestamp: ($ts | tonumber | floor),
        pathname: $pathname,
        referrer: (if $referrer == "" then null else $referrer end),
        utm_source: (if $utm_source == "" then null else $utm_source end),
        utm_medium: (if $utm_medium == "" then null else $utm_medium end),
        utm_campaign: (if $utm_campaign == "" then null else $utm_campaign end),
        utm_term: (if $utm_term == "" then null else $utm_term end),
        utm_content: (if $utm_content == "" then null else $utm_content end),
        browser: (if $browser == "" then null else $browser end),
        os: (if $os == "" then null else $os end),
        device: (if $device == "" then null else $device end),
        country: (if $country == "" then null else $country end)
      } | with_entries(select(.value != null))'
    done | jq -sc '{ events: . }'
  )

  HTTP=$(curl -s -o /tmp/umami-migrate-resp.json -w "%{http_code}" \
    -X POST "$OBSERVE_ENDPOINT/api/v1/events/batch" \
    -H "X-API-Key: $OBSERVE_API_KEY" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD")

  if [ "$HTTP" -ge 400 ]; then
    echo "Batch failed (HTTP $HTTP):" >&2
    cat /tmp/umami-migrate-resp.json >&2
    echo "Last successful event_id: $AFTER_ID (saved to $STATE_FILE)"
    exit 1
  fi

  # Advance checkpoint to the last id in this batch.
  LAST_ID=$(printf '%s\n' "$ROWS" | awk -F'\t' 'END { print $1 }')
  AFTER_ID="$LAST_ID"
  echo "$AFTER_ID" > "$STATE_FILE"

  COUNT=$(printf '%s\n' "$ROWS" | wc -l | tr -d ' ')
  TOTAL=$((TOTAL + COUNT))
  echo "Migrated $COUNT (total: $TOTAL, last: $AFTER_ID)"

  if [ "$STOP_AFTER" -gt 0 ] && [ "$TOTAL" -ge "$STOP_AFTER" ]; then
    echo "Reached STOP_AFTER=$STOP_AFTER. Checkpoint saved."
    exit 0
  fi
done
