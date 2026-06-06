package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/neutronauth"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/config"
	"github.com/useteploy/teploy-observe/internal/dogfood"
	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/export"
	"github.com/useteploy/teploy-observe/internal/backup"
	"github.com/useteploy/teploy-observe/internal/aiquery"
	"github.com/useteploy/teploy-observe/internal/incidents"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/jobs"
	"github.com/useteploy/teploy-observe/internal/live"
	"github.com/useteploy/teploy-observe/internal/meta"
	"github.com/useteploy/teploy-observe/internal/query"
	"github.com/useteploy/teploy-observe/internal/seed"
	"github.com/useteploy/teploy-observe/internal/share"
	"github.com/useteploy/teploy-observe/internal/sites"
	"github.com/useteploy/teploy-observe/internal/dashboards"
	"github.com/useteploy/teploy-observe/internal/experiments"
	"github.com/useteploy/teploy-observe/internal/explorer"
	"github.com/useteploy/teploy-observe/internal/flags"
	"github.com/useteploy/teploy-observe/internal/feedback"
	"github.com/useteploy/teploy-observe/internal/infra"
	"github.com/useteploy/teploy-observe/internal/llm"
	"github.com/useteploy/teploy-observe/internal/groups"
	"github.com/useteploy/teploy-observe/internal/heatmaps"
	"github.com/useteploy/teploy-observe/internal/integrations"
	"github.com/useteploy/teploy-observe/internal/logs"
	"github.com/useteploy/teploy-observe/internal/monitoring"
	"github.com/useteploy/teploy-observe/internal/platform"
	"github.com/useteploy/teploy-observe/internal/reports"
	"github.com/useteploy/teploy-observe/internal/replays"
	"github.com/useteploy/teploy-observe/internal/sourcemaps"
	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/metrics"
	"github.com/useteploy/teploy-observe/internal/persons"
	"github.com/useteploy/teploy-observe/internal/sso"
	"github.com/useteploy/teploy-observe/internal/surveys"
	"github.com/useteploy/teploy-observe/internal/tracking"
	"github.com/useteploy/teploy-observe/internal/tracing"
	"github.com/useteploy/teploy-observe/internal/upgrade"
	"github.com/useteploy/teploy-observe/internal/views"
)

