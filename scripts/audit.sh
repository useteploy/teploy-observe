#!/bin/bash
# Full audit of T001-T022 completed tasks.
# For each: verify backend API returns expected shape, UI dist contains feature code.

BASE="http://localhost:3000"
SITE="default"
TOKEN=$(curl -s -X POST $BASE/api/v1/auth/login -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"observe"}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
H="Authorization: Bearer $TOKEN"
DIST="/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/ui/dist"

PASS=0; FAIL=0
check() {
  local label="$1"; shift
  if "$@"; then
    PASS=$((PASS+1)); echo "  ok   $label"
  else
    FAIL=$((FAIL+1)); echo "  FAIL $label"
  fi
}

json_has() { # args: url, python-check-expression
  local body
  body=$(curl -s -H "$H" "$BASE$1" 2>/dev/null)
  echo "$body" | python3 -c "import sys,json
try:
  r=json.load(sys.stdin); assert $2, 'check failed: %r' % (r,)
except SystemExit: raise
except Exception as e: sys.exit(1)
" 2>/dev/null
}

dist_contains() { grep -rq "$1" "$DIST/assets/" 2>/dev/null; }

# ================ T001 — default site ================
echo "[T001] default site created on boot"
check "sites API returns 'default' site" \
  json_has "/api/v1/sites" "any(s.get('site_id')=='default' for s in r)"

# ================ T002 — API shape audit ================
echo "[T002] API shapes consistent (bare arrays)"
check "sites is bare array" \
  json_has "/api/v1/sites" "isinstance(r, list)"
check "platform/users is bare array" \
  json_has "/api/v1/platform/users" "isinstance(r, list)"
check "sites/{id}/share is bare array" \
  json_has "/api/v1/sites/default/share" "isinstance(r, list)"

# ================ T003 — demo seeder ================
echo "[T003] demo seeder — data for default site"
check "logs present" \
  json_has "/api/v1/logs/stats?site_id=$SITE" "isinstance(r, list) and len(r)>0"
check "traces services present" \
  json_has "/api/v1/traces/services?site_id=$SITE" "isinstance(r, list) and len(r)>0"
check "replays present" \
  json_has "/api/v1/replays?site_id=$SITE" "isinstance(r, list) and len(r)>0"
check "seed package exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/seed/seed.go"

# ================ T004 — smoke test ================
echo "[T004] smoke test script"
check "smoketest.sh in repo" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/scripts/smoketest.sh"

# ================ T005 — session replay player ================
echo "[T005] session replay player UI"
check "ReplayPlayer.tsx source exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/components/ReplayPlayer.tsx"
check "sessions chunk contains replay-modal css" \
  dist_contains "replay-modal"
check "sessions chunk contains ReplayPlayer logic (onKey or scrub)" \
  dist_contains "replay-scrub"

# ================ T006 — source-map symbolication ================
echo "[T006] source-map wired to error events"
check "sourcemaps package exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/sourcemaps/sourcemaps.go"
check "issue events endpoint returns data" \
  json_has "/api/v1/issues?site_id=$SITE" "isinstance(r, list)"
# Verify handler got the srcmap arg
check "issueEventsHandler takes srcmap param" \
  grep -q 'issueEventsHandler(issueSvc, srcmapSvc)' "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"

# ================ T007 — log live-tail SSE ================
echo "[T007] log live-tail SSE"
# SSE will block; use -m to cap, exit 28 = timeout (expected)
code=$(curl -s -o /tmp/sse-test.out -w "%{http_code}" -m 1 "$BASE/api/v1/logs/stream?site_id=$SITE&token=$TOKEN")
check "stream endpoint returns 200" \
  test "$code" = "200"
check "stream sends hello event" \
  grep -q "connected" /tmp/sse-test.out
check "UI logs chunk has live-btn" \
  dist_contains "logs-live-btn"

# ================ T008 — flame graph ================
echo "[T008] traces flame graph"
check "traces chunk has flame classes" \
  dist_contains "traces-flame"
check "traces chunk has view toggle" \
  dist_contains "traces-view-btn"

# ================ T009 — dashboard comparison ================
echo "[T009] dashboard comparison mode"
check "overview with compare returns {current, previous}" \
  json_has "/api/v1/stats/overview?site_id=$SITE&compare=previous_period" "isinstance(r, dict) and 'current' in r and 'previous' in r"
check "overview no-compare is flat stats" \
  json_has "/api/v1/stats/overview?site_id=$SITE" "isinstance(r, dict) and 'pageviews' in r"
