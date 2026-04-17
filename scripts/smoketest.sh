#!/bin/bash
# Smoke-test every API the UI uses. Any 5xx = regression.
set -u
BASE="http://localhost:3000"
SITE="default"
TOKEN=$(curl -s -X POST $BASE/api/v1/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"observe"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
H="Authorization: Bearer $TOKEN"

# Per-route API calls (what the UI triggers on load)
declare -a CALLS=(
  "root|/"
  "dashboard|/api/v1/stats/overview?site_id=$SITE"
  "dashboard|/api/v1/stats/timeseries?site_id=$SITE"
  "dashboard|/api/v1/stats/pages?site_id=$SITE"
  "dashboard|/api/v1/stats/referrers?site_id=$SITE"
  "dashboard|/api/v1/stats/browsers?site_id=$SITE"
  "dashboard|/api/v1/stats/countries?site_id=$SITE"
  "dashboard|/api/v1/stats/os?site_id=$SITE"
  "dashboard|/api/v1/stats/devices?site_id=$SITE"
  "dashboard|/api/v1/stats/channels?site_id=$SITE"
  "dashboard|/api/v1/stats/languages?site_id=$SITE"
  "dashboard|/api/v1/stats/screens?site_id=$SITE"
  "dashboard|/api/v1/stats/utm?site_id=$SITE"
  "dashboard|/api/v1/stats/events?site_id=$SITE"
  "dashboard|/api/v1/stats/entry-pages?site_id=$SITE"
  "dashboard|/api/v1/stats/exit-pages?site_id=$SITE"
  "errors|/api/v1/issues?site_id=$SITE"
  "errors|/api/v1/releases?site_id=$SITE"
  "sessions|/api/v1/replays?site_id=$SITE"
  "sessions|/api/v1/stats/sessions?site_id=$SITE"
  "logs|/api/v1/logs/search?site_id=$SITE"
  "logs|/api/v1/logs/stats?site_id=$SITE"
  "traces|/api/v1/traces/services?site_id=$SITE"
  "traces|/api/v1/traces/search?site_id=$SITE"
  "traces|/api/v1/traces/dependencies?site_id=$SITE"
  "alerts|/api/v1/platform/alerts/rules?site_id=$SITE"
  "alerts|/api/v1/platform/alerts/history?site_id=$SITE"
  "flags|/api/v1/flags?site_id=$SITE"
  "experiments|/api/v1/experiments?site_id=$SITE"
  "surveys|/api/v1/surveys?site_id=$SITE"
  "llm|/api/v1/llm/stats?site_id=$SITE"
  "llm|/api/v1/llm/models?site_id=$SITE"
  "llm|/api/v1/llm/traces?site_id=$SITE"
  "monitoring|/api/v1/infra/hosts?site_id=$SITE"
  "monitoring|/api/v1/monitors?site_id=$SITE"
  "monitoring|/api/v1/crons?site_id=$SITE"
  "dashboards|/api/v1/dashboards?site_id=$SITE"
  "integrations|/api/v1/integrations?site_id=$SITE"
  "reports|/api/v1/reports?site_id=$SITE"
  "insights|/api/v1/stats/journeys?site_id=$SITE"
  "insights|/api/v1/stats/retention?site_id=$SITE"
  "insights|/api/v1/stats/correlations?site_id=$SITE"
  "insights|/api/v1/goals?site_id=$SITE"
  "events|/api/v1/stats/events?site_id=$SITE"
  "campaigns|/api/v1/stats/utm?site_id=$SITE"
  "settings|/api/v1/sites"
  "settings|/api/v1/sites/$SITE/keys"
  "settings|/api/v1/sites/$SITE/share"
  "settings|/api/v1/platform/users"
  "settings|/api/v1/platform/webhooks?site_id=$SITE"
  "explorer|/api/v1/views?site_id=$SITE"
)

PASS=0; FAIL=0
for entry in "${CALLS[@]}"; do
  route="${entry%%|*}"
  path="${entry#*|}"
  if [[ "$path" == "/" ]]; then
    code=$(curl -s -o /dev/null -w "%{http_code}" $BASE$path)
  else
    code=$(curl -s -o /dev/null -w "%{http_code}" -H "$H" $BASE$path)
  fi
  if [[ "$code" -ge 500 ]]; then
    echo "FAIL [$code] $route: $path"
    FAIL=$((FAIL+1))
  else
    PASS=$((PASS+1))
  fi
done
echo
echo "Pass: $PASS  Fail: $FAIL"
exit $FAIL