// version is injected at build time by goreleaser via -X main.version.
// Defaults to "dev" for `go run`/`go install` builds outside a release.
var version = "dev"

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed all:ui/dist
var uiFS embed.FS

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	// Subcommand dispatch. Default (no args) runs the HTTP server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			runBackup(cfg, logger)
			return
		case "restore":
			runRestore(cfg, logger)
			return
		case "version":
			fmt.Println("teploy-observe " + version)
			return
		case "upgrade":
			runUpgrade(cfg, logger, os.Args[2:])
			return
		case "reindex":
			runReindex(cfg, logger, os.Args[2:])
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	// Connect to Nucleus with retry
	ctx := context.Background()
	var db *nucleus.Client
	maxRetries := 15
	for i := 0; i < maxRetries; i++ {
		var err error
		db, err = nucleus.Connect(ctx, cfg.NucleusURL)
		if err == nil {
			break
		}
		if i == maxRetries-1 {
			logger.Error("failed to connect to nucleus after retries", "err", err, "attempts", maxRetries)
			os.Exit(1)
		}
		wait := time.Duration(i+1) * 2 * time.Second
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		logger.Info("waiting for nucleus...", "attempt", i+1, "retry_in", wait)
		time.Sleep(wait)
	}
	logger.Info("connected to nucleus")

	// Run migrations
	migrations, err := nucleus.LoadMigrations(migrationsFS)
	if err != nil {
		logger.Error("failed to load migrations", "err", err)
		os.Exit(1)
	}
	if err := db.Migrate(ctx, migrations); err != nil {
		logger.Error("failed to run migrations", "err", err)
		os.Exit(1)
	}
	logger.Info("migrations complete")

	// Generate any secret not supplied via config, so there's no predictable
	// hardcoded default. A generated admin password is surfaced once below; a
	// generated session salt rotates session/visitor IDs on restart (set
	// OBSERVE_SESSION_SALT to keep them stable).
	generatedAdminPw := false
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = auth.RandomSecret()
		generatedAdminPw = true
	}
	if cfg.SessionSalt == "" {
		cfg.SessionSalt = auth.RandomSecret()
		logger.Warn("OBSERVE_SESSION_SALT not set — using a random salt; set it to keep session/visitor IDs stable across restarts")
	}

	// Auth service
	authSvc := auth.NewAuthService(db, cfg.JWTSecret, logger)
	createdAdmin, err := authSvc.EnsureAdmin(ctx, cfg.AdminUser, cfg.AdminPassword)
	if err != nil {
		logger.Error("failed to ensure admin user", "err", err)
		os.Exit(1)
	}
	if createdAdmin && generatedAdminPw {
		logger.Warn("generated a random admin password — SAVE THIS NOW (set OBSERVE_ADMIN_PASSWORD to choose your own)",
			"username", cfg.AdminUser, "password", cfg.AdminPassword)
	}

	// Site service
	siteSvc := sites.NewSiteService(db)
	if err := siteSvc.EnsureDefault(ctx); err != nil {
		logger.Error("failed to ensure default site", "err", err)
		os.Exit(1)
	}

	// Error tracking services — declared before seed so the demo path can
	// use the canonical IngestErrorEvent wrapper (INSERT + issue resolve +
	// FTS index + distinct_id hashing in one shot).
	srcmapSvc := sourcemaps.NewSourceMapService(db)
	issueSvc := obserrors.NewIssueService(db)
	searchSvc := obserrors.NewSearchService(db)
	errorHandler := obserrors.NewErrorHandler(db, issueSvc, searchSvc, srcmapSvc).
		WithPrivacy(siteSvc.PrivacyConfig, cfg.SessionSalt)

	// Seed demo data for empty tables unless disabled.
	if os.Getenv("OBSERVE_SEED_DEMO") != "false" {
		seed.Run(ctx, db, errorHandler, logger)
	}

	// Ingestion buffer with WAL-backed durability. The queue directory
	// survives process restarts; pending events are replayed on Attach.
	queueDir := os.Getenv("OBSERVE_QUEUE_DIR")
	if queueDir == "" {
		queueDir = filepath.Join(os.Getenv("OBSERVE_DATA_DIR"), "queue")
	}
	buf := ingest.NewBuffer(db, cfg.BufferSize, cfg.FlushSize, cfg.FlushInterval, logger)
	maxQueueBytes := int64(64 * 1024 * 1024) // 64 MiB per WAL file before compaction
	if eventsQ, err := ingest.NewDiskQueue(queueDir, "events", 500*time.Millisecond, maxQueueBytes, logger); err == nil {
		if err := buf.AttachQueue(eventsQ); err != nil {
			logger.Warn("ingest queue: attach failed", "err", err)
		}
	} else {
		logger.Warn("ingest queue: init failed, running in-memory only", "err", err)
	}

	// Stats service
	statsSvc := query.NewStatsService(db)

	// Live event stream service
	liveSvc := live.NewLiveService(db, logger)

	// Share link service
	shareSvc := share.NewShareService(db)

	// Export service
	exportSvc := export.NewExportService(db)

	// Tracing services
	traceIngest := tracing.NewIngestService(db)
	traceQuery := tracing.NewQueryService(db)

	// Metrics service (W3.A Phase 1 — OTLP metrics ingest + query)
	metricsSvc := metrics.NewService(db).WithLogger(logger)

	// C2 (Wave 4) — persons + cohorts. Persons is read-only (aggregate
	// over events.distinct_id). Cohorts owns its own table (migration
	// 023) and exposes MembersForFilter to the stats service so any
	// analytics chart can opt in to a cohort filter via ?cohort_id=X.
	personsSvc := persons.NewService(db)
	cohortsSvc := cohorts.NewService(db)
	statsSvc.WithCohortResolver(cohortsSvc.MembersForFilter)

	// Platform services
	userSvc := platform.NewUserService(db)
	webhookSvc := platform.NewWebhookService(db, logger)
	alertSvc := platform.NewAlertService(db, logger, webhookSvc)

	// Feature expansion services
	reportSvc := reports.NewReportService(db, logger)
	integrationSvc := integrations.NewIntegrationService(db, logger)
	feedbackSvc := feedback.NewFeedbackService(db)
	viewSvc := views.NewViewService(db)
	explorerSvc := explorer.NewExplorerService(db)
	llmSvc := llm.NewLLMService(db)
	infraSvc := infra.NewInfraService(db)
	pipelineSvc := logs.NewPipelineService(db)
	groupSvc := groups.NewGroupService(db)
	ssoSvc := sso.NewSSOService(db)
	flagSvc := flags.NewFlagService(db)
	experimentSvc := experiments.NewExperimentService(db)
	surveySvc := surveys.NewSurveyService(db)
	logSvc := logs.NewLogService(db)
	logSvc.SetPipelines(pipelineSvc)
	uptimeSvc := monitoring.NewUptimeService(db, logger)
	cronSvc := monitoring.NewCronService(db, logger)
	linkSvc := tracking.NewLinkService(db)
	dashSvc := dashboards.NewDashboardService(db).WithMetrics(metricsSvc)
	replaySvc := replays.NewReplayService(db).
		WithLogger(logger).
		WithPrivacy(siteSvc.PrivacyConfig, cfg.SessionSalt)
	heatmapsSvc := heatmaps.NewService(db)
	aiSvc := aiquery.NewService(db, logger)
	aiSchema := aiquery.NewSchemaCard(db)
	scheduledExportSvc := jobs.NewExportService(db, explorerSvc, logger)
	incidentSvc := incidents.NewService(db)

	// A cron check-in resolves any open missed-cron incident for that monitor.
	cronSvc.OnCheckin = func(ctx context.Context, c monitoring.CronMonitor) {
		if err := incidentSvc.CloseByRule(ctx, "cron:"+c.CronID); err != nil {
			logger.Warn("cron incident auto-resolve failed", "cron", c.CronID, "err", err)
		}
	}

	// W2.B: cross-site board summary. SiteLookup adapts the SiteService
	// (Get returns Site) to the BoardService's small SiteMeta tuple so
	// internal/query doesn't have to import internal/sites.
	boardSvc := query.NewBoardService(db, func(ctx context.Context, id string) (query.SiteMeta, bool) {
		s, err := siteSvc.Get(ctx, id)
		if err != nil || s.SiteID == "" {
			return query.SiteMeta{}, false
		}
		return query.SiteMeta{SiteID: s.SiteID, Name: s.Name, Domain: s.Domain}, true
	})

	// Auto-declare an incident whenever an alert rule fires. Dedup in
	// the incident service by keying on rule_id + open state — a
	// repeatedly-firing rule should open one incident and stay open.
	alertSvc.OnTrigger = func(ctx context.Context, rule platform.AlertRule, value float64) {
		active, _ := incidentSvc.ActiveByRule(ctx, rule.RuleID)
		if len(active) > 0 {
			return
		}
		_, err := incidentSvc.Create(ctx, incidents.CreateInput{
			SiteID:      rule.SiteID,
			Title:       rule.Name,
			Description: fmt.Sprintf("alert rule fired: %s=%.2f (threshold %.2f)", rule.Metric, value, rule.Threshold),
			Severity:    "warning",
			Source:      incidents.SourceAlert,
			RuleID:      rule.RuleID,
		}, "alert")
		if err != nil {
			logger.Warn("incident auto-create failed", "rule", rule.RuleID, "err", err)
		}
	}

	// Error buffer (async processing for throughput)
	errorBuf := obserrors.NewErrorBuffer(errorHandler, 50000, 100, 2*time.Second, logger)

	// Background jobs: rollups + retention
	rollups := jobs.NewRollupService(db, logger)
	retention := jobs.NewRetentionService(db, logger, cfg.RawRetentionDays, cfg.HourlyRetentionDays)
	scheduler := jobs.NewScheduler(rollups, retention, logger)

	// Self-observability service
	metaSvc := meta.New(db, retention, version)

	// Self-instrumentation: trace every HTTP request + ship panics to /errors,
	// scoped to site_id=_meta. Off by default (adds ingest load to the same
	// process) — opt in with OBSERVE_DOGFOOD=true. Failures are non-fatal.
	var self *dogfood.Self
	if os.Getenv("OBSERVE_DOGFOOD") == "true" {
		selfEndpoint := selfMonitorEndpoint(cfg.Addr)
		s, err := dogfood.Setup(ctx, db, authSvc, selfEndpoint, logger.Handler())
		if err != nil {
			logger.Warn("dogfood self-monitoring disabled", "err", err)
			self = &dogfood.Self{}
		} else {
			self = s
			logger.Info("dogfood self-monitoring enabled", "endpoint", selfEndpoint, "site_id", dogfood.MetaSiteID)
		}
	} else {
		self = &dogfood.Self{}
	}

	// Build app
	app := neutron.New(
		neutron.WithLogger(logger),
		neutron.WithMiddleware(self.RecoverMiddleware, self.TraceMiddleware),
		neutron.WithLifecycle(db.LifecycleHook()),
		neutron.WithLifecycle(neutron.LifecycleHook{
			Name: "ingest-buffer",
			OnStart: func(ctx context.Context) error {
				buf.Start()
				return nil
			},
			OnStop: func(ctx context.Context) error {
				buf.Stop()
				return nil
			},
		}),
		neutron.WithLifecycle(neutron.LifecycleHook{
			Name: "scheduler",
			OnStart: func(ctx context.Context) error {
				scheduler.Start()
				return nil
			},
			OnStop: func(ctx context.Context) error {
				scheduler.Stop()
				return nil
			},
		}),
		neutron.WithMiddleware(ingest.RequestInfoMiddleware(ingest.ParseTrustedProxies(cfg.TrustedProxies))),
		neutron.WithMiddleware(config.DemoModeMiddleware(cfg.DemoMode)),
		neutron.WithNucleusChecker(db),
		neutron.WithOpenAPIInfo("Teploy Observe", version),
		// /docs is reserved for the in-product docs page; Swagger UI lives at /api/docs.
		neutron.DisableDefaultDocs(),
	)

	r := app.Router()

	// --- Auth API (public) ---
	// IP-keyed rate limit on login throttles password brute-force (10/min/IP).
	loginLimiter := ingest.NewRateLimiter(10, time.Minute, 10)
	loginGroup := r.Group("/api/v1", ipRateLimitMW(loginLimiter))
	neutron.Post(loginGroup, "/auth/login", loginHandler(authSvc),
		neutron.WithTags("auth"),
		neutron.WithSummary("Login and receive JWT token"),
	)
	// --- Ingestion API (API key auth, wildcard CORS, rate limited) ---
	rateLimit := cfg.RateLimit
	if rateLimit <= 0 {
		rateLimit = 1000
	}
	rateLimiter := ingest.NewRateLimiter(rateLimit, time.Second, rateLimit*2)
	// Hydrate per-site caps from the sites table so the first ingest after
	// a restart honors admin overrides.
	if caps, err := siteSvc.ListRatelimits(ctx); err == nil {
		for siteID, rps := range caps {
			if rps > 0 && rps != rateLimit {
				rateLimiter.SetSiteCap(siteID, rps)
			}
		}
	} else {
		logger.Warn("could not load per-site ratelimits", "err", err)
	}
	ingestCORS := neutron.CORS(neutron.CORSOptions{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"POST", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "X-API-Key"},
		MaxAge:       86400,
	})
	// Auth runs before the limiter so the limiter can key on site_id. BodyLimit
	// rejects oversized payloads with 413 before they are buffered/decoded
	// (batch is capped at 100 events, so 2 MiB is ample).
	apiKeyMW := auth.APIKeyAuthMiddleware(authSvc, cfg.SiteID)
	ingestGroup := r.Group("/api/v1", ingestCORS, apiKeyMW, rateLimiter.Middleware, neutron.BodyLimit(2<<20))
	neutron.Post(ingestGroup, "/events", ingest.Handler(buf, cfg.SessionSalt, siteSvc),
		neutron.WithTags("ingest"),
		neutron.WithSummary("Ingest analytics event"),
	)
	neutron.Post(ingestGroup, "/events/batch", ingest.BatchHandler(buf, cfg.SessionSalt, siteSvc),
		neutron.WithTags("ingest"),
		neutron.WithSummary("Ingest batch of analytics events"),
	)

	// --- Error Ingestion (API key auth, wildcard CORS) ---
	neutron.Post(ingestGroup, "/errors", errorIngestHandler(errorBuf),
		neutron.WithTags("errors"),
		neutron.WithSummary("Ingest error event"),
	)

	// --- OTLP Trace Ingestion (API key auth) ---
	neutron.Post(ingestGroup, "/v1/traces", otlpTraceHandler(traceIngest),
		neutron.WithTags("traces"),
		neutron.WithSummary("Ingest OTLP traces"),
	)

	// --- Experiment exposure/conversion ingest (API key auth) ---
	// Without these write paths the A/B results page is always empty. site_id
	// is taken from the validated key, not the body.
	neutron.Post(ingestGroup, "/experiments/expose", experimentExposeHandler(experimentSvc),
		neutron.WithTags("experiments"),
		neutron.WithSummary("Record an experiment exposure"),
	)
	neutron.Post(ingestGroup, "/experiments/convert", experimentConvertHandler(experimentSvc),
		neutron.WithTags("experiments"),
		neutron.WithSummary("Record an experiment conversion"),
	)

	// --- Stats API (JWT auth) ---
	jwtMW := auth.JWTAuthMiddleware(authSvc)
	// Role enforcers layered on top of JWT. Admin = full access to config and
	// destructive endpoints. Editor+Admin = content mutations (flags,
	// dashboards, queries, etc). Viewer = reads only (no explicit MW; JWT alone
	// grants read access).
	requireAdmin := auth.RequireRole(authSvc, auth.RoleAdmin)
	requireEditor := auth.RequireRole(authSvc, auth.RoleAdmin, auth.RoleEditor)
	// Stats reads accept EITHER a JWT or a valid public share token. With a
	// share token the request is allowed (GET only) but site_id is FORCED to the
	// token's site, so a token can only read its own dashboard data — this is
	// what makes public share links actually render instead of bouncing the
	// viewer to /login.
	query.RegisterRoutes(r, statsSvc, jwtOrShareMW(jwtMW, shareSvc))

	// --- Live event stream (JWT auth, registered on root router to avoid group prefix bug) ---
	r.Handle("GET /api/v1/stats/live", jwtMW(liveSvc.Handler()))

	// --- Data export (JWT auth) ---
	// Raw data export is editor+; a viewer should not be able to exfiltrate the
	// full event/session stream or trigger a heavy scan.
	r.Handle("GET /api/v1/export", jwtMW(requireEditor(exportSvc.Handler())))

	// --- Password change (JWT auth) ---
	neutron.Post(r.Group("/api/v1/auth", jwtMW), "/password", changePasswordHandler(authSvc),
		neutron.WithTags("auth"),
		neutron.WithSummary("Change password"),
	)

	// --- Issue management API (JWT auth) ---
	issueGroup := r.Group("/api/v1/issues", jwtMW)
	neutron.Get(issueGroup, "", listIssuesHandler(issueSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("List issues for a site"),
	)
	neutron.Get(issueGroup, "/{issue_id}", getIssueHandler(issueSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("Get issue detail"),
	)
	neutron.Post(issueGroup, "/{issue_id}/status", updateIssueStatusHandler(issueSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("Update issue status"),
	)
	neutron.Get(issueGroup, "/{issue_id}/events", issueEventsHandler(issueSvc, srcmapSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("List error events for an issue"),
	)
	neutron.Get(issueGroup, "/{issue_id}/session", issueSessionHandler(issueSvc, statsSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("Get analytics session correlated with an error"),
	)
	neutron.Get(issueGroup, "/search", searchIssuesHandler(searchSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("Full-text search across error messages"),
	)
	neutron.Get(issueGroup, "/daily", dailyErrorCountsHandler(issueSvc),
		neutron.WithTags("issues"),
		neutron.WithSummary("Daily error counts (zero-filled) for a time window"),
	)

	// --- Trace query API (JWT auth) ---
	traceGroup := r.Group("/api/v1/traces", jwtMW)
	neutron.Get(traceGroup, "/services", listServicesHandler(traceQuery),
		neutron.WithTags("traces"),
		neutron.WithSummary("List services with RED metrics"),
	)
	neutron.Get(traceGroup, "/services/{service}/operations", listOperationsHandler(traceQuery),
		neutron.WithTags("traces"),
		neutron.WithSummary("List operations for a service"),
	)
	neutron.Get(traceGroup, "/search", searchTracesHandler(traceQuery),
		neutron.WithTags("traces"),
		neutron.WithSummary("Search traces with filters"),
	)
	neutron.Get(traceGroup, "/{trace_id}", getTraceHandler(traceQuery),
		neutron.WithTags("traces"),
		neutron.WithSummary("Get trace waterfall"),
	)
	neutron.Get(traceGroup, "/{trace_id}/errors", traceErrorsHandler(traceQuery),
		neutron.WithTags("traces"),
		neutron.WithSummary("Get errors correlated with a trace"),
	)
	neutron.Get(traceGroup, "/dependencies", serviceDepsHandler(traceQuery),
		neutron.WithTags("traces"),
		neutron.WithSummary("Get service dependency graph"),
	)

	// --- Platform API (JWT auth, admin-only for writes) ---
	platformGroup := r.Group("/api/v1/platform", jwtMW)
	platformAdmin := platformGroup.Group("", requireAdmin)
	neutron.Get(platformGroup, "/users", listUsersHandler(userSvc),
		neutron.WithTags("platform"), neutron.WithSummary("List users"))
	neutron.Post(platformAdmin, "/users", createUserHandler(userSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Create/invite user"))
	neutron.Post(platformAdmin, "/users/{user_id}/role", updateUserRoleHandler(userSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Update user role"))
	neutron.Get(platformGroup, "/alerts/rules", listAlertRulesHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("List alert rules"))
	neutron.Post(platformAdmin, "/alerts/rules", createAlertRuleHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Create alert rule"))
	neutron.Post(platformAdmin, "/alerts/rules/{rule_id}/silence", silenceAlertRuleHandler(alertSvc),
		neutron.WithTags("alerts"), neutron.WithSummary("Silence an alert rule for a duration"))
	neutron.Delete(platformAdmin, "/alerts/rules/{rule_id}", deleteAlertRuleHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Delete alert rule"))
	neutron.Get(platformGroup, "/alerts/history", alertHistoryHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Alert history"))
	neutron.Get(platformGroup, "/webhooks", listWebhooksHandler(webhookSvc),
		neutron.WithTags("platform"), neutron.WithSummary("List webhooks"))
	neutron.Post(platformAdmin, "/webhooks", createWebhookHandler(webhookSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Create webhook"))
	neutron.Delete(platformAdmin, "/webhooks/{webhook_id}", deleteWebhookHandler(webhookSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Delete webhook"))

	// --- Share link management API (JWT auth; editor+ for writes) ---
	shareGroup := r.Group("/api/v1", jwtMW)
	shareEditor := shareGroup.Group("", requireEditor)
	neutron.Post(shareEditor, "/sites/{site_id}/share", createShareHandler(shareSvc, siteSvc),
		neutron.WithTags("share"),
		neutron.WithSummary("Create a share link for a site"),
	)
	neutron.Get(shareGroup, "/sites/{site_id}/share", listShareHandler(shareSvc),
		neutron.WithTags("share"),
		neutron.WithSummary("List share links for a site"),
	)
	neutron.Delete(shareEditor, "/share/{token}", revokeShareHandler(shareSvc),
		neutron.WithTags("share"),
		neutron.WithSummary("Revoke a share link"),
	)

	// --- Site management API (JWT auth; admin-only for sites/keys writes) ---
	siteGroup := r.Group("/api/v1", jwtMW)
	siteAdmin := siteGroup.Group("", requireAdmin)
	neutron.Get(siteGroup, "/sites", listSitesHandler(siteSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("List all sites"),
	)
	neutron.Post(siteAdmin, "/sites", createSiteHandler(siteSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Create a new site"),
	)
	neutron.Delete(siteAdmin, "/sites/{site_id}", deleteSiteHandler(siteSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Delete a site"),
	)
	neutron.Post(siteAdmin, "/sites/{site_id}/keys", createAPIKeyHandler(authSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Generate API key for a site"),
	)
	// Admin-only: API keys are secrets; a viewer/editor must not enumerate them.
	neutron.Get(siteAdmin, "/sites/{site_id}/keys", listAPIKeysHandler(authSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("List API keys for a site"),
	)
	neutron.Delete(siteAdmin, "/keys/{key_id}", revokeAPIKeyHandler(authSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Revoke an API key"),
	)
	neutron.Put(siteAdmin, "/sites/{site_id}/ratelimit", setSiteRatelimitHandler(siteSvc, rateLimiter),
		neutron.WithTags("sites"),
		neutron.WithSummary("Set events-per-second cap for a site"),
	)

	// --- Feedback (public, no auth for user submissions) ---
	r.HandleFunc("POST /api/v1/feedback", feedbackSubmitHandler(feedbackSvc))

	// --- Integrations (JWT auth; admin-only writes) ---
	intGroup := r.Group("/api/v1/integrations", jwtMW)
	intAdmin := intGroup.Group("", requireAdmin)
	neutron.Get(intGroup, "", listIntegrationsHandler(integrationSvc),
		neutron.WithTags("integrations"), neutron.WithSummary("List integrations"))
	neutron.Post(intAdmin, "", createIntegrationHandler(integrationSvc),
		neutron.WithTags("integrations"), neutron.WithSummary("Create integration"))
	neutron.Post(intAdmin, "/{integration_id}/test", testIntegrationHandler(integrationSvc),
		neutron.WithTags("integrations"), neutron.WithSummary("Deliver a test payload"))
	neutron.Get(intGroup, "/{integration_id}/deliveries", listDeliveriesHandler(integrationSvc),
		neutron.WithTags("integrations"), neutron.WithSummary("List recent delivery attempts"))
	neutron.Post(intAdmin, "/deliveries/{delivery_id}/replay", replayDeliveryHandler(integrationSvc),
		neutron.WithTags("integrations"), neutron.WithSummary("Replay a prior delivery"))
	neutron.Delete(intAdmin, "/{integration_id}", deleteIntegrationHandler(integrationSvc),
		neutron.WithTags("integrations"), neutron.WithSummary("Delete integration"))

	// --- Saved views (JWT auth; editor+ for writes) ---
	viewGroup := r.Group("/api/v1/views", jwtMW)
	viewEditor := viewGroup.Group("", requireEditor)
	neutron.Get(viewGroup, "", listViewsHandler(viewSvc),
		neutron.WithTags("views"), neutron.WithSummary("List saved views"))
	neutron.Post(viewEditor, "", createViewHandler(viewSvc),
		neutron.WithTags("views"), neutron.WithSummary("Create saved view"))
	neutron.Delete(viewEditor, "/{view_id}", deleteViewHandler(viewSvc),
		neutron.WithTags("views"), neutron.WithSummary("Delete saved view"))

	// --- Feedback list (JWT auth) ---
	neutron.Get(r.Group("/api/v1/feedback", jwtMW), "/list", listFeedbackHandler(feedbackSvc),
		neutron.WithTags("feedback"), neutron.WithSummary("List user feedback"))

	// --- Reports (JWT auth; editor+ for writes) ---
	reportGroup := r.Group("/api/v1/reports", jwtMW)
	reportEditor := reportGroup.Group("", requireEditor)
	neutron.Get(reportGroup, "", listReportsHandler(reportSvc),
		neutron.WithTags("reports"), neutron.WithSummary("List report schedules"))
	neutron.Post(reportEditor, "", createReportHandler(reportSvc),
		neutron.WithTags("reports"), neutron.WithSummary("Create report schedule"))
	neutron.Delete(reportEditor, "/{schedule_id}", deleteReportHandler(reportSvc),
		neutron.WithTags("reports"), neutron.WithSummary("Delete report schedule"))

	// --- Groups (JWT auth; admin-only writes) ---
	grpGroup := r.Group("/api/v1/groups", jwtMW)
	grpAdmin := grpGroup.Group("", requireAdmin)
	neutron.Get(grpGroup, "", listGroupsHandler(groupSvc),
		neutron.WithTags("groups"), neutron.WithSummary("List groups"))
	neutron.Post(grpAdmin, "", createGroupHandler(groupSvc),
		neutron.WithTags("groups"), neutron.WithSummary("Create group"))
	neutron.Post(grpAdmin, "/{group_id}/members", addGroupMemberHandler(groupSvc),
		neutron.WithTags("groups"), neutron.WithSummary("Add member to group"))

	// --- SSO (public endpoints) ---
	r.HandleFunc("GET /api/v1/sso/metadata", ssoMetadataHandler(ssoSvc, cfg.Addr))
	r.HandleFunc("POST /api/v1/sso/callback", ssoSvc.SAMLCallbackHandler())
	// Admin-only: SSO configs include IdP certificates/metadata; writes are
	// admin-only, so reads are too.
	neutron.Get(r.Group("/api/v1/sso", jwtMW, requireAdmin), "/configs", listSSOHandler(ssoSvc),
		neutron.WithTags("sso"), neutron.WithSummary("List SSO configs"))
	neutron.Post(r.Group("/api/v1/sso", jwtMW, requireAdmin), "/configs", createSSOHandler(ssoSvc),
		neutron.WithTags("sso"), neutron.WithSummary("Create SSO config"))

	// --- LLM observability (API key auth for ingest, JWT for queries) ---
	r.HandleFunc("POST /api/v1/llm/ingest", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var input llm.LLMInput
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			// Was silently ignored: a malformed body proceeded as an empty
			// record and returned success.
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error":"invalid JSON body"}`))
			return
		}
		if input.SiteID == "" { input.SiteID = "default" }
		result, err := llmSvc.Ingest(req.Context(), input)
		if err != nil {
			// Was HTTP 200 with the raw err reflected into hand-built JSON
			// (success-on-failure + an escaping hazard). Return 5xx with a
			// generic message; the detail is logged server-side.
			logger.Error("llm ingest failed", "site", input.SiteID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"ok":false,"error":"ingest failed"}`))
			return
		}
		json.NewEncoder(w).Encode(result)
	})
	llmGroup := r.Group("/api/v1/llm", jwtMW)
	neutron.Get(llmGroup, "/stats", llmStatsHandler(llmSvc),
		neutron.WithTags("llm"), neutron.WithSummary("LLM usage stats"))
	neutron.Get(llmGroup, "/models", llmModelsHandler(llmSvc),
		neutron.WithTags("llm"), neutron.WithSummary("LLM model breakdown"))
	neutron.Get(llmGroup, "/traces", llmTracesHandler(llmSvc),
		neutron.WithTags("llm"), neutron.WithSummary("Recent LLM traces"))

	// --- Infrastructure monitoring (public agent reports, JWT for queries) ---
	r.HandleFunc("POST /api/v1/infra/report", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var input infra.MetricInput
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error":"invalid JSON body"}`))
			return
		}
		if input.SiteID == "" {
			input.SiteID = "default"
		}
		// This endpoint is keyless (agent flow), so validate the site exists to
		// stop anonymous callers creating junk sites / polluting another site's
		// host metrics, and bound the identifier lengths.
		if len(input.SiteID) > 64 || len(input.Hostname) > 253 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error":"field too long"}`))
			return
		}
		if _, err := siteSvc.Get(req.Context(), input.SiteID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error":"unknown site_id"}`))
			return
		}
		if err := infraSvc.Report(req.Context(), input); err != nil {
			// Was HTTP 200 {"ok":false}: the agent treated a backend failure as
			// delivered and never retried. Return 5xx so it does.
			logger.Error("infra report failed", "site", input.SiteID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"ok":false,"error":"report failed"}`))
			return
		}
		fmt.Fprintf(w, `{"ok":true}`)
	})
	infraGroup := r.Group("/api/v1/infra", jwtMW)
	neutron.Get(infraGroup, "/hosts", infraHostsHandler(infraSvc),
		neutron.WithTags("infra"), neutron.WithSummary("List monitored hosts"))
	neutron.Get(infraGroup, "/hosts/{hostname}/history", infraHistoryHandler(infraSvc),
		neutron.WithTags("infra"), neutron.WithSummary("Host metric history"))

	// --- Log pipelines (JWT auth; admin-only writes) ---
	pipeGroup := r.Group("/api/v1/log-pipelines", jwtMW)
	pipeAdmin := pipeGroup.Group("", requireAdmin)
	neutron.Get(pipeGroup, "", listPipelinesHandler(pipelineSvc),
		neutron.WithTags("logs"), neutron.WithSummary("List log pipelines"))
	neutron.Post(pipeAdmin, "", createPipelineHandler(pipelineSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Create log pipeline"))

	// --- OTLP standard endpoint (compatible with all OTLP HTTP exporters) ---
	// Authenticated like every other ingest path: the API-key middleware
	// resolves site_id into the request context, so a client cannot inject
	// spans for another tenant via the X-Observe-Site header. Standard OTLP
	// exporters supply the key via OTEL_EXPORTER_OTLP_HEADERS=X-API-Key=...
	otlpHandler := tracing.NewOTLPHandler(traceIngest)
	r.Handle("POST /v1/traces", apiKeyMW(otlpHandler))

	// --- SQL Explorer (JWT + editor role; query tables stays read-only) ---
	// Admin-only: the raw SQL explorer can read any table, including
	// secret-bearing ones (instance_settings, share_links.token,
	// sites.session_salt, webhooks.secret, api_keys.key_hash). An editor must
	// not be able to exfiltrate those, and /query/tables previously had no role
	// gate at all.
	r.Handle("POST /api/v1/query", jwtMW(requireAdmin(explorerQueryHandler(explorerSvc))))
	r.Handle("POST /api/v1/query/explain", jwtMW(requireAdmin(explorerExplainHandler(explorerSvc))))
	r.Handle("GET /api/v1/query/tables", jwtMW(requireAdmin(explorerTablesHandler(explorerSvc))))

	// --- AI query assistant (admin reads/writes config; editor+ may ask) ---
	r.Handle("GET /api/v1/ai/config", jwtMW(requireAdmin(aiConfigGetHandler(aiSvc))))
	r.Handle("PUT /api/v1/ai/config", jwtMW(requireAdmin(aiConfigPutHandler(aiSvc))))
	r.Handle("POST /api/v1/ai/query", jwtMW(requireEditor(aiQueryHandler(aiSvc, aiSchema, llmSvc))))

	// --- Scheduled SQL exports (admin only: writes data externally) ---
	r.Handle("GET /api/v1/exports/scheduled", jwtMW(requireAdmin(exportsListHandler(scheduledExportSvc))))
	r.Handle("POST /api/v1/exports/scheduled", jwtMW(requireAdmin(exportsCreateHandler(scheduledExportSvc))))
	r.Handle("DELETE /api/v1/exports/scheduled/{export_id}", jwtMW(requireAdmin(exportsDeleteHandler(scheduledExportSvc))))
	r.Handle("POST /api/v1/exports/scheduled/{export_id}/run", jwtMW(requireAdmin(exportsRunNowHandler(scheduledExportSvc))))

	// --- Incidents (admin+editor may create/close; all roles may read) ---
	r.Handle("GET /api/v1/incidents", jwtMW(incidentsListHandler(incidentSvc)))
	r.Handle("POST /api/v1/incidents", jwtMW(requireEditor(incidentsCreateHandler(incidentSvc))))
	r.Handle("POST /api/v1/incidents/{incident_id}/close", jwtMW(requireEditor(incidentsCloseHandler(incidentSvc))))

	// --- Feature flags (JWT auth + public evaluate; editor+ writes) ---
	flagGroup := r.Group("/api/v1/flags", jwtMW)
	flagEditor := flagGroup.Group("", requireEditor)
	neutron.Get(flagGroup, "", listFlagsHandler(flagSvc),
		neutron.WithTags("flags"), neutron.WithSummary("List feature flags"))
	neutron.Post(flagEditor, "", createFlagHandler(flagSvc),
		neutron.WithTags("flags"), neutron.WithSummary("Create feature flag"))
	neutron.Post(flagEditor, "/{flag_id}/toggle", toggleFlagHandler(flagSvc),
		neutron.WithTags("flags"), neutron.WithSummary("Toggle feature flag"))
	neutron.Get(flagGroup, "/{flag_id}/history", flagHistoryHandler(flagSvc),
		neutron.WithTags("flags"), neutron.WithSummary("Flag change log"))
	// Public evaluate endpoint (no JWT, uses API key or site_id)
	// Public flag SDK endpoint: generous per-site/IP cap (60/min) for the
	// browser evaluation path.
	flagEvalLimiter := ingest.NewRateLimiter(60, time.Minute, 120)
	r.HandleFunc("POST /api/v1/flags/evaluate", flagEvaluateHandler(flagSvc, flagEvalLimiter))

	// --- Experiments (JWT auth; editor+ writes) ---
	expGroup := r.Group("/api/v1/experiments", jwtMW)
	expEditor := expGroup.Group("", requireEditor)
	neutron.Get(expGroup, "", listExperimentsHandler(experimentSvc),
		neutron.WithTags("experiments"), neutron.WithSummary("List experiments"))
	neutron.Post(expEditor, "", createExperimentHandler(experimentSvc),
		neutron.WithTags("experiments"), neutron.WithSummary("Create experiment"))
	neutron.Post(expEditor, "/{experiment_id}/start", startExperimentHandler(experimentSvc),
		neutron.WithTags("experiments"), neutron.WithSummary("Start experiment"))
	neutron.Post(expEditor, "/{experiment_id}/stop", stopExperimentHandler(experimentSvc),
		neutron.WithTags("experiments"), neutron.WithSummary("Stop experiment"))
	neutron.Get(expGroup, "/{experiment_id}/results", experimentResultsHandler(experimentSvc),
		neutron.WithTags("experiments"), neutron.WithSummary("Get experiment results"))

	// --- Surveys (JWT auth + public endpoints; editor+ writes) ---
	surveyGroup := r.Group("/api/v1/surveys", jwtMW)
	surveyEditor := surveyGroup.Group("", requireEditor)
	neutron.Get(surveyGroup, "", listSurveysHandler(surveySvc),
		neutron.WithTags("surveys"), neutron.WithSummary("List surveys"))
	neutron.Post(surveyEditor, "", createSurveyHandler(surveySvc),
		neutron.WithTags("surveys"), neutron.WithSummary("Create survey"))
	neutron.Post(surveyEditor, "/{survey_id}/activate", activateSurveyHandler(surveySvc),
		neutron.WithTags("surveys"), neutron.WithSummary("Activate survey"))
	neutron.Get(surveyGroup, "/{survey_id}/responses", surveyResponsesHandler(surveySvc),
		neutron.WithTags("surveys"), neutron.WithSummary("List survey responses"))
	// Public: get active surveys + submit response
	r.HandleFunc("GET /api/v1/surveys/active", activeSurveysPublicHandler(surveySvc))
	r.HandleFunc("POST /api/v1/surveys/respond", surveyRespondHandler(surveySvc))

	// --- Release health (JWT auth) ---
	releaseHealthSvc := obserrors.NewReleaseHealthService(db)
	releaseGroup := r.Group("/api/v1/releases", jwtMW)
	neutron.Get(releaseGroup, "", releaseHealthHandler(issueSvc),
		neutron.WithTags("releases"), neutron.WithSummary("Release error counts"))
	neutron.Get(releaseGroup, "/health", releaseHealthV2Handler(releaseHealthSvc),
		neutron.WithTags("releases"), neutron.WithSummary("Crash-free + adoption + error rate per release"))
	neutron.Get(releaseGroup, "/sparkline", releaseSparklineHandler(releaseHealthSvc),
		neutron.WithTags("releases"), neutron.WithSummary("Daily crash-free % series for one release"))

	// --- Log ingestion (API key auth) ---
	neutron.Post(ingestGroup, "/logs", logIngestHandler(logSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Ingest log entry"))

	// --- Replay ingestion (API key auth) ---
	neutron.Post(ingestGroup, "/replays", replayIngestHandler(replaySvc),
		neutron.WithTags("replays"), neutron.WithSummary("Ingest session replay events"))

	// --- API docs (Swagger UI) ---
	r.Handle("GET /api/docs", neutron.SwaggerUI(app.OpenAPI()))
	r.Handle("GET /api/docs/", neutron.SwaggerUI(app.OpenAPI()))

	// --- Self-observability (/meta) ---
	// Admin-only: the meta snapshot exposes build version + operational internals.
	metaGroup := r.Group("/api/v1", jwtMW, requireAdmin)
	neutron.Get(metaGroup, "/meta", func(ctx context.Context, _ neutron.Empty) (meta.Snapshot, error) {
		return metaSvc.Snapshot(ctx)
	}, neutron.WithTags("meta"), neutron.WithSummary("Observe self-observability snapshot"))

	// --- Public config (UI reads this to know about demo mode, etc.) ---
	r.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		demo := "false"
		if cfg.DemoMode {
			demo = "true"
		}
		_, _ = w.Write([]byte(`{"demo_mode":` + demo + `}`))
	})

	// --- Source maps (JWT auth) ---
	// Editor+: uploading a source map is a write; a viewer must not poison it.
	r.Handle("POST /api/v1/sourcemaps/upload", jwtMW(requireEditor(srcmapUploadHandler(srcmapSvc))))

	// --- Log query API (JWT auth) ---
	logGroup := r.Group("/api/v1/logs", jwtMW)
	neutron.Get(logGroup, "/search", logSearchHandler(logSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Search logs"))
	neutron.Get(logGroup, "/stats", logStatsHandler(logSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Log counts per level"))
	neutron.Get(logGroup, "/histogram", logHistogramHandler(logSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Log volume histogram by level"))
	r.Handle("GET /api/v1/logs/stream", jwtMW(logStreamHandler(logSvc)))

	// --- Goals API (JWT auth; editor+ writes) ---
	goalGroup := r.Group("/api/v1/goals", jwtMW)
	goalEditor := goalGroup.Group("", requireEditor)
	neutron.Get(goalGroup, "", listGoalsHandler(statsSvc),
		neutron.WithTags("goals"), neutron.WithSummary("List goals with conversions"))
	neutron.Post(goalEditor, "", createGoalHandler(statsSvc),
		neutron.WithTags("goals"), neutron.WithSummary("Create goal"))

	// --- Uptime monitors (JWT auth; admin-only writes) ---
	uptimeGroup := r.Group("/api/v1/monitors", jwtMW)
	uptimeAdmin := uptimeGroup.Group("", requireAdmin)
	neutron.Get(uptimeGroup, "", listMonitorsHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("List uptime monitors"))
	neutron.Post(uptimeAdmin, "", createMonitorHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("Create uptime monitor"))
	neutron.Get(uptimeGroup, "/{monitor_id}/results", monitorResultsHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("Get monitor results"))
	neutron.Delete(uptimeAdmin, "/{monitor_id}", deleteMonitorHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("Delete (disable) uptime monitor"))

	// --- Cron monitors (JWT auth + public checkin; editor+ writes) ---
	cronGroup := r.Group("/api/v1/crons", jwtMW)
	cronEditor := cronGroup.Group("", requireEditor)
	neutron.Get(cronGroup, "", listCronsHandler(cronSvc),
		neutron.WithTags("crons"), neutron.WithSummary("List cron monitors"))
	neutron.Post(cronEditor, "", createCronHandler(cronSvc),
		neutron.WithTags("crons"), neutron.WithSummary("Create cron monitor"))
	neutron.Delete(cronEditor, "/{cron_id}", deleteCronHandler(cronSvc),
		neutron.WithTags("crons"), neutron.WithSummary("Delete (disable) cron monitor"))
	// Public checkin (no auth). Preferred form is an opaque per-cron ping token
	// (returned at creation): possession of the token authorizes the heartbeat.
	r.HandleFunc("POST /api/v1/checkin/token/{ping_token}", cronCheckinByTokenHandler(cronSvc))
	r.HandleFunc("GET /api/v1/checkin/token/{ping_token}", cronCheckinByTokenHandler(cronSvc))
	// Legacy slug forms, retained for back-compat with existing monitors.
	r.HandleFunc("POST /api/v1/checkin/{site_id}/{slug}", cronCheckinHandler(cronSvc))
	r.HandleFunc("GET /api/v1/checkin/{site_id}/{slug}", cronCheckinHandler(cronSvc))
	r.HandleFunc("POST /api/v1/checkin/{slug}", cronCheckinHandler(cronSvc))
	r.HandleFunc("GET /api/v1/checkin/{slug}", cronCheckinHandler(cronSvc))

	// --- Dashboards (JWT auth; editor+ writes) ---
	dashGroup := r.Group("/api/v1/dashboards", jwtMW)
	dashEditor := dashGroup.Group("", requireEditor)
	neutron.Get(dashGroup, "", listDashboardsHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("List dashboards"))
	neutron.Post(dashEditor, "", createDashboardHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Create dashboard"))
	neutron.Get(dashGroup, "/{dashboard_id}", getDashboardHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Get dashboard with panels"))
	neutron.Post(dashEditor, "/{dashboard_id}/panels", addPanelHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Add panel to dashboard"))
	neutron.Delete(dashEditor, "/{dashboard_id}", deleteDashboardHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Delete dashboard"))
	neutron.Post(dashEditor, "/{dashboard_id}/panels/{panel_id}/layout", updatePanelLayoutHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Update panel position & size"))
	neutron.Post(dashGroup, "/{dashboard_id}/panels/{panel_id}/execute", executePanelHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Execute panel query"))

	// --- Replays (JWT auth) ---
	replayGroup := r.Group("/api/v1/replays", jwtMW)
	neutron.Get(replayGroup, "", listReplaysHandler(replaySvc),
		neutron.WithTags("replays"), neutron.WithSummary("List session replays"))
	neutron.Get(replayGroup, "/{replay_id}", getReplayHandler(replaySvc),
		neutron.WithTags("replays"), neutron.WithSummary("Get replay events"))
	neutron.Get(replayGroup, "/{replay_id}/issues", replayIssuesHandler(issueSvc),
		neutron.WithTags("replays"), neutron.WithSummary("List issues with events linked to this replay"))

	// --- Click heatmaps (JWT auth) ---
	heatmapGroup := r.Group("/api/v1/heatmaps", jwtMW)
	neutron.Get(heatmapGroup, "", queryHeatmapHandler(heatmapsSvc),
		neutron.WithTags("heatmaps"), neutron.WithSummary("Aggregated click heatmap for a URL"))

	// --- Tracked links ---
	linkGroup := r.Group("/api/v1/links", jwtMW)
	neutron.Get(linkGroup, "", listLinksHandler(linkSvc),
		neutron.WithTags("links"), neutron.WithSummary("List tracked links"))
	neutron.Post(linkGroup, "", createLinkHandler(linkSvc),
		neutron.WithTags("links"), neutron.WithSummary("Create tracked link"))
	// Public redirect (no auth)
	r.HandleFunc("GET /l/{slug}", linkSvc.ClickHandler())
	// Tracking pixel (no auth)
	r.HandleFunc("GET /t/pixel.gif", linkSvc.PixelHandler())

	// Tracker scripts (served as static JS)
	r.HandleFunc("GET /t/observe.js", serveTracker)
	r.HandleFunc("GET /t/observe-errors.js", serveErrorTracker)
	r.HandleFunc("GET /t/observe-replay.js", serveReplayTracker)
	r.HandleFunc("GET /t/observe-feedback.js", serveFeedbackWidget)

	// Dashboard UI (embedded static files)
	uiSub, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		logger.Error("failed to load ui assets", "err", err)
		os.Exit(1)
	}
	// --- Health check ---
	r.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := db.SQL().Exec(req.Context(), "SELECT 1")
		if err != nil {
			w.WriteHeader(503)
			fmt.Fprintf(w, `{"status":"error","error":%q}`, err.Error())
			return
		}
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	r.Handle("GET /assets/", http.FileServer(http.FS(uiSub)))

	// --- Public share dashboard ---
	r.HandleFunc("GET /share/{token}", shareViewHandler(shareSvc, uiSub))

	// --- Performance issue detectors API ---
	// Routes live in perf_handlers.go; only the registration call is here so
	// merge-conflict surface with W4.A (heatmaps) and W4.B (replay→issue) is
	// a single line addition rather than a scattered diff.
	RegisterPerformanceRoutes(r.Group("/api/v1/performance", jwtMW), traceQuery)

	// --- Boards API ---
	// Multi-site aggregate dashboards (W2.B). Routes + handlers live in
	// boards_handlers.go to keep merge-conflict surface with W2.A (funnels)
	// and W2.C (attribution) at zero.
	RegisterBoardsRoutes(r, jwtMW, requireEditor, boardSvc)

	// --- Attribution API ---
	// Routes live in attribution_handlers.go (W2.C). Single-line wiring keeps
	// the merge surface minimal against W2.A (funnels) and W2.B (boards).
	RegisterAttributionRoutes(r.Group("", jwtMW), query.NewAttributionService(db))

	// --- Trace funnels API ---
	// Routes live in funnel_handlers.go (W2.A). Same single-line wiring
	// convention as RegisterPerformanceRoutes / RegisterAttributionRoutes
	// above to keep this section diff-stable across waves.
	RegisterFunnelRoutes(r.Group("/api/v1/tracing/funnel", jwtMW), traceQuery, viewSvc, requireEditor)

	// --- Metrics API ---
	// W3.A Phase 1: OTLP metrics ingest at /v1/metrics + list/query at
	// /api/v1/metrics/*. Routes live in metrics_handlers.go to keep the
	// main.go diff to a single line (matches the boards / attribution /
	// funnels convention above). Placed at the END of route registrations
	// to minimize merge conflict surface with parallel waves.
	RegisterMetricsRoutes(r, jwtMW, apiKeyMW, metricsSvc)

	// --- Persons API ---
	// C2 Wave 4: aggregate over events.distinct_id. Read-only; no editor
	// gating needed. Single-line wiring matches the metrics / boards
	// convention above.
	RegisterPersonsRoutes(r, jwtMW, personsSvc)

	// --- Cohorts API ---
	// C2 Wave 4: behavioural grouping. Owns its own table (migration 023)
	// and exposes MembersForFilter to the stats service for ?cohort_id=
	// filtering across every analytics chart.
	RegisterCohortsRoutes(r, jwtMW, requireEditor, cohortsSvc)

	// SPA catch-all: serve index.html for all non-API, non-asset GET requests.
	// This must be registered last so API routes take precedence.
	indexHTML, _ := fs.ReadFile(uiSub, "index.html")
	r.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		// Let the file server handle real asset files
		if strings.HasPrefix(req.URL.Path, "/assets/") || strings.HasPrefix(req.URL.Path, "/api/") ||
			strings.HasPrefix(req.URL.Path, "/v1/") || strings.HasPrefix(req.URL.Path, "/t/") ||
			strings.HasPrefix(req.URL.Path, "/l/") || req.URL.Path == "/healthz" {
			http.NotFound(w, req)
			return
		}
		if indexHTML == nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	// buf + scheduler are started by the lifecycle hooks registered on
	// the neutron app above. errorBuf has no lifecycle hook so we start
	// it directly. (Calling .Start twice on scheduler creates a second
	// goroutine set whose cancel-func gets overwritten, hanging Stop —
	// learnt the hard way during T043.)
	errorBuf.Start()

	// Alert check loop (every 60s)
	go func() {
		time.Sleep(30 * time.Second)
		for {
			if err := alertSvc.CheckRules(context.Background()); err != nil {
				logger.Error("alert check failed", "err", err)
			}
			time.Sleep(60 * time.Second)
		}
	}()

	// Uptime monitor check loop (every 30s)
	go func() {
		time.Sleep(15 * time.Second)
		for {
			uptimeSvc.RunChecks(context.Background())
			time.Sleep(30 * time.Second)
		}
	}()

	// Missed-cron detection loop (every 60s). Without this, CheckMissed was
	// dead code and a silently-dead cron was never alerted. Each missing cron
	// opens a deduped incident (keyed on cron:<id>); a subsequent check-in
	// that brings it back inside its grace period auto-resolves the incident.
	go func() {
		time.Sleep(45 * time.Second)
		for {
			ctx := context.Background()
			missed, err := cronSvc.CheckMissed(ctx)
			if err != nil {
				logger.Error("cron missed-check failed", "err", err)
			}
			missingNow := make(map[string]bool, len(missed))
			for _, c := range missed {
				ruleKey := "cron:" + c.CronID
				missingNow[ruleKey] = true
				if active, _ := incidentSvc.ActiveByRule(ctx, ruleKey); len(active) > 0 {
					continue // already open — dedup
				}
				_, err := incidentSvc.Create(ctx, incidents.CreateInput{
					SiteID:      c.SiteID,
					Title:       fmt.Sprintf("Cron missed: %s", c.Name),
					Description: fmt.Sprintf("cron %q (slug %q) has not checked in within its %ds grace period", c.Name, c.Slug, c.GracePeriod),
					Severity:    "warning",
					Source:      incidents.SourceCron,
					RuleID:      ruleKey,
				}, "cron")
				if err != nil {
					logger.Warn("cron incident auto-create failed", "cron", c.CronID, "err", err)
				}
			}
			time.Sleep(60 * time.Second)
		}
	}()

	// Scheduled export runner (every minute)
	go func() {
		time.Sleep(10 * time.Second)
		for {
			scheduledExportSvc.RunDue(context.Background(), time.Now())
			time.Sleep(time.Minute)
		}
	}()

	// Report scheduler (every hour)
	go func() {
		time.Sleep(45 * time.Second)
		smtpHost := os.Getenv("OBSERVE_SMTP_HOST")
		smtpPort := os.Getenv("OBSERVE_SMTP_PORT")
		smtpUser := os.Getenv("OBSERVE_SMTP_USER")
		smtpPass := os.Getenv("OBSERVE_SMTP_PASS")
		fromEmail := os.Getenv("OBSERVE_SMTP_FROM")
		for {
			reportSvc.RunScheduled(context.Background(), smtpHost, smtpPort, smtpUser, smtpPass, fromEmail)
			time.Sleep(1 * time.Hour)
		}
	}()

	// PID file lets `teploy-observe upgrade` find this process and send SIGTERM.
	// Env snapshot lets `teploy-observe upgrade` re-launch the new binary with
	// the same OBSERVE_* configuration even when the upgrader was invoked
	// from a different shell. Both are best-effort.
	dataDir := os.Getenv("OBSERVE_DATA_DIR")
	if dataDir != "" {
		if err := upgrade.WritePID(dataDir); err != nil {
			logger.Warn("could not write pid file", "err", err, "dir", dataDir)
		} else {
			logger.Info("pid file written", "path", upgrade.PIDFile(dataDir))
		}
		if err := upgrade.WriteEnv(dataDir); err != nil {
			logger.Warn("could not write env snapshot", "err", err)
		}
	}

	if os.Getenv("OBSERVE_LOG_ROUTES") == "1" {
		r.PrintRoutes()
	}

	logger.Info("starting observe", "addr", cfg.Addr)
	// app.Run owns SIGTERM + http drain + lifecycle.stop (which Stops the
	// ingest buffer + scheduler we registered as hooks). It blocks until
	// shutdown completes.
	runErr := app.Run(cfg.Addr)

	// Supplemental shutdown for state Neutron's lifecycle doesn't manage:
	// errorBuf flush, dogfood self-monitoring, db.Close, PID file.
	logger.Info("running supplemental shutdown")
	errorBuf.Stop()
	_ = self.Close()
	db.Close()
	if dataDir != "" {
		_ = upgrade.RemovePID(dataDir)
	}
	logger.Info("shutdown complete")

	if runErr != nil {
		logger.Error("server error", "err", runErr)
		os.Exit(1)
	}
}

// ─── Subcommands ────────────────────────────────────────────────────────────

func printHelp() {
	// Write directly to stdout to avoid `go vet` parsing the `date +%F`
	// example below as a printf directive.
	os.Stdout.WriteString(`Observe — self-hosted analytics, errors, logs, traces, replays.

Usage:
  teploy-observe              Start the HTTP server (default).
  teploy-observe backup       Stream a tar archive of all tables to stdout.
  teploy-observe restore      Read a tar archive from stdin and insert into tables.
  teploy-observe upgrade      Drain ingest, swap binary, resume — zero event loss.
  teploy-observe reindex      Rebuild the FTS index from error_events.
  teploy-observe version      Print the teploy-observe version.
  teploy-observe help         Show this message.

Upgrade flags:
  --target <path>      Path to the new binary (default: ./teploy-observe-new)
  --data-dir <path>    Override OBSERVE_DATA_DIR for PID lookup

Reindex flags:
  --site <id>          Only reindex one site (default: all sites)
  --batch <n>          Rows per scan batch (default: 1000)
  --dry-run            Scan but do not write to FTS (verifies source rows)

Env vars:
  OBSERVE_ADDR                 HTTP bind address (default :3000)
  OBSERVE_NUCLEUS_URL          Nucleus/Postgres DSN
  OBSERVE_DATA_DIR             Data directory (PID file, WAL queue)
  OBSERVE_JWT_SECRET           JWT signing secret (required in prod)
  OBSERVE_SECRET_KEY           Master key for encrypting stored secrets
                               (LLM API key, S3/R2 credentials) at rest;
                               required to configure those features
  OBSERVE_ADMIN_USER           First-boot admin username (default: admin)
  OBSERVE_ADMIN_PASSWORD       First-boot admin password (default: observe)
  OBSERVE_DEMO_MODE            "true" blocks write ops for public demos
  OBSERVE_SEED_DEMO            "false" skips first-boot demo seeding

Example backup:
  teploy-observe backup | zstd > teploy-observe-$(date +%F).tar.zst
  zstdcat teploy-observe-2026-04-17.tar.zst | teploy-observe restore

Example upgrade:
  cp /tmp/teploy-observe-new ./teploy-observe-new
  ./teploy-observe upgrade --target ./teploy-observe-new
`)
}

func connectForCLI(cfg config.Config, logger *slog.Logger) *nucleus.Client {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, cfg.NucleusURL)
	if err != nil {
		logger.Error("failed to connect to nucleus", "err", err)
		os.Exit(1)
	}
	return db
}

func runBackup(cfg config.Config, logger *slog.Logger) {
	db := connectForCLI(cfg, logger)
	defer db.Close()

	ctx := context.Background()
	// Tar goes to stdout; per-table errors to stderr so the tar stream stays pristine.
	if err := backup.DumpWithLog(ctx, db, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "backup completed with errors: %v\n", err)
		os.Exit(2)
	}
}

func runRestore(cfg config.Config, logger *slog.Logger) {
	db := connectForCLI(cfg, logger)
	defer db.Close()

	ctx := context.Background()
	if err := backup.Restore(ctx, db, os.Stdin); err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(1)
	}
	logger.Info("restore complete")
}

// runUpgrade orchestrates a zero-downtime binary swap. See
// internal/upgrade for the building blocks. Flow:
//   1. Parse --target and --data-dir flags.
//   2. Pre-flight the target binary (exists, executable, version >= current).
//   3. SIGTERM the running observe via PID file.
//   4. Wait up to 30s for it to exit (graceful shutdown flushes ingest +
//      WAL queue persists buffered events on disk).
//   5. Atomic swap: rename current → .prev, rename target → current.
//   6. Spawn the new binary detached with the same env.
//   7. Wait up to 60s for /healthz to return 200.
//   8. On any failure after the swap, restore the previous binary.
func runUpgrade(cfg config.Config, logger *slog.Logger, args []string) {
	target := "./teploy-observe-new"
	dataDir := os.Getenv("OBSERVE_DATA_DIR")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--target":
			if i+1 >= len(args) {
				logger.Error("upgrade: --target requires a value")
				os.Exit(2)
			}
			target = args[i+1]
			i++
		case "--data-dir":
			if i+1 >= len(args) {
				logger.Error("upgrade: --data-dir requires a value")
				os.Exit(2)
			}
			dataDir = args[i+1]
			i++
		case "-h", "--help":
			fmt.Println("usage: teploy-observe upgrade [--target PATH] [--data-dir PATH]")
			return
		default:
			logger.Error("upgrade: unknown argument", "arg", args[i])
			os.Exit(2)
		}
	}
	if dataDir == "" {
		logger.Error("upgrade: OBSERVE_DATA_DIR or --data-dir required (need to find PID file)")
		os.Exit(2)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		logger.Error("upgrade: resolve target path", "err", err)
		os.Exit(1)
	}

	// 1. Pre-flight the target.
	logger.Info("upgrade: pre-flighting target", "target", absTarget)
	currentVersionStr := "teploy-observe " + version
	targetVersion, err := upgrade.PreflightTarget(absTarget, currentVersionStr)
	if err != nil {
		logger.Error("upgrade: pre-flight failed", "err", err)
		os.Exit(1)
	}
	logger.Info("upgrade: target ok", "target_version", fmt.Sprintf("%d.%d.%d", targetVersion.Major, targetVersion.Minor, targetVersion.Patch))

	// 2. Locate current binary path and the running PID.
	currentBinary, err := os.Executable()
	if err != nil {
		logger.Error("upgrade: cannot resolve current executable", "err", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(currentBinary); err == nil {
		currentBinary = resolved
	}
	pid, err := upgrade.ReadPID(dataDir)
	if err != nil {
		logger.Error("upgrade: cannot read pid file (is observe running?)", "err", err, "path", upgrade.PIDFile(dataDir))
		os.Exit(1)
	}
	logger.Info("upgrade: found running observe", "pid", pid, "binary", currentBinary)

	// 3+4. SIGTERM and wait for exit.
	if err := upgrade.SignalProcess(pid, syscall.SIGTERM); err != nil {
		logger.Error("upgrade: send sigterm", "err", err, "pid", pid)
		os.Exit(1)
	}
	logger.Info("upgrade: sent sigterm, waiting for exit (up to 30s)")
	ctx := context.Background()
	if err := upgrade.WaitForShutdown(ctx, pid, dataDir, 30*time.Second); err != nil {
		logger.Error("upgrade: old process did not exit in time", "err", err)
		os.Exit(1)
	}
	logger.Info("upgrade: old process exited, ingest WAL queue is on disk")

	// 5. Atomic swap (rename within the same directory is atomic on the
	// same filesystem; cross-FS would error and we'd refuse below).
	prevBackup, err := upgrade.SwapBinary(currentBinary, absTarget)
	if err != nil {
		logger.Error("upgrade: binary swap failed", "err", err)
		os.Exit(1)
	}
	logger.Info("upgrade: binary swapped", "current", currentBinary, "backup", prevBackup)

	// 6. Spawn the new binary detached. Prefer the OBSERVE_* env snapshot
	// the previous instance wrote (so the upgrader can be invoked from a
	// different shell and the new binary still gets OBSERVE_NUCLEUS_URL
	// etc.); fall back to the upgrader's own env when no snapshot exists.
	spawnEnv := os.Environ()
	if snap, err := upgrade.ReadEnv(dataDir); err == nil && len(snap) > 0 {
		// Merge: start from non-OBSERVE_ vars in current env, then layer
		// the snapshot on top so its OBSERVE_* values win.
		merged := make([]string, 0, len(snap)+len(spawnEnv))
		for _, kv := range spawnEnv {
			if !strings.HasPrefix(kv, "OBSERVE_") {
				merged = append(merged, kv)
			}
		}
		merged = append(merged, snap...)
		spawnEnv = merged
		logger.Info("upgrade: spawning with env snapshot", "vars", len(snap))
	}
	cmd := exec.Command(currentBinary)
	cmd.Env = spawnEnv
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = newProcAttrDetached()
	if err := cmd.Start(); err != nil {
		logger.Error("upgrade: spawn new binary failed, rolling back", "err", err)
		if rerr := upgrade.RestoreBinary(currentBinary, prevBackup); rerr != nil {
			logger.Error("upgrade: ROLLBACK FAILED — manual intervention required", "err", rerr)
		}
		os.Exit(1)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	logger.Info("upgrade: new binary spawned, waiting for healthz (up to 60s)")

	// 7. Health check. Prefer the OBSERVE_ADDR from the env snapshot
	// (what the running observe is actually listening on) over the
	// upgrader's own cfg, which falls back to :3000 by default.
	healthAddr := cfg.Addr
	if snap, err := upgrade.ReadEnv(dataDir); err == nil {
		for _, kv := range snap {
			if strings.HasPrefix(kv, "OBSERVE_ADDR=") {
				healthAddr = strings.TrimPrefix(kv, "OBSERVE_ADDR=")
				break
			}
		}
	}
	healthURL := upgrade.HealthURL(healthAddr)
	logger.Info("upgrade: probing healthz", "url", healthURL)
	if err := upgrade.WaitForHealthz(ctx, healthURL, 60*time.Second); err != nil {
		logger.Error("upgrade: new binary failed health check, rolling back", "err", err, "url", healthURL)
		// Try to terminate the unhealthy new process so the rollback can boot.
		if newPid, perr := upgrade.ReadPID(dataDir); perr == nil && newPid != pid {
			_ = upgrade.SignalProcess(newPid, syscall.SIGTERM)
			_ = upgrade.WaitForExit(ctx, newPid, 15*time.Second)
		}
		if rerr := upgrade.RestoreBinary(currentBinary, prevBackup); rerr != nil {
			logger.Error("upgrade: ROLLBACK FAILED — manual intervention required", "err", rerr, "backup", prevBackup)
			os.Exit(1)
		}
		logger.Info("upgrade: previous binary restored, you must restart observe manually")
		os.Exit(1)
	}

	// 8. Success — clean up the backup.
	_ = os.Remove(prevBackup)
	logger.Info("upgrade: complete", "version", fmt.Sprintf("%d.%d.%d", targetVersion.Major, targetVersion.Minor, targetVersion.Patch))
}

// runReindex rebuilds the FTS index from rows in error_events. Useful when
// FTS files have been wiped or when a schema change has invalidated the
// existing index. The same code path lives in the IngestErrorEvent wrapper,
// so re-running this is identical to having ingested every row through the
// live HTTP path.
//
// Idempotent: re-indexing the same row writes a new mapping that supersedes
// the previous one. For a fully clean rebuild, drop the FTS files first.
func runReindex(cfg config.Config, logger *slog.Logger, args []string) {
	siteID := ""
	batch := 1000
	dryRun := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--site":
			if i+1 >= len(args) {
				logger.Error("reindex: --site requires a value")
				os.Exit(2)
			}
			siteID = args[i+1]
			i++
		case "--batch":
			if i+1 >= len(args) {
				logger.Error("reindex: --batch requires a value")
				os.Exit(2)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				logger.Error("reindex: --batch must be a positive integer", "got", args[i+1])
				os.Exit(2)
			}
			batch = n
			i++
		case "--dry-run":
			dryRun = true
		case "-h", "--help":
			fmt.Println("Usage: observe reindex [--site <id>] [--batch <n>] [--dry-run]")
			return
		default:
			logger.Error("reindex: unknown flag", "arg", args[i])
			os.Exit(2)
		}
	}

	db := connectForCLI(cfg, logger)
	defer db.Close()

	searchSvc := obserrors.NewSearchService(db)

	logger.Info("reindex: start",
		"site", orAll(siteID), "batch", batch, "dry_run", dryRun)

	ctx := context.Background()
	progress, err := searchSvc.ReindexAll(ctx, siteID, batch, dryRun, int64(batch),
		func(p obserrors.ReindexProgress) {
			logger.Info("reindex: progress", "scanned", p.Scanned, "indexed", p.Indexed)
		})
	if err != nil {
		logger.Error("reindex: failed",
			"err", err, "scanned", progress.Scanned, "indexed", progress.Indexed)
		os.Exit(1)
	}
	logger.Info("reindex: complete",
		"scanned", progress.Scanned, "indexed", progress.Indexed, "dry_run", dryRun)
}

func orAll(s string) string {
	if s == "" {
		return "<all>"
	}
	return s
}

// --- Auth handlers ---

type loginInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

func loginHandler(authSvc *auth.AuthService) neutron.HandlerFunc[loginInput, loginResponse] {
	return func(ctx context.Context, input loginInput) (loginResponse, error) {
		if input.Username == "" || input.Password == "" {
			return loginResponse{}, neutron.ErrBadRequest("username and password required")
		}
		token, err := authSvc.Login(ctx, input.Username, input.Password)
		if err != nil {
			return loginResponse{}, neutron.ErrUnauthorized("invalid credentials")
		}
		return loginResponse{Token: token}, nil
	}
}

type changePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func changePasswordHandler(authSvc *auth.AuthService) neutron.HandlerFunc[changePasswordInput, neutron.Empty] {
	return func(ctx context.Context, input changePasswordInput) (neutron.Empty, error) {
		claims, _ := neutronauth.ClaimsFromContext(ctx)
		userID, _ := claims["sub"].(string)
		if userID == "" {
			return neutron.Empty{}, neutron.ErrUnauthorized("invalid token")
		}
		if input.CurrentPassword == "" || input.NewPassword == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("current_password and new_password required")
		}
		if err := authSvc.ChangePassword(ctx, userID, input.CurrentPassword, input.NewPassword); err != nil {
			return neutron.Empty{}, neutron.ErrBadRequest(err.Error())
		}
		return neutron.Empty{}, nil
	}
}

// emptyOnNil coerces a nil slice to an empty slice so list endpoints
// always serialize as `[]` rather than `null`. The Go JSON encoder
// renders a nil slice as null, which breaks TS clients that expect
// `T[]`. See docs/api-shape-convention.md for the API contract.
func emptyOnNil[T any](s []T, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	if s == nil {
		return []T{}, nil
	}
	return s, nil
}

// --- Site handlers ---

func listSitesHandler(siteSvc *sites.SiteService) neutron.HandlerFunc[neutron.Empty, []sites.Site] {
	return func(ctx context.Context, _ neutron.Empty) ([]sites.Site, error) {
		list, err := siteSvc.List(ctx)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []sites.Site{}
		}
		return list, nil
	}
}

type createSiteInput struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
}

func createSiteHandler(siteSvc *sites.SiteService) neutron.HandlerFunc[createSiteInput, sites.Site] {
	return func(ctx context.Context, input createSiteInput) (sites.Site, error) {
		if input.Domain == "" {
			return sites.Site{}, neutron.ErrBadRequest("domain is required")
		}
		if input.Name == "" {
			input.Name = input.Domain
		}
		return siteSvc.Create(ctx, input.Domain, input.Name)
	}
}

type deleteSiteInput struct {
	SiteID string `path:"site_id"`
}

type setSiteRatelimitInput struct {
	SiteID        string `path:"site_id"`
	RatePerSecond int    `json:"rate_per_second"`
}

type setSiteRatelimitResult struct {
	SiteID        string `json:"site_id"`
	RatePerSecond int    `json:"rate_per_second"`
}

func setSiteRatelimitHandler(siteSvc *sites.SiteService, rl *ingest.RateLimiter) neutron.HandlerFunc[setSiteRatelimitInput, setSiteRatelimitResult] {
	return func(ctx context.Context, input setSiteRatelimitInput) (setSiteRatelimitResult, error) {
		if input.SiteID == "" {
			return setSiteRatelimitResult{}, neutron.ErrBadRequest("site_id is required")
		}
		if input.RatePerSecond < 0 {
			return setSiteRatelimitResult{}, neutron.ErrBadRequest("rate_per_second must be >= 0")
		}
		if err := siteSvc.SetRatelimit(ctx, input.SiteID, input.RatePerSecond); err != nil {
			return setSiteRatelimitResult{}, err
		}
		rl.SetSiteCap(input.SiteID, input.RatePerSecond)
		return setSiteRatelimitResult{SiteID: input.SiteID, RatePerSecond: input.RatePerSecond}, nil
	}
}

func deleteSiteHandler(siteSvc *sites.SiteService) neutron.HandlerFunc[deleteSiteInput, neutron.Empty] {
	return func(ctx context.Context, input deleteSiteInput) (neutron.Empty, error) {
		if input.SiteID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("site_id is required")
		}
		err := siteSvc.Delete(ctx, input.SiteID)
		if err != nil {
			return neutron.Empty{}, err
		}
		return neutron.Empty{}, nil
	}
}

// --- API key handlers ---

type createAPIKeyInput struct {
	SiteID string `path:"site_id"`
	Label  string `json:"label"`
}

type createAPIKeyResponse struct {
	Key  string       `json:"key"`
	Info auth.APIKeyInfo `json:"info"`
}

func createAPIKeyHandler(authSvc *auth.AuthService) neutron.HandlerFunc[createAPIKeyInput, createAPIKeyResponse] {
	return func(ctx context.Context, input createAPIKeyInput) (createAPIKeyResponse, error) {
		if input.SiteID == "" {
			return createAPIKeyResponse{}, neutron.ErrBadRequest("site_id is required")
		}
		if input.Label == "" {
			input.Label = "default"
		}
		key, info, err := authSvc.CreateAPIKey(ctx, input.SiteID, input.Label)
		if err != nil {
			return createAPIKeyResponse{}, err
		}
		return createAPIKeyResponse{Key: key, Info: info}, nil
	}
}

type listAPIKeysInput struct {
	SiteID string `path:"site_id"`
}

func listAPIKeysHandler(authSvc *auth.AuthService) neutron.HandlerFunc[listAPIKeysInput, []auth.APIKeyInfo] {
	return func(ctx context.Context, input listAPIKeysInput) ([]auth.APIKeyInfo, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return emptyOnNil(authSvc.ListAPIKeys(ctx, input.SiteID))
	}
}

type revokeAPIKeyInput struct {
	KeyID string `path:"key_id"`
}

func revokeAPIKeyHandler(authSvc *auth.AuthService) neutron.HandlerFunc[revokeAPIKeyInput, neutron.Empty] {
	return func(ctx context.Context, input revokeAPIKeyInput) (neutron.Empty, error) {
		if input.KeyID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("key_id required")
		}
		return neutron.Empty{}, authSvc.RevokeAPIKey(ctx, input.KeyID)
	}
}

// serveTracker serves the lightweight analytics tracker script.
func serveTracker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(trackerScript)
}

//go:embed tracker/observe.js
var trackerScript []byte

//go:embed tracker/observe-errors.js
var errorTrackerScript []byte

//go:embed tracker/observe-replay.js
var replayTrackerScript []byte

func serveErrorTracker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(errorTrackerScript)
}

func serveReplayTracker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(replayTrackerScript)
}

//go:embed tracker/observe-feedback.js
var feedbackWidgetScript []byte

func serveFeedbackWidget(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(feedbackWidgetScript)
}

// --- Share handlers ---

type createShareInput struct {
	SiteID string `path:"site_id"`
}

func createShareHandler(shareSvc *share.ShareService, siteSvc *sites.SiteService) neutron.HandlerFunc[createShareInput, share.ShareLink] {
	return func(ctx context.Context, input createShareInput) (share.ShareLink, error) {
		if input.SiteID == "" {
			return share.ShareLink{}, neutron.ErrBadRequest("site_id is required")
		}
		// Don't mint share links for nonexistent sites (avoids dangling links).
		if s, err := siteSvc.Get(ctx, input.SiteID); err != nil || s.SiteID == "" {
			return share.ShareLink{}, neutron.ErrNotFound("site not found")
		}
		return shareSvc.Create(ctx, input.SiteID)
	}
}

type listShareInput struct {
	SiteID string `path:"site_id"`
}

func listShareHandler(shareSvc *share.ShareService) neutron.HandlerFunc[listShareInput, []share.ShareLink] {
	return func(ctx context.Context, input listShareInput) ([]share.ShareLink, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id is required")
		}
		links, err := shareSvc.List(ctx, input.SiteID)
		if err != nil {
			return nil, err
		}
		if links == nil {
			links = []share.ShareLink{}
		}
		return links, nil
	}
}

type revokeShareInput struct {
	Token string `path:"token"`
}

func revokeShareHandler(shareSvc *share.ShareService) neutron.HandlerFunc[revokeShareInput, neutron.Empty] {
	return func(ctx context.Context, input revokeShareInput) (neutron.Empty, error) {
		if input.Token == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("token is required")
		}
		if err := shareSvc.Revoke(ctx, input.Token); err != nil {
			return neutron.Empty{}, err
		}
		return neutron.Empty{}, nil
	}
}

// --- Error ingestion handler ---

func errorIngestHandler(buf *obserrors.ErrorBuffer) neutron.HandlerFunc[obserrors.ErrorInput, obserrors.ErrorResponse] {
	return func(ctx context.Context, input obserrors.ErrorInput) (obserrors.ErrorResponse, error) {
		siteID := input.SiteID
		if siteID == "" {
			siteID = ingest.SiteIDFromContext(ctx)
		}
		if siteID == "" {
			return obserrors.ErrorResponse{}, neutron.ErrBadRequest("missing site_id")
		}
		if !buf.Push(siteID, input) {
			return obserrors.ErrorResponse{}, neutron.ErrRateLimited("error buffer full")
		}
		return obserrors.ErrorResponse{OK: true}, nil
	}
}

// --- Issue management handlers ---

type listIssuesInput struct {
	SiteID string `query:"site_id"`
	Status string `query:"status"`
	Limit  int    `query:"limit"`
	Offset int    `query:"offset"`
}

func listIssuesHandler(svc *obserrors.IssueService) neutron.HandlerFunc[listIssuesInput, []obserrors.Issue] {
	return func(ctx context.Context, input listIssuesInput) ([]obserrors.Issue, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		issues, err := svc.ListIssues(ctx, input.SiteID, input.Status, input.Limit, input.Offset)
		if err != nil {
			return nil, err
		}
		if issues == nil {
			issues = []obserrors.Issue{}
		}
		return issues, nil
	}
}

type getIssueInput struct {
	IssueID string `path:"issue_id"`
	SiteID  string `query:"site_id"`
}

func getIssueHandler(svc *obserrors.IssueService) neutron.HandlerFunc[getIssueInput, obserrors.Issue] {
	return func(ctx context.Context, input getIssueInput) (obserrors.Issue, error) {
		if input.SiteID == "" || input.IssueID == "" {
			return obserrors.Issue{}, neutron.ErrBadRequest("site_id and issue_id required")
		}
		issue, err := svc.GetIssue(ctx, input.IssueID, input.SiteID)
		if err != nil {
			return obserrors.Issue{}, err
		}
		if issue == nil {
			return obserrors.Issue{}, neutron.ErrNotFound("issue not found")
		}
		return *issue, nil
	}
}

type updateStatusInput struct {
	IssueID string `path:"issue_id"`
	SiteID  string `json:"site_id"`
	Status  string `json:"status"`
}

func updateIssueStatusHandler(svc *obserrors.IssueService) neutron.HandlerFunc[updateStatusInput, neutron.Empty] {
	return func(ctx context.Context, input updateStatusInput) (neutron.Empty, error) {
		if input.IssueID == "" || input.SiteID == "" || input.Status == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("issue_id, site_id, and status required")
		}
		if input.Status != "open" && input.Status != "resolved" && input.Status != "ignored" {
			return neutron.Empty{}, neutron.ErrBadRequest("status must be open, resolved, or ignored")
		}
		if err := svc.UpdateStatus(ctx, input.IssueID, input.SiteID, input.Status); err != nil {
			return neutron.Empty{}, err
		}
		return neutron.Empty{}, nil
	}
}

type issueEventsInput struct {
	IssueID string `path:"issue_id"`
	SiteID  string `query:"site_id"`
	Limit   int    `query:"limit"`
}

func issueEventsHandler(svc *obserrors.IssueService, srcmap *sourcemaps.SourceMapService) neutron.HandlerFunc[issueEventsInput, []obserrors.ErrorEvent] {
	return func(ctx context.Context, input issueEventsInput) ([]obserrors.ErrorEvent, error) {
		if input.SiteID == "" || input.IssueID == "" {
			return nil, neutron.ErrBadRequest("site_id and issue_id required")
		}
		events, err := svc.LatestEvents(ctx, input.IssueID, input.SiteID, input.Limit)
		if err != nil {
			return nil, err
		}
		if events == nil {
			events = []obserrors.ErrorEvent{}
		}
		// Symbolicate stack traces in place when a source map is available for the release.
		if srcmap != nil {
			for i := range events {
				if events[i].StackTrace == "" || events[i].ReleaseTag == "" {
					continue
				}
				resolved, err := srcmap.ResolveStackTrace(ctx, input.SiteID, events[i].ReleaseTag, events[i].StackTrace)
				if err == nil && resolved != "" {
					events[i].StackTrace = resolved
				}
			}
		}
		return events, nil
	}
}

type issueSessionInput struct {
	IssueID string `path:"issue_id"`
	SiteID  string `query:"site_id"`
}

type issueSessionResponse struct {
	SessionID string              `json:"session_id"`
	Events    []query.SessionEvent `json:"events"`
}

func issueSessionHandler(issueSvc *obserrors.IssueService, statsSvc *query.StatsService) neutron.HandlerFunc[issueSessionInput, issueSessionResponse] {
	return func(ctx context.Context, input issueSessionInput) (issueSessionResponse, error) {
		if input.SiteID == "" || input.IssueID == "" {
			return issueSessionResponse{}, neutron.ErrBadRequest("site_id and issue_id required")
		}
		// Get the latest error event for this issue to find its session_id
		events, err := issueSvc.LatestEvents(ctx, input.IssueID, input.SiteID, 1)
		if err != nil || len(events) == 0 {
			return issueSessionResponse{}, neutron.ErrNotFound("no error events for this issue")
		}
		sessionID := events[0].SessionID
		if sessionID == "" {
			return issueSessionResponse{}, neutron.ErrNotFound("error has no associated session")
		}
		// Fetch the analytics session timeline
		sessEvents, err := statsSvc.SessionDetail(ctx, sessionID, input.SiteID)
		if err != nil {
			return issueSessionResponse{}, err
		}
		return issueSessionResponse{SessionID: sessionID, Events: sessEvents}, nil
	}
}

// --- LLM handlers ---

func llmIngestHandler(svc *llm.LLMService) neutron.HandlerFunc[llm.LLMInput, llm.LLMResponse] {
	return func(ctx context.Context, input llm.LLMInput) (llm.LLMResponse, error) {
		if input.SiteID == "" { input.SiteID = ingest.SiteIDFromContext(ctx) }
		return svc.Ingest(ctx, input)
	}
}

type llmStatsInput struct { SiteID string `query:"site_id"`; From string `query:"from"`; To string `query:"to"` }

func llmStatsHandler(svc *llm.LLMService) neutron.HandlerFunc[llmStatsInput, llm.LLMStats] {
	return func(ctx context.Context, input llmStatsInput) (llm.LLMStats, error) {
		from, to := parseTimeRange(input.From, input.To)
		s, err := svc.Stats(ctx, input.SiteID, from, to)
		if err != nil { return llm.LLMStats{}, err }
		return *s, nil
	}
}

func llmModelsHandler(svc *llm.LLMService) neutron.HandlerFunc[llmStatsInput, []llm.ModelStats] {
	return func(ctx context.Context, input llmStatsInput) ([]llm.ModelStats, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.ModelBreakdown(ctx, input.SiteID, from, to))
	}
}

type llmTracesInput struct { SiteID string `query:"site_id"`; Limit int `query:"limit"` }

func llmTracesHandler(svc *llm.LLMService) neutron.HandlerFunc[llmTracesInput, []llm.LLMTrace] {
	return func(ctx context.Context, input llmTracesInput) ([]llm.LLMTrace, error) {
		return emptyOnNil(svc.RecentTraces(ctx, input.SiteID, input.Limit))
	}
}

// --- Infra handlers ---

func infraReportHandler(svc *infra.InfraService) neutron.HandlerFunc[infra.MetricInput, map[string]string] {
	return func(ctx context.Context, input infra.MetricInput) (map[string]string, error) {
		if input.SiteID == "" { input.SiteID = ingest.SiteIDFromContext(ctx) }
		err := svc.Report(ctx, input)
		if err != nil { return nil, err }
		return map[string]string{"ok": "true"}, nil
	}
}

type infraHostsInput struct { SiteID string `query:"site_id"` }

func infraHostsHandler(svc *infra.InfraService) neutron.HandlerFunc[infraHostsInput, []infra.HostSummary] {
	return func(ctx context.Context, input infraHostsInput) ([]infra.HostSummary, error) {
		return emptyOnNil(svc.ListHosts(ctx, input.SiteID))
	}
}

type infraHistoryInput struct {
	SiteID   string `query:"site_id"`
	Hostname string `path:"hostname"`
	From     string `query:"from"`
	To       string `query:"to"`
}

func infraHistoryHandler(svc *infra.InfraService) neutron.HandlerFunc[infraHistoryInput, []infra.HostMetric] {
	return func(ctx context.Context, input infraHistoryInput) ([]infra.HostMetric, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.HostHistory(ctx, input.SiteID, input.Hostname, from, to, 100))
	}
}

// --- Pipeline handlers ---

type listPipelinesInput struct { SiteID string `query:"site_id"` }

func listPipelinesHandler(svc *logs.PipelineService) neutron.HandlerFunc[listPipelinesInput, []logs.Pipeline] {
	return func(ctx context.Context, input listPipelinesInput) ([]logs.Pipeline, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createPipelineInput struct {
	SiteID   string `json:"site_id"`
	Name     string `json:"name"`
	Rules    string `json:"rules"`
	Priority int    `json:"priority"`
}

func createPipelineHandler(svc *logs.PipelineService) neutron.HandlerFunc[createPipelineInput, logs.Pipeline] {
	return func(ctx context.Context, input createPipelineInput) (logs.Pipeline, error) {
		if input.SiteID == "" || input.Name == "" {
			return logs.Pipeline{}, neutron.ErrBadRequest("site_id and name required")
		}
		if err := logs.ValidateRules(input.Rules); err != nil {
			return logs.Pipeline{}, neutron.ErrBadRequest(err.Error())
		}
		p, err := svc.Create(ctx, input.SiteID, input.Name, input.Rules, input.Priority)
		if err != nil { return logs.Pipeline{}, err }
		return *p, nil
	}
}

// --- Group handlers ---

type listGroupsInput struct{ SiteID string `query:"site_id"` }

func listGroupsHandler(svc *groups.GroupService) neutron.HandlerFunc[listGroupsInput, []groups.Group] {
	return func(ctx context.Context, input listGroupsInput) ([]groups.Group, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createGroupInput struct {
	SiteID     string         `json:"site_id"`
	GroupType  string         `json:"group_type"`
	Name       string         `json:"name"`
	Properties map[string]any `json:"properties"`
}

func createGroupHandler(svc *groups.GroupService) neutron.HandlerFunc[createGroupInput, groups.Group] {
	return func(ctx context.Context, input createGroupInput) (groups.Group, error) {
		if input.SiteID == "" || input.Name == "" {
			return groups.Group{}, neutron.ErrBadRequest("site_id and name required")
		}
		g, err := svc.Create(ctx, input.SiteID, input.GroupType, input.Name, input.Properties)
		if err != nil { return groups.Group{}, err }
		return *g, nil
	}
}

type addMemberInput struct {
	GroupID   string `path:"group_id"`
	SiteID    string `json:"site_id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
}

func addGroupMemberHandler(svc *groups.GroupService) neutron.HandlerFunc[addMemberInput, neutron.Empty] {
	return func(ctx context.Context, input addMemberInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.AddMember(ctx, input.SiteID, input.GroupID, input.SessionID, input.UserID)
	}
}

// --- Correlation handler ---

type correlationInput struct {
	SiteID string `query:"site_id"`
	Target string `query:"target"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func correlationHandler(svc *query.StatsService) neutron.HandlerFunc[correlationInput, []query.Correlation] {
	return func(ctx context.Context, input correlationInput) ([]query.Correlation, error) {
		from, to := parseTimeRange(input.From, input.To)
		target := input.Target
		if target == "" { target = "signup" }
		return emptyOnNil(svc.CorrelationAnalysis(ctx, input.SiteID, target, from, to))
	}
}

// --- SSO handlers ---

func ssoMetadataHandler(svc *sso.SSOService, addr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		baseURL := "http://" + r.Host
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(svc.GetSAMLMetadata(baseURL)))
	}
}

type listSSOInput struct{}

func listSSOHandler(svc *sso.SSOService) neutron.HandlerFunc[listSSOInput, []sso.SSOConfig] {
	return func(ctx context.Context, _ listSSOInput) ([]sso.SSOConfig, error) {
		return emptyOnNil(svc.List(ctx))
	}
}

type createSSOInput struct {
	Provider    string `json:"provider"`
	EntityID    string `json:"entity_id"`
	SSOURL      string `json:"sso_url"`
	Certificate string `json:"certificate"`
	AttributeMap string `json:"attribute_map"`
}

func createSSOHandler(svc *sso.SSOService) neutron.HandlerFunc[createSSOInput, sso.SSOConfig] {
	return func(ctx context.Context, input createSSOInput) (sso.SSOConfig, error) {
		if input.EntityID == "" || input.SSOURL == "" {
			return sso.SSOConfig{}, neutron.ErrBadRequest("entity_id and sso_url required")
		}
		c, err := svc.Create(ctx, input.Provider, input.EntityID, input.SSOURL, input.Certificate, input.AttributeMap)
		if err != nil { return sso.SSOConfig{}, err }
		return *c, nil
	}
}

// --- Explorer handlers ---

func explorerQueryHandler(svc *explorer.ExplorerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			SQL string `json:"sql"`
		}
		json.NewDecoder(r.Body).Decode(&input)
		if input.SQL == "" {
			http.Error(w, "sql required", http.StatusBadRequest)
			return
		}
		result, _ := svc.Execute(r.Context(), input.SQL)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func incidentsListHandler(svc *incidents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.URL.Query().Get("site_id")
		if siteID == "" {
			siteID = "default"
		}
		fromStr := r.URL.Query().Get("from")
		toStr := r.URL.Query().Get("to")
		var list []incidents.Incident
		var err error
		if fromStr != "" && toStr != "" {
			var from, to int64
			fmt.Sscanf(fromStr, "%d", &from)
			fmt.Sscanf(toStr, "%d", &to)
			list, err = svc.InRange(r.Context(), siteID, from, to)
		} else {
			list, err = svc.Active(r.Context(), siteID)
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		if list == nil {
			list = []incidents.Incident{}
		}
		json.NewEncoder(w).Encode(list)
	}
}

func incidentsCreateHandler(svc *incidents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input incidents.CreateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		out, err := svc.Create(r.Context(), input, "user")
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(out)
	}
}

func incidentsCloseHandler(svc *incidents.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("incident_id")
		if id == "" {
			http.Error(w, "incident_id required", http.StatusBadRequest)
			return
		}
		if err := svc.Close(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(204)
	}
}

func exportsListHandler(svc *jobs.ExportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.List(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		if list == nil {
			list = []jobs.ScheduledExport{}
		}
		json.NewEncoder(w).Encode(list)
	}
}

func exportsCreateHandler(svc *jobs.ExportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input jobs.CreateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		out, err := svc.Create(r.Context(), input)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(out)
	}
}

func exportsDeleteHandler(svc *jobs.ExportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("export_id")
		if id == "" {
			http.Error(w, "export_id required", http.StatusBadRequest)
			return
		}
		if err := svc.Delete(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(204)
	}
}

func exportsRunNowHandler(svc *jobs.ExportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("export_id")
		if id == "" {
			http.Error(w, "export_id required", http.StatusBadRequest)
			return
		}
		err := svc.RunExport(r.Context(), id)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"ok":false,"error":%q}`, err.Error())
			return
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}
}

func aiConfigGetHandler(svc *aiquery.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := svc.GetConfig(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		json.NewEncoder(w).Encode(cfg)
	}
}

func aiConfigPutHandler(svc *aiquery.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input aiquery.Config
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := svc.SetConfig(r.Context(), input); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg, _ := svc.GetConfig(r.Context())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	}
}

func aiQueryHandler(svc *aiquery.Service, card *aiquery.SchemaCard, llmSvc *llm.LLMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Question string `json:"question"`
			SiteID   string `json:"site_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Question == "" {
			http.Error(w, "question required", http.StatusBadRequest)
			return
		}
		siteID := input.SiteID
		if siteID == "" {
			siteID = "default"
		}
		ctx := r.Context()
		schema, err := card.Get(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result, err := svc.Generate(ctx, input.Question, schema)
		// Dogfood: record the AI call in llm_traces whether success or error.
		go func(ok bool, errMsg string) {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			status := "ok"
			if !ok {
				status = "error"
			}
			_, _ = llmSvc.Ingest(bg, llm.LLMInput{
				SiteID:           siteID,
				Provider:         "aiquery",
				Model:            result.Model,
				Operation:        "explorer_nl_to_sql",
				PromptTokens:     result.TokensIn,
				CompletionTokens: result.TokensOut,
				LatencyMs:        int(result.LatencyMs),
				Status:           status,
				ErrorMessage:     errMsg,
				Prompt:           input.Question,
				Completion:       result.SQL,
			})
		}(err == nil, errStr(err))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(502)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		json.NewEncoder(w).Encode(result)
	}
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func explorerExplainHandler(svc *explorer.ExplorerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			SQL string `json:"sql"`
		}
		json.NewDecoder(r.Body).Decode(&input)
		if input.SQL == "" {
			http.Error(w, "sql required", http.StatusBadRequest)
			return
		}
		result, _ := svc.Explain(r.Context(), input.SQL)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func explorerTablesHandler(svc *explorer.ExplorerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tables, _ := svc.ListTables(r.Context())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tables)
	}
}

// --- Flag handlers ---

type listFlagsInput struct{ SiteID string `query:"site_id"` }

func listFlagsHandler(svc *flags.FlagService) neutron.HandlerFunc[listFlagsInput, []flags.FeatureFlag] {
	return func(ctx context.Context, input listFlagsInput) ([]flags.FeatureFlag, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createFlagInput struct {
	SiteID      string `json:"site_id"`
	FlagKey     string `json:"flag_key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	FlagType    string `json:"flag_type"`
	RolloutPct  int    `json:"rollout_pct"`
	Variants    string `json:"variants"`
	Targeting   string `json:"targeting"`
}

func createFlagHandler(svc *flags.FlagService) neutron.HandlerFunc[createFlagInput, flags.FeatureFlag] {
	return func(ctx context.Context, input createFlagInput) (flags.FeatureFlag, error) {
		if input.SiteID == "" || input.FlagKey == "" {
			return flags.FeatureFlag{}, neutron.ErrBadRequest("site_id and flag_key required")
		}
		f, err := svc.Create(ctx, input.SiteID, input.FlagKey, input.Name, input.Description, input.FlagType, input.Variants, input.Targeting, input.RolloutPct)
		if err != nil { return flags.FeatureFlag{}, err }
		return *f, nil
	}
}

type toggleFlagInput struct {
	FlagID  string `path:"flag_id"`
	Enabled bool   `json:"enabled"`
}

func toggleFlagHandler(svc *flags.FlagService) neutron.HandlerFunc[toggleFlagInput, neutron.Empty] {
	return func(ctx context.Context, input toggleFlagInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Toggle(ctx, input.FlagID, input.Enabled)
	}
}

type flagHistoryInput struct {
	FlagID string `path:"flag_id"`
}

func flagHistoryHandler(svc *flags.FlagService) neutron.HandlerFunc[flagHistoryInput, []flags.FlagHistoryEntry] {
	return func(ctx context.Context, input flagHistoryInput) ([]flags.FlagHistoryEntry, error) {
		if input.FlagID == "" {
			return nil, neutron.ErrBadRequest("flag_id required")
		}
		return emptyOnNil(svc.History(ctx, input.FlagID))
	}
}

// flagEvaluateHandler is the public, browser-callable flag SDK endpoint. It is
// IP-rate-limited to curb enumeration / write floods and validates input so a
// malformed request returns 400 instead of a silent {enabled:false} oracle.
func flagEvaluateHandler(svc *flags.FlagService, rl *ingest.RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
		var input struct {
			SiteID  string            `json:"site_id"`
			FlagKey string            `json:"flag_key"`
			UserID  string            `json:"user_id"`
			Context map[string]string `json:"context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if input.SiteID == "" || input.FlagKey == "" {
			http.Error(w, `{"error":"site_id and flag_key required"}`, http.StatusBadRequest)
			return
		}
		// Rate limit per (site, ip): bounds write floods into flag_evaluations.
		if !rl.Allow(input.SiteID, ip) {
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		result, err := svc.Evaluate(r.Context(), input.SiteID, input.FlagKey, input.UserID, input.Context)
		if err != nil {
			http.Error(w, `{"error":"evaluation failed"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(result)
	}
}

// --- Experiment handlers ---

type listExperimentsInput struct{ SiteID string `query:"site_id"` }

func listExperimentsHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[listExperimentsInput, []experiments.Experiment] {
	return func(ctx context.Context, input listExperimentsInput) ([]experiments.Experiment, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createExperimentInput struct {
	SiteID     string `json:"site_id"`
	Name       string `json:"name"`
	FlagKey    string `json:"flag_key"`
	GoalMetric string `json:"goal_metric"`
	GoalValue  string `json:"goal_value"`
	Variants   string `json:"variants"`
	MinSample  int    `json:"min_sample"`
}

func createExperimentHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[createExperimentInput, experiments.Experiment] {
	return func(ctx context.Context, input createExperimentInput) (experiments.Experiment, error) {
		if input.SiteID == "" || input.FlagKey == "" {
			return experiments.Experiment{}, neutron.ErrBadRequest("site_id and flag_key required")
		}
		e, err := svc.Create(ctx, input.SiteID, input.Name, input.FlagKey, input.GoalMetric, input.GoalValue, input.Variants, input.MinSample)
		if err != nil { return experiments.Experiment{}, err }
		return *e, nil
	}
}

type experimentExposeInput struct {
	ExperimentID string `json:"experiment_id"`
	UserID       string `json:"user_id"`
	Variant      string `json:"variant"`
}

func experimentExposeHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentExposeInput, neutron.Empty] {
	return func(ctx context.Context, input experimentExposeInput) (neutron.Empty, error) {
		siteID := ingest.SiteIDFromContext(ctx)
		if siteID == "" || input.ExperimentID == "" || input.UserID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("experiment_id and user_id required")
		}
		return neutron.Empty{}, svc.RecordExposure(ctx, input.ExperimentID, siteID, input.UserID, input.Variant)
	}
}

type experimentConvertInput struct {
	ExperimentID string `json:"experiment_id"`
	UserID       string `json:"user_id"`
}

func experimentConvertHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentConvertInput, neutron.Empty] {
	return func(ctx context.Context, input experimentConvertInput) (neutron.Empty, error) {
		siteID := ingest.SiteIDFromContext(ctx)
		if siteID == "" || input.ExperimentID == "" || input.UserID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("experiment_id and user_id required")
		}
		return neutron.Empty{}, svc.RecordConversion(ctx, input.ExperimentID, siteID, input.UserID)
	}
}

type experimentIDInput struct{ ExperimentID string `path:"experiment_id"` }

func startExperimentHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentIDInput, neutron.Empty] {
	return func(ctx context.Context, input experimentIDInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Start(ctx, input.ExperimentID)
	}
}

func stopExperimentHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentIDInput, neutron.Empty] {
	return func(ctx context.Context, input experimentIDInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Stop(ctx, input.ExperimentID)
	}
}

type experimentResultsInput struct {
	ExperimentID string `path:"experiment_id"`
	SiteID       string `query:"site_id"`
}

func experimentResultsHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentResultsInput, experiments.ExperimentResults] {
	return func(ctx context.Context, input experimentResultsInput) (experiments.ExperimentResults, error) {
		r, err := svc.Results(ctx, input.ExperimentID, input.SiteID)
		if err != nil { return experiments.ExperimentResults{}, err }
		return *r, nil
	}
}

// --- Survey handlers ---

type listSurveysInput struct{ SiteID string `query:"site_id"` }

func listSurveysHandler(svc *surveys.SurveyService) neutron.HandlerFunc[listSurveysInput, []surveys.Survey] {
	return func(ctx context.Context, input listSurveysInput) ([]surveys.Survey, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createSurveyInput struct {
	SiteID     string `json:"site_id"`
	Name       string `json:"name"`
	Questions  string `json:"questions"`
	Appearance string `json:"appearance"`
	Targeting  string `json:"targeting"`
}

func createSurveyHandler(svc *surveys.SurveyService) neutron.HandlerFunc[createSurveyInput, surveys.Survey] {
	return func(ctx context.Context, input createSurveyInput) (surveys.Survey, error) {
		if input.SiteID == "" || input.Name == "" {
			return surveys.Survey{}, neutron.ErrBadRequest("site_id and name required")
		}
		s, err := svc.Create(ctx, input.SiteID, input.Name, input.Questions, input.Appearance, input.Targeting)
		if err != nil { return surveys.Survey{}, err }
		return *s, nil
	}
}

type surveyIDInput struct{ SurveyID string `path:"survey_id"` }

func activateSurveyHandler(svc *surveys.SurveyService) neutron.HandlerFunc[surveyIDInput, neutron.Empty] {
	return func(ctx context.Context, input surveyIDInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Activate(ctx, input.SurveyID)
	}
}

type surveyResponsesInput struct {
	SurveyID string `path:"survey_id"`
	SiteID   string `query:"site_id"`
	Limit    int    `query:"limit"`
}

func surveyResponsesHandler(svc *surveys.SurveyService) neutron.HandlerFunc[surveyResponsesInput, []surveys.SurveyResponse] {
	return func(ctx context.Context, input surveyResponsesInput) ([]surveys.SurveyResponse, error) {
		return emptyOnNil(svc.ListResponses(ctx, input.SurveyID, input.SiteID, input.Limit))
	}
}

func activeSurveysPublicHandler(svc *surveys.SurveyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.URL.Query().Get("site_id")
		active, _ := svc.GetActive(r.Context(), siteID)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if active == nil { active = []surveys.Survey{} }
		json.NewEncoder(w).Encode(active)
	}
}

func surveyRespondHandler(svc *surveys.SurveyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			SurveyID string         `json:"survey_id"`
			SiteID   string         `json:"site_id"`
			UserID   string         `json:"user_id"`
			Answers  map[string]any `json:"answers"`
		}
		json.NewDecoder(r.Body).Decode(&input)
		id, err := svc.SubmitResponse(r.Context(), input.SurveyID, input.SiteID, input.UserID, input.Answers)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if err != nil {
			fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
			return
		}
		fmt.Fprintf(w, `{"ok":true,"response_id":"%s"}`, id)
	}
}

// --- Report handlers ---

type listReportsInput struct {
	SiteID string `query:"site_id"`
}

func listReportsHandler(svc *reports.ReportService) neutron.HandlerFunc[listReportsInput, []reports.ReportSchedule] {
	return func(ctx context.Context, input listReportsInput) ([]reports.ReportSchedule, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createReportInput struct {
	SiteID     string `json:"site_id"`
	Name       string `json:"name"`
	Frequency  string `json:"frequency"`
	Recipients string `json:"recipients"`
}

func createReportHandler(svc *reports.ReportService) neutron.HandlerFunc[createReportInput, reports.ReportSchedule] {
	return func(ctx context.Context, input createReportInput) (reports.ReportSchedule, error) {
		if input.SiteID == "" || input.Recipients == "" {
			return reports.ReportSchedule{}, neutron.ErrBadRequest("site_id and recipients required")
		}
		r, err := svc.Create(ctx, input.SiteID, input.Name, input.Frequency, input.Recipients)
		if err != nil {
			return reports.ReportSchedule{}, err
		}
		return *r, nil
	}
}

type deleteReportInput struct {
	ScheduleID string `path:"schedule_id"`
}

func deleteReportHandler(svc *reports.ReportService) neutron.HandlerFunc[deleteReportInput, neutron.Empty] {
	return func(ctx context.Context, input deleteReportInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Delete(ctx, input.ScheduleID)
	}
}

// --- Integration handlers ---

type listIntegrationsInput struct {
	SiteID string `query:"site_id"`
}

func listIntegrationsHandler(svc *integrations.IntegrationService) neutron.HandlerFunc[listIntegrationsInput, []integrations.Integration] {
	return func(ctx context.Context, input listIntegrationsInput) ([]integrations.Integration, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createIntegrationInput struct {
	SiteID string `json:"site_id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config"`
}

func createIntegrationHandler(svc *integrations.IntegrationService) neutron.HandlerFunc[createIntegrationInput, integrations.Integration] {
	return func(ctx context.Context, input createIntegrationInput) (integrations.Integration, error) {
		if input.SiteID == "" || input.Type == "" {
			return integrations.Integration{}, neutron.ErrBadRequest("site_id and type required")
		}
		i, err := svc.Create(ctx, input.SiteID, input.Name, input.Type, input.Config)
		if err != nil {
			return integrations.Integration{}, err
		}
		return *i, nil
	}
}

type deleteIntegrationInput struct {
	IntegrationID string `path:"integration_id"`
}

func deleteIntegrationHandler(svc *integrations.IntegrationService) neutron.HandlerFunc[deleteIntegrationInput, neutron.Empty] {
	return func(ctx context.Context, input deleteIntegrationInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Delete(ctx, input.IntegrationID)
	}
}

type testIntegrationInput struct {
	IntegrationID string `path:"integration_id"`
}
type testIntegrationResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

func testIntegrationHandler(svc *integrations.IntegrationService) neutron.HandlerFunc[testIntegrationInput, testIntegrationResponse] {
	return func(ctx context.Context, input testIntegrationInput) (testIntegrationResponse, error) {
		if input.IntegrationID == "" {
			return testIntegrationResponse{}, neutron.ErrBadRequest("integration_id required")
		}
		if err := svc.Test(ctx, input.IntegrationID); err != nil {
			return testIntegrationResponse{OK: false, Message: err.Error()}, nil
		}
		return testIntegrationResponse{OK: true, Message: "Test delivered"}, nil
	}
}

type listDeliveriesInput struct {
	IntegrationID string `path:"integration_id"`
	Limit         int    `query:"limit"`
}

func listDeliveriesHandler(svc *integrations.IntegrationService) neutron.HandlerFunc[listDeliveriesInput, []integrations.Delivery] {
	return func(ctx context.Context, input listDeliveriesInput) ([]integrations.Delivery, error) {
		if input.IntegrationID == "" {
			return nil, neutron.ErrBadRequest("integration_id required")
		}
		rows, err := svc.ListDeliveries(ctx, input.IntegrationID, input.Limit)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []integrations.Delivery{}
		}
		return rows, nil
	}
}

type replayDeliveryInput struct {
	DeliveryID string `path:"delivery_id"`
}

func replayDeliveryHandler(svc *integrations.IntegrationService) neutron.HandlerFunc[replayDeliveryInput, testIntegrationResponse] {
	return func(ctx context.Context, input replayDeliveryInput) (testIntegrationResponse, error) {
		if input.DeliveryID == "" {
			return testIntegrationResponse{}, neutron.ErrBadRequest("delivery_id required")
		}
		if err := svc.Replay(ctx, input.DeliveryID); err != nil {
			return testIntegrationResponse{OK: false, Message: err.Error()}, nil
		}
		return testIntegrationResponse{OK: true, Message: "Replay delivered"}, nil
	}
}

// --- Feedback handlers ---

func feedbackSubmitHandler(svc *feedback.FeedbackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input feedback.FeedbackInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		id, err := svc.Submit(r.Context(), input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprintf(w, `{"ok":true,"feedback_id":"%s"}`, id)
	}
}

type listFeedbackInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
	Limit  int    `query:"limit"`
}

func listFeedbackHandler(svc *feedback.FeedbackService) neutron.HandlerFunc[listFeedbackInput, []feedback.FeedbackEntry] {
	return func(ctx context.Context, input listFeedbackInput) ([]feedback.FeedbackEntry, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.List(ctx, input.SiteID, from, to, input.Limit))
	}
}

// --- Saved views handlers ---

type listViewsInput struct {
	SiteID string `query:"site_id"`
}

func listViewsHandler(svc *views.ViewService) neutron.HandlerFunc[listViewsInput, []views.SavedView] {
	return func(ctx context.Context, input listViewsInput) ([]views.SavedView, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createViewInput struct {
	SiteID     string `json:"site_id"`
	Name       string `json:"name"`
	ViewConfig string `json:"view_config"`
}

func createViewHandler(svc *views.ViewService) neutron.HandlerFunc[createViewInput, views.SavedView] {
	return func(ctx context.Context, input createViewInput) (views.SavedView, error) {
		if input.SiteID == "" || input.Name == "" {
			return views.SavedView{}, neutron.ErrBadRequest("site_id and name required")
		}
		v, err := svc.Create(ctx, input.SiteID, input.Name, input.ViewConfig, "")
		if err != nil {
			return views.SavedView{}, err
		}
		return *v, nil
	}
}

type deleteViewInput struct {
	ViewID string `path:"view_id"`
}

func deleteViewHandler(svc *views.ViewService) neutron.HandlerFunc[deleteViewInput, neutron.Empty] {
	return func(ctx context.Context, input deleteViewInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Delete(ctx, input.ViewID)
	}
}

// --- Release health handler ---

type releaseHealthInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func releaseHealthHandler(svc *obserrors.IssueService) neutron.HandlerFunc[releaseHealthInput, []obserrors.ReleaseHealth] {
	return func(ctx context.Context, input releaseHealthInput) ([]obserrors.ReleaseHealth, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.ReleaseHealthList(ctx, input.SiteID, from, to))
	}
}

// releaseHealthV2Handler is the new per-release crash-free + adoption +
// error-rate endpoint introduced in Wave 1 (B2 phase 1). It complements
// the legacy ReleaseHealthList (counts only) without removing it.
func releaseHealthV2Handler(svc *obserrors.ReleaseHealthService) neutron.HandlerFunc[releaseHealthInput, []obserrors.ReleaseStat] {
	return func(ctx context.Context, input releaseHealthInput) ([]obserrors.ReleaseStat, error) {
		if input.SiteID == "" {
			input.SiteID = "default"
		}
		from, to := parseTimeRange(input.From, input.To)
		stats, err := svc.Health(ctx, input.SiteID, from.UnixMilli(), to.UnixMilli())
		if err != nil {
			return nil, err
		}
		if stats == nil {
			stats = []obserrors.ReleaseStat{}
		}
		return stats, nil
	}
}

type releaseSparklineInput struct {
	SiteID  string `query:"site_id"`
	Release string `query:"release"`
	Days    int    `query:"days"`
}

func releaseSparklineHandler(svc *obserrors.ReleaseHealthService) neutron.HandlerFunc[releaseSparklineInput, []obserrors.ReleaseSparklinePoint] {
	return func(ctx context.Context, input releaseSparklineInput) ([]obserrors.ReleaseSparklinePoint, error) {
		if input.SiteID == "" {
			input.SiteID = "default"
		}
		if input.Release == "" {
			return []obserrors.ReleaseSparklinePoint{}, neutron.ErrBadRequest("release required")
		}
		days := input.Days
		if days <= 0 {
			days = 14
		}
		points, err := svc.Sparkline(ctx, input.SiteID, input.Release, days)
		if err != nil {
			return nil, err
		}
		if points == nil {
			points = []obserrors.ReleaseSparklinePoint{}
		}
		return points, nil
	}
}

// --- Log handlers ---

type logIngestResponse struct {
	OK    bool   `json:"ok"`
	LogID string `json:"log_id"`
}

func logIngestHandler(svc *logs.LogService) neutron.HandlerFunc[logs.LogInput, logIngestResponse] {
	return func(ctx context.Context, input logs.LogInput) (logIngestResponse, error) {
		if input.SiteID == "" {
			input.SiteID = ingest.SiteIDFromContext(ctx)
		}
		id, err := svc.IngestLog(ctx, input)
		if err != nil {
			return logIngestResponse{}, err
		}
		return logIngestResponse{OK: true, LogID: id}, nil
	}
}

type logSearchInput struct {
	SiteID  string `query:"site_id"`
	From    string `query:"from"`
	To      string `query:"to"`
	Level   string `query:"level"`
	Service string `query:"service"`
	Query   string `query:"q"`
	Limit   int    `query:"limit"`
	Offset  int    `query:"offset"`
}

func logSearchHandler(svc *logs.LogService) neutron.HandlerFunc[logSearchInput, []logs.Log] {
	return func(ctx context.Context, input logSearchInput) ([]logs.Log, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.SearchLogs(ctx, input.SiteID, from, to, input.Level, input.Service, input.Query, input.Limit, input.Offset))
	}
}

type logStatsInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func logStatsHandler(svc *logs.LogService) neutron.HandlerFunc[logStatsInput, []logs.LevelCount] {
	return func(ctx context.Context, input logStatsInput) ([]logs.LevelCount, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.LogStats(ctx, input.SiteID, from, to))
	}
}

type logHistogramInput struct {
	SiteID   string `query:"site_id"`
	From     string `query:"from"`
	To       string `query:"to"`
	BucketMs int64  `query:"bucket_ms"`
}

func logHistogramHandler(svc *logs.LogService) neutron.HandlerFunc[logHistogramInput, []logs.HistogramBucket] {
	return func(ctx context.Context, input logHistogramInput) ([]logs.HistogramBucket, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.Histogram(ctx, input.SiteID, from, to, input.BucketMs))
	}
}

// logStreamHandler returns a Server-Sent Events handler that streams new logs
// for the given site_id as they arrive.
func logStreamHandler(svc *logs.LogService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.URL.Query().Get("site_id")
		if siteID == "" {
			http.Error(w, "site_id required", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		sub := svc.Bx.Subscribe(siteID)
		defer svc.Bx.Close(sub)

		// Hello event so clients know the stream is open.
		_, _ = fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		keepalive := time.NewTicker(25 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepalive.C:
				if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case log, ok := <-sub.Ch:
				if !ok {
					return
				}
				raw, err := json.Marshal(log)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// --- Goal handlers ---

type listGoalsInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func listGoalsHandler(svc *query.StatsService) neutron.HandlerFunc[listGoalsInput, []query.GoalConversion] {
	return func(ctx context.Context, input listGoalsInput) ([]query.GoalConversion, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.GoalConversions(ctx, input.SiteID, from, to))
	}
}

type createGoalInput struct {
	SiteID    string `json:"site_id"`
	Name      string `json:"name"`
	GoalType  string `json:"goal_type"`
	GoalValue string `json:"goal_value"`
}

func createGoalHandler(svc *query.StatsService) neutron.HandlerFunc[createGoalInput, query.Goal] {
	return func(ctx context.Context, input createGoalInput) (query.Goal, error) {
		if input.SiteID == "" || input.Name == "" || input.GoalValue == "" {
			return query.Goal{}, neutron.ErrBadRequest("site_id, name, and goal_value required")
		}
		g, err := svc.CreateGoal(ctx, input.SiteID, input.Name, input.GoalType, input.GoalValue)
		if err != nil {
			return query.Goal{}, err
		}
		return *g, nil
	}
}

// --- Uptime monitor handlers ---

type listMonitorsInput struct {
	SiteID string `query:"site_id"`
}

func listMonitorsHandler(svc *monitoring.UptimeService) neutron.HandlerFunc[listMonitorsInput, []monitoring.Monitor] {
	return func(ctx context.Context, input listMonitorsInput) ([]monitoring.Monitor, error) {
		return emptyOnNil(svc.ListMonitors(ctx, input.SiteID))
	}
}

type createMonitorInput struct {
	SiteID         string `json:"site_id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	IntervalSecs   int    `json:"interval_secs"`
	ExpectedStatus int    `json:"expected_status"`
}

func createMonitorHandler(svc *monitoring.UptimeService) neutron.HandlerFunc[createMonitorInput, monitoring.Monitor] {
	return func(ctx context.Context, input createMonitorInput) (monitoring.Monitor, error) {
		if input.SiteID == "" || input.URL == "" {
			return monitoring.Monitor{}, neutron.ErrBadRequest("site_id and url required")
		}
		m := monitoring.Monitor{
			SiteID: input.SiteID, Name: input.Name, URL: input.URL,
			IntervalSecs: input.IntervalSecs, ExpectedStatus: input.ExpectedStatus,
		}
		created, err := svc.CreateMonitor(ctx, m)
		if err != nil {
			return monitoring.Monitor{}, err
		}
		return *created, nil
	}
}

type monitorResultsInput struct {
	MonitorID string `path:"monitor_id"`
	SiteID    string `query:"site_id"`
	Limit     int    `query:"limit"`
}

func monitorResultsHandler(svc *monitoring.UptimeService) neutron.HandlerFunc[monitorResultsInput, []monitoring.MonitorResult] {
	return func(ctx context.Context, input monitorResultsInput) ([]monitoring.MonitorResult, error) {
		return emptyOnNil(svc.ListResults(ctx, input.MonitorID, input.Limit))
	}
}

type deleteMonitorInput struct {
	MonitorID string `path:"monitor_id"`
	SiteID    string `query:"site_id"`
}

func deleteMonitorHandler(svc *monitoring.UptimeService) neutron.HandlerFunc[deleteMonitorInput, neutron.Empty] {
	return func(ctx context.Context, input deleteMonitorInput) (neutron.Empty, error) {
		if input.SiteID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("site_id required")
		}
		return neutron.Empty{}, svc.DeleteMonitor(ctx, input.SiteID, input.MonitorID)
	}
}

// --- Cron monitor handlers ---

type listCronsInput struct {
	SiteID string `query:"site_id"`
}

func listCronsHandler(svc *monitoring.CronService) neutron.HandlerFunc[listCronsInput, []monitoring.CronMonitor] {
	return func(ctx context.Context, input listCronsInput) ([]monitoring.CronMonitor, error) {
		return emptyOnNil(svc.ListCrons(ctx, input.SiteID))
	}
}

type createCronInput struct {
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	Schedule    string `json:"schedule"`
	GracePeriod int    `json:"grace_period"`
}

func createCronHandler(svc *monitoring.CronService) neutron.HandlerFunc[createCronInput, monitoring.CronMonitor] {
	return func(ctx context.Context, input createCronInput) (monitoring.CronMonitor, error) {
		if input.SiteID == "" || input.Name == "" {
			return monitoring.CronMonitor{}, neutron.ErrBadRequest("site_id and name required")
		}
		cm := monitoring.CronMonitor{
			SiteID: input.SiteID, Name: input.Name,
			Schedule: input.Schedule, GracePeriod: input.GracePeriod,
		}
		c, err := svc.CreateCron(ctx, cm)
		if err != nil {
			return monitoring.CronMonitor{}, err
		}
		return *c, nil
	}
}

// cronCheckinHandler records a heartbeat for the cron identified by {site_id}
// and {slug}. The single-segment route omits {site_id} and falls back to the
// "default" site for single-tenant installs. Arguments must be passed to
// RecordCheckin in (site_id, slug) order — passing the slug as the site_id was
// a latent bug that made every check-in 404 and fired missed-check for all crons.
func cronCheckinHandler(svc *monitoring.CronService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if slug == "" {
			http.Error(w, "missing slug", http.StatusBadRequest)
			return
		}
		siteID := r.PathValue("site_id")
		if siteID == "" {
			siteID = "default"
		}
		if err := svc.RecordCheckin(r.Context(), siteID, slug, "ok", 0); err != nil {
			// Only a genuine not-found is 404; backend errors are 5xx so the
			// heartbeat client retries instead of giving up.
			if errors.Is(err, monitoring.ErrCronNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
}

type deleteCronInput struct {
	CronID string `path:"cron_id"`
	SiteID string `query:"site_id"`
}

func deleteCronHandler(svc *monitoring.CronService) neutron.HandlerFunc[deleteCronInput, neutron.Empty] {
	return func(ctx context.Context, input deleteCronInput) (neutron.Empty, error) {
		if input.SiteID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("site_id required")
		}
		return neutron.Empty{}, svc.DeleteCron(ctx, input.SiteID, input.CronID)
	}
}

// cronCheckinByTokenHandler records a heartbeat for the cron identified by its
// opaque ping token. Unauthenticated by design (heartbeats come from arbitrary
// job runners) but the token — not a guessable site/slug — is the bearer secret.
func cronCheckinByTokenHandler(svc *monitoring.CronService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("ping_token")
		if err := svc.RecordCheckinByToken(r.Context(), token, "ok", 0); err != nil {
			if errors.Is(err, monitoring.ErrCronNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
}

// --- Dashboard handlers ---

type listDashboardsInput struct {
	SiteID string `query:"site_id"`
}

func listDashboardsHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[listDashboardsInput, []dashboards.Dashboard] {
	return func(ctx context.Context, input listDashboardsInput) ([]dashboards.Dashboard, error) {
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createDashboardInput struct {
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func createDashboardHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[createDashboardInput, dashboards.Dashboard] {
	return func(ctx context.Context, input createDashboardInput) (dashboards.Dashboard, error) {
		if input.SiteID == "" || input.Name == "" {
			return dashboards.Dashboard{}, neutron.ErrBadRequest("site_id and name required")
		}
		d, err := svc.Create(ctx, input.SiteID, input.Name, input.Description, "")
		if err != nil {
			return dashboards.Dashboard{}, err
		}
		return *d, nil
	}
}

type getDashboardInput struct {
	DashboardID string `path:"dashboard_id"`
}

type dashboardDetail struct {
	dashboards.Dashboard
	Panels []dashboards.Panel `json:"panels"`
}

func getDashboardHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[getDashboardInput, dashboardDetail] {
	return func(ctx context.Context, input getDashboardInput) (dashboardDetail, error) {
		d, err := svc.Get(ctx, input.DashboardID)
		if err != nil || d == nil {
			return dashboardDetail{}, neutron.ErrNotFound("dashboard not found")
		}
		panels, _ := svc.ListPanels(ctx, input.DashboardID)
		if panels == nil {
			panels = []dashboards.Panel{}
		}
		return dashboardDetail{Dashboard: *d, Panels: panels}, nil
	}
}

type addPanelInput struct {
	DashboardID string `path:"dashboard_id"`
	PanelType   string `json:"panel_type"`
	Title       string `json:"title"`
	QueryType   string `json:"query_type"`
	QueryConfig string `json:"query_config"`
	PositionX   string `json:"position_x"`
	PositionY   string `json:"position_y"`
	Width       string `json:"width"`
	Height      string `json:"height"`
}

func addPanelHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[addPanelInput, dashboards.Panel] {
	return func(ctx context.Context, input addPanelInput) (dashboards.Panel, error) {
		panel := dashboards.Panel{
			PanelType: input.PanelType, Title: input.Title,
			QueryType: input.QueryType, QueryConfig: input.QueryConfig,
			PositionX: input.PositionX, PositionY: input.PositionY,
			Width: input.Width, Height: input.Height,
		}
		p, err := svc.AddPanel(ctx, input.DashboardID, panel)
		if err != nil {
			return dashboards.Panel{}, err
		}
		return *p, nil
	}
}

type updatePanelLayoutInput struct {
	DashboardID string `path:"dashboard_id"`
	PanelID     string `path:"panel_id"`
	PositionX   string `json:"position_x"`
	PositionY   string `json:"position_y"`
	Width       string `json:"width"`
	Height      string `json:"height"`
}

func updatePanelLayoutHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[updatePanelLayoutInput, neutron.Empty] {
	return func(ctx context.Context, input updatePanelLayoutInput) (neutron.Empty, error) {
		if input.PanelID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("panel_id required")
		}
		// Fetch existing to preserve type/title/query fields.
		panels, err := svc.ListPanels(ctx, input.DashboardID)
		if err != nil {
			return neutron.Empty{}, err
		}
		var target *dashboards.Panel
		for i := range panels {
			if panels[i].PanelID == input.PanelID {
				target = &panels[i]
				break
			}
		}
		if target == nil {
			return neutron.Empty{}, neutron.ErrBadRequest("panel not found")
		}
		if input.PositionX != "" { target.PositionX = input.PositionX }
		if input.PositionY != "" { target.PositionY = input.PositionY }
		if input.Width != "" { target.Width = input.Width }
		if input.Height != "" { target.Height = input.Height }
		target.DashboardID = input.DashboardID
		return neutron.Empty{}, svc.UpdatePanel(ctx, *target)
	}
}

type deleteDashboardInput struct {
	DashboardID string `path:"dashboard_id"`
}

func deleteDashboardHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[deleteDashboardInput, neutron.Empty] {
	return func(ctx context.Context, input deleteDashboardInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Delete(ctx, input.DashboardID)
	}
}

type executePanelInput struct {
	DashboardID string `path:"dashboard_id"`
	PanelID     string `path:"panel_id"`
	SiteID      string `json:"site_id"`
	From        string `json:"from"`
	To          string `json:"to"`
}

func executePanelHandler(svc *dashboards.DashboardService) neutron.HandlerFunc[executePanelInput, any] {
	return func(ctx context.Context, input executePanelInput) (any, error) {
		panels, err := svc.ListPanels(ctx, input.DashboardID)
		if err != nil {
			return nil, err
		}
		for _, p := range panels {
			if p.PanelID == input.PanelID {
				return svc.ExecutePanel(ctx, input.SiteID, p, input.From, input.To)
			}
		}
		return nil, neutron.ErrNotFound("panel not found")
	}
}

// --- Replay handlers ---

func replayIngestHandler(svc *replays.ReplayService) neutron.HandlerFunc[replays.IngestInput, map[string]string] {
	return func(ctx context.Context, input replays.IngestInput) (map[string]string, error) {
		if input.SiteID == "" {
			input.SiteID = ingest.SiteIDFromContext(ctx)
		}
		id, err := svc.Ingest(ctx, input)
		if err != nil {
			return nil, err
		}
		return map[string]string{"ok": "true", "replay_id": id}, nil
	}
}

type listReplaysInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
	Limit  int    `query:"limit"`
	Offset int    `query:"offset"`
}

func listReplaysHandler(svc *replays.ReplayService) neutron.HandlerFunc[listReplaysInput, []replays.ReplaySession] {
	return func(ctx context.Context, input listReplaysInput) ([]replays.ReplaySession, error) {
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.ListReplays(ctx, input.SiteID, from, to, input.Limit, input.Offset))
	}
}

type getReplayInput struct {
	ReplayID string `path:"replay_id"`
}

func getReplayHandler(svc *replays.ReplayService) neutron.HandlerFunc[getReplayInput, []replays.ReplayEvent] {
	return func(ctx context.Context, input getReplayInput) ([]replays.ReplayEvent, error) {
		return emptyOnNil(svc.GetReplayEvents(ctx, input.ReplayID))
	}
}

// --- Heatmap handlers ---

type queryHeatmapInput struct {
	SiteID string `query:"site_id"`
	URL    string `query:"url"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func queryHeatmapHandler(svc *heatmaps.Service) neutron.HandlerFunc[queryHeatmapInput, []heatmaps.Click] {
	return func(ctx context.Context, input queryHeatmapInput) ([]heatmaps.Click, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		if input.URL == "" {
			return nil, neutron.ErrBadRequest("url required")
		}
		from, to := parseTimeRange(input.From, input.To)
		clicks, err := svc.Query(ctx, input.SiteID, input.URL, from.UnixMilli(), to.UnixMilli())
		if err != nil {
			return nil, err
		}
		if clicks == nil {
			clicks = []heatmaps.Click{}
		}
		return clicks, nil
	}
}

type replayIssuesInput struct {
	ReplayID string `path:"replay_id"`
	SiteID   string `query:"site_id"`
}

func replayIssuesHandler(svc *obserrors.IssueService) neutron.HandlerFunc[replayIssuesInput, []obserrors.Issue] {
	return func(ctx context.Context, input replayIssuesInput) ([]obserrors.Issue, error) {
		if input.SiteID == "" {
			input.SiteID = "default"
		}
		return emptyOnNil(svc.IssuesByReplay(ctx, input.SiteID, input.ReplayID))
	}
}

// --- Link handlers ---

type listLinksInput struct {
	SiteID string `query:"site_id"`
}

func listLinksHandler(svc *tracking.LinkService) neutron.HandlerFunc[listLinksInput, []tracking.TrackedLink] {
	return func(ctx context.Context, input listLinksInput) ([]tracking.TrackedLink, error) {
		return emptyOnNil(svc.ListLinks(ctx, "default", input.SiteID))
	}
}

type createLinkInput struct {
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	Destination string `json:"destination"`
}

func createLinkHandler(svc *tracking.LinkService) neutron.HandlerFunc[createLinkInput, tracking.TrackedLink] {
	return func(ctx context.Context, input createLinkInput) (tracking.TrackedLink, error) {
		if input.SiteID == "" || input.Destination == "" {
			return tracking.TrackedLink{}, neutron.ErrBadRequest("site_id and destination required")
		}
		link, err := svc.CreateLink(ctx, "default", input.SiteID, input.Name, input.Destination)
		if err != nil {
			return tracking.TrackedLink{}, err
		}
		return link, nil
	}
}

// --- Source map upload handler ---

func srcmapUploadHandler(svc *sourcemaps.SourceMapService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.FormValue("site_id")
		release := r.FormValue("release")
		filename := r.FormValue("filename")
		if siteID == "" || release == "" || filename == "" {
			http.Error(w, "site_id, release, and filename required", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("sourcemap")
		if err != nil {
			http.Error(w, "sourcemap file required", http.StatusBadRequest)
			return
		}
		defer file.Close()
		// A single file.Read can return fewer bytes than requested, silently
		// truncating larger sourcemaps and breaking symbolication. Read fully up
		// to the cap (+1 byte so an over-limit upload is detected, not truncated).
		const maxSourcemap = 10 * 1024 * 1024 // 10MB
		data, err := io.ReadAll(io.LimitReader(file, maxSourcemap+1))
		if err != nil {
			http.Error(w, "failed to read sourcemap", http.StatusBadRequest)
			return
		}
		if len(data) > maxSourcemap {
			http.Error(w, "sourcemap exceeds 10MB limit", http.StatusRequestEntityTooLarge)
			return
		}

		if err := svc.Upload(r.Context(), siteID, release, filename, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		svc.TrackRelease(r.Context(), siteID, release)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
}

// --- Platform handlers ---

type listUsersInput struct{}

func listUsersHandler(svc *platform.UserService) neutron.HandlerFunc[listUsersInput, []platform.User] {
	return func(ctx context.Context, _ listUsersInput) ([]platform.User, error) {
		users, err := svc.List(ctx)
		if err != nil {
			return nil, err
		}
		if users == nil {
			users = []platform.User{}
		}
		return users, nil
	}
}

type createUserInput struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func createUserHandler(svc *platform.UserService) neutron.HandlerFunc[createUserInput, platform.User] {
	return func(ctx context.Context, input createUserInput) (platform.User, error) {
		if input.Username == "" || input.Password == "" {
			return platform.User{}, neutron.ErrBadRequest("username and password required")
		}
		user, err := svc.Create(ctx, input.Username, input.Email, input.Password, input.Role, "")
		if err != nil {
			return platform.User{}, err
		}
		return *user, nil
	}
}

type updateRoleInput struct {
	UserID string `path:"user_id"`
	Role   string `json:"role"`
}

func updateUserRoleHandler(svc *platform.UserService) neutron.HandlerFunc[updateRoleInput, neutron.Empty] {
	return func(ctx context.Context, input updateRoleInput) (neutron.Empty, error) {
		if input.UserID == "" || input.Role == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("user_id and role required")
		}
		return neutron.Empty{}, svc.UpdateRole(ctx, input.UserID, input.Role)
	}
}

type listAlertRulesInput struct {
	SiteID string `query:"site_id"`
}

func listAlertRulesHandler(svc *platform.AlertService) neutron.HandlerFunc[listAlertRulesInput, []platform.AlertRule] {
	return func(ctx context.Context, input listAlertRulesInput) ([]platform.AlertRule, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return emptyOnNil(svc.ListRules(ctx, input.SiteID))
	}
}

type createAlertRuleInput struct {
	SiteID        string  `json:"site_id"`
	Name          string  `json:"name"`
	Metric        string  `json:"metric"`
	Operator      string  `json:"operator"`
	Threshold     float64 `json:"threshold"`
	WindowMinutes int     `json:"window_minutes"`
	Cooldown      int     `json:"cooldown"`
}

var validAlertMetrics = map[string]struct{}{
	"error_count": {}, "error_rate": {}, "pageviews": {}, "visitors": {},
}

var validAlertOperators = map[string]struct{}{
	"gt": {}, "gte": {}, "lt": {}, "lte": {}, "eq": {},
}

func createAlertRuleHandler(svc *platform.AlertService) neutron.HandlerFunc[createAlertRuleInput, platform.AlertRule] {
	return func(ctx context.Context, input createAlertRuleInput) (platform.AlertRule, error) {
		if input.SiteID == "" || input.Name == "" || input.Metric == "" {
			return platform.AlertRule{}, neutron.ErrBadRequest("site_id, name, and metric required")
		}
		if _, ok := validAlertMetrics[input.Metric]; !ok {
			return platform.AlertRule{}, neutron.ErrBadRequest("unsupported metric: " + input.Metric)
		}
		if input.Operator != "" {
			if _, ok := validAlertOperators[input.Operator]; !ok {
				return platform.AlertRule{}, neutron.ErrBadRequest("unsupported operator: " + input.Operator)
			}
		}
		rule := platform.AlertRule{
			SiteID: input.SiteID, Name: input.Name, Metric: input.Metric,
			Operator:      input.Operator,
			Threshold:     input.Threshold,
			WindowMinutes: input.WindowMinutes,
			Cooldown:      input.Cooldown,
		}
		r, err := svc.CreateRule(ctx, rule)
		if err != nil {
			return platform.AlertRule{}, err
		}
		return *r, nil
	}
}

type deleteAlertRuleInput struct {
	RuleID string `path:"rule_id"`
}

func deleteAlertRuleHandler(svc *platform.AlertService) neutron.HandlerFunc[deleteAlertRuleInput, neutron.Empty] {
	return func(ctx context.Context, input deleteAlertRuleInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.DeleteRule(ctx, input.RuleID)
	}
}

type silenceAlertRuleInput struct {
	RuleID    string `path:"rule_id"`
	Minutes   int    `json:"minutes"`
}
type silenceAlertRuleResponse struct {
	SilenceUntilMs int64 `json:"silence_until_ms"`
}

func silenceAlertRuleHandler(svc *platform.AlertService) neutron.HandlerFunc[silenceAlertRuleInput, silenceAlertRuleResponse] {
	return func(ctx context.Context, input silenceAlertRuleInput) (silenceAlertRuleResponse, error) {
		if input.RuleID == "" {
			return silenceAlertRuleResponse{}, neutron.ErrBadRequest("rule_id required")
		}
		d := time.Duration(input.Minutes) * time.Minute
		if err := svc.Silence(ctx, input.RuleID, d); err != nil {
			return silenceAlertRuleResponse{}, err
		}
		return silenceAlertRuleResponse{SilenceUntilMs: svc.SilenceStatus(ctx, input.RuleID)}, nil
	}
}

type alertHistoryInput struct {
	SiteID string `query:"site_id"`
	Limit  int    `query:"limit"`
	Offset int    `query:"offset"`
}

func alertHistoryHandler(svc *platform.AlertService) neutron.HandlerFunc[alertHistoryInput, []platform.AlertHistoryEntry] {
	return func(ctx context.Context, input alertHistoryInput) ([]platform.AlertHistoryEntry, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return emptyOnNil(svc.ListHistory(ctx, input.SiteID, input.Limit, input.Offset))
	}
}

type listWebhooksInput struct {
	SiteID string `query:"site_id"`
}

func listWebhooksHandler(svc *platform.WebhookService) neutron.HandlerFunc[listWebhooksInput, []platform.Webhook] {
	return func(ctx context.Context, input listWebhooksInput) ([]platform.Webhook, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return emptyOnNil(svc.List(ctx, input.SiteID))
	}
}

type createWebhookInput struct {
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	WebhookType string `json:"webhook_type"`
	URL         string `json:"url"`
}

func createWebhookHandler(svc *platform.WebhookService) neutron.HandlerFunc[createWebhookInput, platform.Webhook] {
	return func(ctx context.Context, input createWebhookInput) (platform.Webhook, error) {
		if input.SiteID == "" || input.URL == "" {
			return platform.Webhook{}, neutron.ErrBadRequest("site_id and url required")
		}
		w, err := svc.Create(ctx, input.SiteID, input.Name, input.WebhookType, input.URL)
		if err != nil {
			return platform.Webhook{}, err
		}
		return *w, nil
	}
}

type deleteWebhookInput struct {
	WebhookID string `path:"webhook_id"`
}

func deleteWebhookHandler(svc *platform.WebhookService) neutron.HandlerFunc[deleteWebhookInput, neutron.Empty] {
	return func(ctx context.Context, input deleteWebhookInput) (neutron.Empty, error) {
		return neutron.Empty{}, svc.Delete(ctx, input.WebhookID)
	}
}

// --- OTLP trace ingestion handler ---

func otlpTraceHandler(svc *tracing.IngestService) neutron.HandlerFunc[tracing.ExportTraceRequest, tracing.IngestResponse] {
	return func(ctx context.Context, input tracing.ExportTraceRequest) (tracing.IngestResponse, error) {
		siteID := ingest.SiteIDFromContext(ctx)
		if siteID == "" {
			return tracing.IngestResponse{}, neutron.ErrBadRequest("missing site_id")
		}
		return svc.Ingest(ctx, siteID, input)
	}
}

// --- Trace query handlers ---

type listServicesInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func listServicesHandler(svc *tracing.QueryService) neutron.HandlerFunc[listServicesInput, []tracing.ServiceSummary] {
	return func(ctx context.Context, input listServicesInput) ([]tracing.ServiceSummary, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.ListServices(ctx, input.SiteID, from, to))
	}
}

type listOpsInput struct {
	SiteID  string `query:"site_id"`
	Service string `path:"service"`
	From    string `query:"from"`
	To      string `query:"to"`
}

func listOperationsHandler(svc *tracing.QueryService) neutron.HandlerFunc[listOpsInput, []tracing.OperationSummary] {
	return func(ctx context.Context, input listOpsInput) ([]tracing.OperationSummary, error) {
		if input.SiteID == "" || input.Service == "" {
			return nil, neutron.ErrBadRequest("site_id and service required")
		}
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.ListOperations(ctx, input.SiteID, input.Service, from, to))
	}
}

type searchTracesInput struct {
	SiteID      string `query:"site_id"`
	From        string `query:"from"`
	To          string `query:"to"`
	Service     string `query:"service"`
	Operation   string `query:"operation"`
	Offset      int    `query:"offset"`
	Status      string `query:"status"`
	MinDuration int64  `query:"min_duration"`
	MaxDuration int64  `query:"max_duration"`
	Limit       int    `query:"limit"`
}

func searchTracesHandler(svc *tracing.QueryService) neutron.HandlerFunc[searchTracesInput, []tracing.TraceSummary] {
	return func(ctx context.Context, input searchTracesInput) ([]tracing.TraceSummary, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.SearchTraces(ctx, input.SiteID, from, to, input.Service, input.Operation, input.Status, input.MinDuration, input.MaxDuration, input.Limit, input.Offset))
	}
}

type getTraceInput struct {
	TraceID string `path:"trace_id"`
	SiteID  string `query:"site_id"`
}

func getTraceHandler(svc *tracing.QueryService) neutron.HandlerFunc[getTraceInput, []tracing.Span] {
	return func(ctx context.Context, input getTraceInput) ([]tracing.Span, error) {
		if input.SiteID == "" || input.TraceID == "" {
			return nil, neutron.ErrBadRequest("site_id and trace_id required")
		}
		return emptyOnNil(svc.GetTrace(ctx, input.TraceID, input.SiteID))
	}
}

type traceErrorsInput struct {
	TraceID string `path:"trace_id"`
	SiteID  string `query:"site_id"`
}

func traceErrorsHandler(svc *tracing.QueryService) neutron.HandlerFunc[traceErrorsInput, []tracing.TraceErrorHit] {
	return func(ctx context.Context, input traceErrorsInput) ([]tracing.TraceErrorHit, error) {
		if input.SiteID == "" || input.TraceID == "" {
			return nil, neutron.ErrBadRequest("site_id and trace_id required")
		}
		hits, err := svc.TraceErrors(ctx, input.TraceID, input.SiteID)
		if err != nil {
			return nil, err
		}
		if hits == nil {
			hits = []tracing.TraceErrorHit{}
		}
		return hits, nil
	}
}

type serviceDepsInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func serviceDepsHandler(svc *tracing.QueryService) neutron.HandlerFunc[serviceDepsInput, []tracing.Dependency] {
	return func(ctx context.Context, input serviceDepsInput) ([]tracing.Dependency, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(input.From, input.To)
		return emptyOnNil(svc.ServiceDependencies(ctx, input.SiteID, from, to))
	}
}

func parseTimeRange(fromStr, toStr string) (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, fromStr)
	to, _ := time.Parse(time.RFC3339, toStr)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// --- Error search handler ---

type searchIssuesInput struct {
	SiteID string `query:"site_id"`
	Query  string `query:"q"`
	Limit  int    `query:"limit"`
}

func searchIssuesHandler(svc *obserrors.SearchService) neutron.HandlerFunc[searchIssuesInput, []obserrors.Issue] {
	return func(ctx context.Context, input searchIssuesInput) ([]obserrors.Issue, error) {
		if input.SiteID == "" || input.Query == "" {
			return nil, neutron.ErrBadRequest("site_id and q required")
		}
		issues, err := svc.SearchIssues(ctx, input.SiteID, input.Query, input.Limit)
		if err != nil {
			return nil, err
		}
		if issues == nil {
			issues = []obserrors.Issue{}
		}
		return issues, nil
	}
}

type dailyErrorCountsInput struct {
	SiteID string `query:"site_id"`
	Days   int    `query:"days"`
}

func dailyErrorCountsHandler(svc *obserrors.IssueService) neutron.HandlerFunc[dailyErrorCountsInput, []obserrors.DailyCount] {
	return func(ctx context.Context, input dailyErrorCountsInput) ([]obserrors.DailyCount, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return emptyOnNil(svc.DailyCounts(ctx, input.SiteID, input.Days))
	}
}

// ipRateLimitMW rate-limits by client IP alone (no site_id), for public,
// unauthenticated endpoints like login where we want to throttle brute force.
func ipRateLimitMW(rl *ingest.RateLimiter) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
			if !rl.Allow("", ip) {
				http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// jwtOrShareMW lets a request through if it carries either a valid JWT or a
// valid public share token (X-Share-Token header or ?share_token=). For a share
// token the request must be a GET and site_id is overwritten with the token's
// resolved site, so a token can only ever read its own site's data. Anything
// else falls through to the normal JWT middleware.
func jwtOrShareMW(jwtMW neutron.Middleware, shareSvc *share.ShareService) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		jwtNext := jwtMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get("X-Share-Token")
			if tok == "" {
				tok = r.URL.Query().Get("share_token")
			}
			if tok == "" {
				jwtNext.ServeHTTP(w, r)
				return
			}
			if r.Method != http.MethodGet {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("share token is read-only"))
				return
			}
			siteID, err := shareSvc.Resolve(r.Context(), tok)
			if err != nil || siteID == "" {
				neutron.WriteError(w, r, neutron.ErrUnauthorized("invalid share token"))
				return
			}
			// Pin site_id to the token's site (ignore any client-supplied value).
			q := r.URL.Query()
			q.Set("site_id", siteID)
			r.URL.RawQuery = q.Encode()
			next.ServeHTTP(w, r)
		})
	}
}

// shareViewHandler resolves a share token and serves the dashboard UI in
// "shared" mode: the site_id and the token are injected as escaped meta tags so
// the SPA can route reads through the token-scoped stats API instead of bouncing
// the anonymous viewer to /login.
func shareViewHandler(shareSvc *share.ShareService, uiFS fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if token == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}

		siteID, err := shareSvc.Resolve(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid share link", http.StatusNotFound)
			return
		}

		data, err := fs.ReadFile(uiFS, "index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Escape values before embedding them in HTML attributes (reflected-XSS
		// sink otherwise). The token here is the path token, already validated.
		inject := fmt.Sprintf(
			`<meta name="observe-site-id" content="%s"><meta name="observe-shared" content="true"><meta name="observe-share-token" content="%s">`,
			html.EscapeString(siteID), html.EscapeString(token))
		page := strings.Replace(string(data), "</head>", inject+"</head>", 1)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}
}

// newProcAttrDetached returns a SysProcAttr that detaches the spawned child
// from the upgrader's process group so the new teploy-observe outlives the
// `teploy-observe upgrade` command. On unix this means Setsid=true.
func newProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// selfMonitorEndpoint converts a bind address (":3000", "0.0.0.0:3000")
// into a base URL the in-process SDK can call back into.
func selfMonitorEndpoint(addr string) string {
	if env := os.Getenv("OBSERVE_SELF_URL"); env != "" {
		return env
	}
	if addr == "" {
		return "http://127.0.0.1:3000"
	}
	if addr[0] == ':' {
		return "http://127.0.0.1" + addr
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return "http://" + addr
	}
	return addr
}