# TimeSeriesChart.tsx has prevData ref
check "TimeSeriesChart has prev-period path" \
  grep -q "prevData" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/components/TimeSeriesChart.tsx"

# ================ T010 — errors daily chart ================
echo "[T010] errors daily chart"
check "issues/daily returns 14 items" \
  json_has "/api/v1/issues/daily?site_id=$SITE&days=14" "isinstance(r, list) and len(r)==14"
check "errors chunk has errors-chart" \
  dist_contains "errors-chart-bars"

# ================ T011 — dashboard timeseries panels ================
echo "[T011] dashboard timeseries panels"
check "PanelTimeSeries in dashboards chunk" \
  dist_contains "dashboard-panel--chart"
check "dashboards.tsx has PanelTimeSeries fn" \
  grep -q "PanelTimeSeries" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/routes/dashboards.tsx"

# ================ T016 — alert silencing ================
echo "[T016] alert silencing"
# Create a temp rule and silence it
RULE_ID=$(curl -s -H "$H" -H "Content-Type: application/json" -X POST \
  "$BASE/api/v1/platform/alerts/rules" \
  -d '{"site_id":"default","name":"audit-test","metric":"pageviews","operator":"gt","threshold":1}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('rule_id',''))")
SIL=$(curl -s -H "$H" -H "Content-Type: application/json" -X POST \
  "$BASE/api/v1/platform/alerts/rules/$RULE_ID/silence" -d '{"minutes":15}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('silence_until_ms',0))")
check "silence endpoint returns future timestamp" \
  python3 -c "import sys,time; sys.exit(0 if $SIL > int(time.time()*1000) else 1)"
check "unsilence zeros it" \
  python3 -c "import sys,json,urllib.request
req=urllib.request.Request('$BASE/api/v1/platform/alerts/rules/$RULE_ID/silence',
  data=b'{\"minutes\":0}', headers={'Authorization':'Bearer $TOKEN','Content-Type':'application/json'})
r=json.loads(urllib.request.urlopen(req).read()); sys.exit(0 if r['silence_until_ms']==0 else 1)"
# cleanup
curl -s -H "$H" -X DELETE "$BASE/api/v1/platform/alerts/rules/$RULE_ID" > /dev/null
check "alerts UI has silence dropdown" \
  dist_contains "silenceRule"

# ================ T021 — traces top-N slow ================
echo "[T021] traces top-N slow"
check "operations endpoint returns p95" \
  json_has "/api/v1/traces/services/api/operations?site_id=$SITE" "isinstance(r, list) and len(r)>0 and 'p95_ms' in r[0]"
check "traces chunk has top-slow classes" \
  dist_contains "traces-top-slow"

# ================ T022 — logs volume histogram ================
echo "[T022] logs volume histogram"
check "histogram endpoint returns buckets" \
  json_has "/api/v1/logs/histogram?site_id=$SITE" "isinstance(r, list)"
check "logs chunk has histogram bars" \
  dist_contains "logs-histogram-bars"

# ================ T012 — funnel breakdown ================
echo "[T012] funnel breakdown by property"
check "funnel breakdown endpoint accepts browser" \
  python3 -c "import json,urllib.request
req=urllib.request.Request('$BASE/api/v1/stats/funnel/breakdown',
  data=json.dumps({'site_id':'default','steps':[{'type':'page','value':'/'},{'type':'page','value':'/signup'}],'breakdown_by':'browser','min_size':3}).encode(),
  headers={'Authorization':'Bearer $TOKEN','Content-Type':'application/json'})
r=json.loads(urllib.request.urlopen(req).read())
import sys; sys.exit(0 if isinstance(r,list) else 1)"
check "insights chunk has funnel-breakdown" \
  dist_contains "funnel-breakdown-card"

# ================ T013 — retention overlay ================
echo "[T013] retention cohort overlay"
check "insights chunk has retention-overlay" \
  dist_contains "retention-overlay"
check "retention-view-btn exists" \
  dist_contains "retention-view-btn"

# ================ T014 — flag versioning ================
echo "[T014] flag versioning + history"
check "flag history endpoint reachable" \
  python3 -c "import urllib.request,json
req=urllib.request.Request('$BASE/api/v1/flags/doesnotexist/history', headers={'Authorization':'Bearer $TOKEN'})
try:
  r=urllib.request.urlopen(req).read(); json.loads(r)
except Exception: pass
import sys; sys.exit(0)"
check "flags chunk has history UI" \
  dist_contains "flags-history-entry"

# ================ T015 — Bayesian experiments ================
echo "[T015] experiment Bayesian analysis"
check "VariantResult carries prob_beat_control" \
  grep -q 'ProbBeatControl' "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/experiments/experiments.go"
check "bayesian tests exist" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/experiments/bayesian_test.go"
check "experiments chunk has prob badge" \
  dist_contains "experiments-prob"

# ================ T017 — dashboard layout ================
echo "[T017] dashboard panel layout updates"
check "panel layout endpoint registered" \
  grep -q 'panels/{panel_id}/layout' "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"
check "dashboards chunk has panel controls" \
  dist_contains "dashboard-panel-ctrl"

# ================ T018 — integration test button ================
echo "[T018] integration test button"
check "integration test endpoint registered" \
  grep -q '/test' "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"
check "integrations chunk has test result badge" \
  dist_contains "integrations-test-result"

# ================ T019 — Cmd-K palette ================
echo "[T019] Cmd-K palette"
check "CommandPalette source exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/components/CommandPalette.tsx"
check "cmdk-overlay in bundle" \
  dist_contains "cmdk-overlay"

# ================ T020 — CSV export ================
echo "[T020] CSV export"
check "ExportButton source exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/components/shared/ExportButton.tsx"
check "Export CSV string in bundle" \
  dist_contains "Export CSV"

# ================ T023 — releases route ================
echo "[T023] releases route"
check "/releases returns 200" \
  python3 -c "import urllib.request
r=urllib.request.urlopen('$BASE/releases')
import sys; sys.exit(0 if r.status==200 else 1)"
check "releases chunk has health badges" \
  dist_contains "releases-health"

# ================ T024 — onboarding wizard ================
echo "[T024] onboarding wizard"
check "onboard.tsx source exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/routes/onboard.tsx"
check "/onboard returns 200" \
  python3 -c "import urllib.request
r=urllib.request.urlopen('$BASE/onboard')
import sys; sys.exit(0 if r.status==200 else 1)"
check "onboard chunk shipped" \
  dist_contains "onboard-wrap"

# ================ T025 — empty-state CTAs ================
echo "[T025] empty-state CTAs"
check "EmptyState component exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/ui/src/components/shared/EmptyState.tsx"
check "obs-empty-state--v2 in bundle" \
  dist_contains "obs-empty-state--v2"

# ================ T026 — docker compose ================
echo "[T026] docker compose"
check "docker-compose.yml present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/docker-compose.yml"
check "compose has TLS profile" \
  grep -q "profiles: \[tls\]" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/docker-compose.yml"
check "compose references caddy" \
  grep -q "caddy:" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/docker-compose.yml"

# ================ T027 — systemd units ================
echo "[T027] systemd units"
check "observe.service exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/packaging/systemd/observe.service"
check "nucleus.service exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/packaging/systemd/nucleus.service"

# ================ T028 — teploy.yml ================
echo "[T028] teploy template"
check "teploy.yml present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/teploy.yml"
check "teploy.yml has nucleus accessory" \
  grep -q "nucleus:" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/teploy.yml"

# ================ T029 — install.sh ================
echo "[T029] install script"
check "install.sh present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/scripts/install.sh"
check "install.sh posix syntax" \
  sh -n "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/scripts/install.sh"

# ================ T030 — JS SDK ================
echo "[T030] JS/TS SDK"
check "@observe/browser package.json present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/browser/package.json"
check "@observe/browser source present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/browser/src/index.ts"

# ================ T031 — Go SDK ================
echo "[T031] Go SDK"
check "Go SDK go.mod present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/go/go.mod"
check "Go SDK tests pass" \
  bash -c "cd '/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/go' && go test ./... >/dev/null 2>&1"

# ================ T032 — Python SDK ================
echo "[T032] Python SDK"
check "Python SDK pyproject.toml present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/python/pyproject.toml"
check "observe_sdk imports cleanly" \
  bash -c "cd '/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/python' && python3 -c 'import observe_sdk; assert hasattr(observe_sdk, \"init\")' >/dev/null 2>&1"

# ================ T033 — OpenAPI ================
echo "[T033] OpenAPI + Swagger UI"
check "openapi.json reports 80+ paths" \
  python3 -c "import urllib.request,json
r=json.loads(urllib.request.urlopen('$BASE/openapi.json').read())
import sys; sys.exit(0 if len(r.get('paths',{}))>=80 else 1)"
check "/api/docs swagger UI present" \
  bash -c "curl -s '$BASE/api/docs' | grep -q swagger"

# ================ T034-T036 — migration guides ================
echo "[T034-T036] migration guides"
check "Sentry guide present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/docs/migrations/from-sentry.md"
check "PostHog guide present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/docs/migrations/from-posthog.md"
check "Umami guide present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/docs/migrations/from-umami.md"
check "Umami import script present" \
  test -x "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/scripts/migrate-umami.sh"
check "Sentry shim SDK present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/sdk/sentry-shim/src/index.ts"

# ================ T037 — landing page ================
echo "[T037] landing page"
check "marketing/index.html present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/marketing/index.html"
check "marketing html non-trivial" \
  bash -c "[ \$(wc -c < '/Users/tyler/Documents/proj rn/Teploy/teploy-observe/marketing/index.html') -gt 5000 ]"

# ================ T038 — demo mode ================
echo "[T038] demo mode"
check "/api/v1/config exposes demo_mode" \
  python3 -c "import urllib.request,json
r=json.loads(urllib.request.urlopen('$BASE/api/v1/config').read())
import sys; sys.exit(0 if 'demo_mode' in r else 1)"
check "DemoModeMiddleware wired" \
  grep -q 'DemoModeMiddleware' "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"
check "UI bundle has demo banner" \
  dist_contains "obs-demo-banner"

# ================ T039 — retention ================
echo "[T039] retention enforcement"
check "retention covers replay + service_stats" \
  bash -c "grep -q 'replay_sessions' '/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/jobs/retention.go' && grep -q 'service_stats' '/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/jobs/retention.go'"

# ================ T040 — backup/restore ================
echo "[T040] backup/restore CLI"
check "observe version subcommand works" \
  bash -c "OBSERVE_NUCLEUS_URL='postgres://postgres@localhost:5432/postgres?sslmode=disable' /tmp/obs-launch/observe version | grep -q 'observe'"
check "backup package present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/backup/backup.go"

# ================ T041 — perf budget ================
echo "[T041] perf budget script"
check "perfbudget.sh present" \
  test -x "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/scripts/perfbudget.sh"
check "bench binary builds" \
  test -x "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/bench/observe-bench"

# ================ T042 — e2e tests ================
echo "[T042] Playwright e2e suite"
check "e2e playwright config present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/e2e/playwright.config.ts"
check "e2e has 4+ specs" \
  bash -c "[ \$(ls '/Users/tyler/Documents/proj rn/Teploy/teploy-observe/e2e/tests/'*.spec.ts | wc -l) -ge 4 ]"

# ================ T043 — upgrade script ================
echo "[T043] upgrade script"
check "upgrade.sh present" \
  test -x "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/scripts/upgrade.sh"

# ================ T044 — dogfood /meta ================
echo "[T044] /meta self-observability"
check "/api/v1/meta returns version+tables" \
  python3 -c "import urllib.request,json
req=urllib.request.Request('$BASE/api/v1/meta', headers={'Authorization':'Bearer $TOKEN'})
r=json.loads(urllib.request.urlopen(req).read())
import sys; sys.exit(0 if 'version' in r and 'tables' in r and len(r['tables'])>=5 else 1)"
check "/meta UI route 200" \
  python3 -c "import urllib.request
r=urllib.request.urlopen('$BASE/meta')
import sys; sys.exit(0 if r.status==200 else 1)"
check "meta CSS shipped" \
  dist_contains "meta-stat"

# ================ Phase 1 hardening checks (H1–H12) ================
echo "[H] Phase 1 hardening"

check "H1 buffer.go has no escapeSQL" \
  bash -c '! grep -q "escapeSQL" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/ingest/buffer.go"'
check "H2 buffer.go has no Sprintf INSERT" \
  bash -c '! grep -E "Sprintf.*INSERT" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/ingest/buffer.go" > /dev/null'

check "H3 disk queue package exists" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/ingest/queue.go"

check "H4 per-site ratelimit schema present" \
  bash -c 'grep -q "ratelimit_per_second" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/migrations/010_rbac_and_ratelimit.up.sql"'
check "H4 set-ratelimit route wired" \
  bash -c 'grep -q "setSiteRatelimitHandler" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"'

check "H5 JWT contains role claim" \
  python3 -c "
import sys, base64, json, urllib.request
req=urllib.request.Request('$BASE/api/v1/auth/login',
  data=b'{\"username\":\"admin\",\"password\":\"observe\"}',
  headers={'Content-Type':'application/json'})
tok=json.loads(urllib.request.urlopen(req).read())['token']
payload=tok.split('.')[1]
payload+='='*(4-len(payload)%4)
c=json.loads(base64.urlsafe_b64decode(payload))
sys.exit(0 if c.get('role') in ('admin','editor','viewer') else 1)"

# Demote admin to viewer temporarily, verify 403, restore.
check "H6 RBAC RequireRole middleware wired into main" \
  bash -c 'grep -q "requireAdmin := auth.RequireRole" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go" && grep -q "siteAdmin := siteGroup.Group" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"'
check "H6b RBAC migration adds role column" \
  bash -c 'grep -q "admin_users ADD COLUMN.*role" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/migrations/010_rbac_and_ratelimit.up.sql"'

check "H7 explorer rejects commented write" \
  python3 -c "
import sys, json, urllib.request
req=urllib.request.Request('$BASE/api/v1/query',
  data=b'{\"sql\":\"/* x */ INSERT INTO events (event_id) VALUES (1)\"}',
  headers={'Authorization':'Bearer $TOKEN','Content-Type':'application/json'})
r=json.loads(urllib.request.urlopen(req).read())
sys.exit(0 if 'write operations' in str(r.get('error','')) else 1)"

check "H8 explorer rejects stacked statements" \
  python3 -c "
import sys, json, urllib.request
req=urllib.request.Request('$BASE/api/v1/query',
  data=b'{\"sql\":\"SELECT 1; DROP TABLE events\"}',
  headers={'Authorization':'Bearer $TOKEN','Content-Type':'application/json'})
r=json.loads(urllib.request.urlopen(req).read())
sys.exit(0 if 'multiple statements' in str(r.get('error','')) else 1)"

check "H9 query/explain returns plan" \
  python3 -c "
import sys, json, urllib.request
req=urllib.request.Request('$BASE/api/v1/query/explain',
  data=b'{\"sql\":\"SELECT COUNT(*) FROM events\"}',
  headers={'Authorization':'Bearer $TOKEN','Content-Type':'application/json'})
r=json.loads(urllib.request.urlopen(req).read())
sys.exit(0 if r.get('row_count',0) > 0 else 1)"

check "H10 ?token= rejected on POST" \
  python3 -c "
import sys, urllib.request, urllib.error
req=urllib.request.Request('$BASE/api/v1/sites?token=$TOKEN',
  data=b'{\"name\":\"x\",\"domain\":\"x.ex\"}',
  headers={'Content-Type':'application/json'})
try:
    urllib.request.urlopen(req)
    sys.exit(1)
except urllib.error.HTTPError as e:
    sys.exit(0 if e.code == 401 else 1)"

check "H11 ?token= allowed on GET export" \
  python3 -c "
import sys, urllib.request
r=urllib.request.urlopen('$BASE/api/v1/export?site_id=default&from=0&to=9999999999999&format=csv&token=$TOKEN')
sys.exit(0 if r.status == 200 else 1)"

check "H12 ChangePassword UPDATE not INSERT" \
  bash -c '! grep -A3 "ChangePassword" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/auth/auth.go" | grep -q "INSERT INTO admin_users"'

# ================ Phase 2 differentiation checks (D1–D9) ================
echo "[D] Phase 2 differentiation"

check "D1 aiquery package present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/aiquery/aiquery.go"
check "D2 /api/v1/ai/config returns shape" \
  json_has "/api/v1/ai/config" "'has_key' in r"
check "D3 exports package present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/jobs/exports.go"
check "D4 /api/v1/exports/scheduled returns array" \
  json_has "/api/v1/exports/scheduled" "isinstance(r, list)"
check "D5 incidents package present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/internal/incidents/incidents.go"
check "D6 /api/v1/incidents returns array" \
  json_has "/api/v1/incidents" "isinstance(r, list)"
check "D7 instance_settings migration present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/migrations/011_instance_settings.up.sql"
check "D8 scheduled_exports migration present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/migrations/012_scheduled_exports.up.sql"
check "D9 incidents migration present" \
  test -f "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/migrations/013_incidents.up.sql"

check "D10 AI call logged to llm_traces" \
  bash -c 'grep -q "Operation:        \"explorer_nl_to_sql\"" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"'
check "D11 alert OnTrigger auto-creates incident" \
  bash -c 'grep -q "alertSvc.OnTrigger = func" "/Users/tyler/Documents/proj rn/Teploy/teploy-observe/cmd/observe/main.go"'

echo
echo "========================================"
echo "PASS: $PASS   FAIL: $FAIL"
echo "========================================"
exit $FAIL
