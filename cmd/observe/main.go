package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/auth"
	"github.com/teploy/observe/internal/config"
	obserrors "github.com/teploy/observe/internal/errors"
	"github.com/teploy/observe/internal/export"
	"github.com/teploy/observe/internal/ingest"
	"github.com/teploy/observe/internal/jobs"
	"github.com/teploy/observe/internal/live"
	"github.com/teploy/observe/internal/query"
	"github.com/teploy/observe/internal/share"
	"github.com/teploy/observe/internal/sites"
	"github.com/teploy/observe/internal/dashboards"
	"github.com/teploy/observe/internal/logs"
	"github.com/teploy/observe/internal/monitoring"
	"github.com/teploy/observe/internal/platform"
	"github.com/teploy/observe/internal/replays"
	"github.com/teploy/observe/internal/sourcemaps"
	"github.com/teploy/observe/internal/tracking"
	"github.com/teploy/observe/internal/tracing"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed all:ui/dist
var uiFS embed.FS

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	// Connect to Nucleus
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, cfg.NucleusURL)
	if err != nil {
		logger.Error("failed to connect to nucleus", "err", err)
		os.Exit(1)
	}

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

	// Auth service
	authSvc := auth.NewAuthService(db, cfg.JWTSecret, logger)
	if err := authSvc.EnsureAdmin(ctx, cfg.AdminUser, cfg.AdminPassword); err != nil {
		logger.Error("failed to ensure admin user", "err", err)
		os.Exit(1)
	}

	// Site service
	siteSvc := sites.NewSiteService(db)

	// Ingestion buffer
	buf := ingest.NewBuffer(db, cfg.BufferSize, cfg.FlushSize, cfg.FlushInterval, logger)

	// Stats service
	statsSvc := query.NewStatsService(db)

	// Live event stream service
	liveSvc := live.NewLiveService(db, logger)

	// Share link service
	shareSvc := share.NewShareService(db)

	// Export service
	exportSvc := export.NewExportService(db)

	// Error tracking services
	issueSvc := obserrors.NewIssueService(db)
	searchSvc := obserrors.NewSearchService(db)
	errorHandler := obserrors.NewErrorHandler(db, issueSvc, searchSvc)

	// Tracing services
	traceIngest := tracing.NewIngestService(db)
	traceQuery := tracing.NewQueryService(db)

	// Platform services
	userSvc := platform.NewUserService(db)
	alertSvc := platform.NewAlertService(db, logger)
	webhookSvc := platform.NewWebhookService(db, logger)

	// Feature expansion services
	logSvc := logs.NewLogService(db)
	uptimeSvc := monitoring.NewUptimeService(db, logger)
	cronSvc := monitoring.NewCronService(db, logger)
	linkSvc := tracking.NewLinkService(db)
	srcmapSvc := sourcemaps.NewSourceMapService(db)
	dashSvc := dashboards.NewDashboardService(db)
	replaySvc := replays.NewReplayService(db)

	// Background jobs: rollups + retention
	rollups := jobs.NewRollupService(db, logger)
	retention := jobs.NewRetentionService(db, logger, cfg.RawRetentionDays, cfg.HourlyRetentionDays)
	scheduler := jobs.NewScheduler(rollups, retention, logger)

	// Build app
	app := neutron.New(
		neutron.WithLogger(logger),
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
		neutron.WithMiddleware(ingest.RequestInfoMiddleware),
		neutron.WithNucleusChecker(db),
		neutron.WithOpenAPIInfo("Teploy Observe", "0.1.0"),
	)

	r := app.Router()

	// --- Auth API (public) ---
	neutron.Post(r, "/api/v1/auth/login", loginHandler(authSvc),
		neutron.WithTags("auth"),
		neutron.WithSummary("Login and receive JWT token"),
	)

	// --- Ingestion API (API key auth, wildcard CORS, rate limited) ---
	rateLimiter := ingest.NewRateLimiter(100, time.Second, 200) // 100 req/s per IP, burst 200
	ingestCORS := neutron.CORS(neutron.CORSOptions{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"POST", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "X-API-Key"},
		MaxAge:       86400,
	})
	ingestGroup := r.Group("/api/v1", ingestCORS, rateLimiter.Middleware, auth.APIKeyAuthMiddleware(authSvc, cfg.SiteID))
	neutron.Post(ingestGroup, "/events", ingest.Handler(buf, cfg.SessionSalt),
		neutron.WithTags("ingest"),
		neutron.WithSummary("Ingest analytics event"),
	)
	neutron.Post(ingestGroup, "/events/batch", ingest.BatchHandler(buf, cfg.SessionSalt),
		neutron.WithTags("ingest"),
		neutron.WithSummary("Ingest batch of analytics events"),
	)

	// --- Error Ingestion (API key auth, wildcard CORS) ---
	neutron.Post(ingestGroup, "/errors", errorIngestHandler(errorHandler),
		neutron.WithTags("errors"),
		neutron.WithSummary("Ingest error event"),
	)

	// --- OTLP Trace Ingestion (API key auth) ---
	neutron.Post(ingestGroup, "/v1/traces", otlpTraceHandler(traceIngest),
		neutron.WithTags("traces"),
		neutron.WithSummary("Ingest OTLP traces"),
	)

	// --- Stats API (JWT auth) ---
	jwtMW := auth.JWTAuthMiddleware(authSvc)
	query.RegisterRoutes(r, statsSvc, jwtMW)

	// --- Live event stream (JWT auth, registered on root router to avoid group prefix bug) ---
	r.Handle("GET /api/v1/stats/live", jwtMW(liveSvc.Handler()))

	// --- Data export (JWT auth) ---
	r.Handle("GET /api/v1/export", jwtMW(exportSvc.Handler()))

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
	neutron.Get(issueGroup, "/{issue_id}/events", issueEventsHandler(issueSvc),
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
	neutron.Get(platformGroup, "/users", listUsersHandler(userSvc),
		neutron.WithTags("platform"), neutron.WithSummary("List users"))
	neutron.Post(platformGroup, "/users", createUserHandler(userSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Create/invite user"))
	neutron.Post(platformGroup, "/users/{user_id}/role", updateUserRoleHandler(userSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Update user role"))
	neutron.Get(platformGroup, "/alerts/rules", listAlertRulesHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("List alert rules"))
	neutron.Post(platformGroup, "/alerts/rules", createAlertRuleHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Create alert rule"))
	neutron.Delete(platformGroup, "/alerts/rules/{rule_id}", deleteAlertRuleHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Delete alert rule"))
	neutron.Get(platformGroup, "/alerts/history", alertHistoryHandler(alertSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Alert history"))
	neutron.Get(platformGroup, "/webhooks", listWebhooksHandler(webhookSvc),
		neutron.WithTags("platform"), neutron.WithSummary("List webhooks"))
	neutron.Post(platformGroup, "/webhooks", createWebhookHandler(webhookSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Create webhook"))
	neutron.Delete(platformGroup, "/webhooks/{webhook_id}", deleteWebhookHandler(webhookSvc),
		neutron.WithTags("platform"), neutron.WithSummary("Delete webhook"))

	// --- Share link management API (JWT auth) ---
	shareGroup := r.Group("/api/v1", jwtMW)
	neutron.Post(shareGroup, "/sites/{site_id}/share", createShareHandler(shareSvc),
		neutron.WithTags("share"),
		neutron.WithSummary("Create a share link for a site"),
	)
	neutron.Get(shareGroup, "/sites/{site_id}/share", listShareHandler(shareSvc),
		neutron.WithTags("share"),
		neutron.WithSummary("List share links for a site"),
	)
	neutron.Delete(shareGroup, "/share/{token}", revokeShareHandler(shareSvc),
		neutron.WithTags("share"),
		neutron.WithSummary("Revoke a share link"),
	)

	// --- Site management API (JWT auth) ---
	siteGroup := r.Group("/api/v1", jwtMW)
	neutron.Get(siteGroup, "/sites", listSitesHandler(siteSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("List all sites"),
	)
	neutron.Post(siteGroup, "/sites", createSiteHandler(siteSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Create a new site"),
	)
	neutron.Delete(siteGroup, "/sites/{site_id}", deleteSiteHandler(siteSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Delete a site"),
	)
	neutron.Post(siteGroup, "/sites/{site_id}/keys", createAPIKeyHandler(authSvc),
		neutron.WithTags("sites"),
		neutron.WithSummary("Generate API key for a site"),
	)

	// --- Log ingestion (API key auth) ---
	neutron.Post(ingestGroup, "/logs", logIngestHandler(logSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Ingest log entry"))

	// --- Replay ingestion (API key auth) ---
	neutron.Post(ingestGroup, "/replays", replayIngestHandler(replaySvc),
		neutron.WithTags("replays"), neutron.WithSummary("Ingest session replay events"))

	// --- Source maps (JWT auth) ---
	r.Handle("POST /api/v1/sourcemaps/upload", jwtMW(srcmapUploadHandler(srcmapSvc)))

	// --- Log query API (JWT auth) ---
	logGroup := r.Group("/api/v1/logs", jwtMW)
	neutron.Get(logGroup, "/search", logSearchHandler(logSvc),
		neutron.WithTags("logs"), neutron.WithSummary("Search logs"))

	// --- Goals API (JWT auth) ---
	goalGroup := r.Group("/api/v1/goals", jwtMW)
	neutron.Get(goalGroup, "", listGoalsHandler(statsSvc),
		neutron.WithTags("goals"), neutron.WithSummary("List goals with conversions"))
	neutron.Post(goalGroup, "", createGoalHandler(statsSvc),
		neutron.WithTags("goals"), neutron.WithSummary("Create goal"))

	// --- Uptime monitors (JWT auth) ---
	uptimeGroup := r.Group("/api/v1/monitors", jwtMW)
	neutron.Get(uptimeGroup, "", listMonitorsHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("List uptime monitors"))
	neutron.Post(uptimeGroup, "", createMonitorHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("Create uptime monitor"))
	neutron.Get(uptimeGroup, "/{monitor_id}/results", monitorResultsHandler(uptimeSvc),
		neutron.WithTags("monitors"), neutron.WithSummary("Get monitor results"))

	// --- Cron monitors (JWT auth + public checkin) ---
	cronGroup := r.Group("/api/v1/crons", jwtMW)
	neutron.Get(cronGroup, "", listCronsHandler(cronSvc),
		neutron.WithTags("crons"), neutron.WithSummary("List cron monitors"))
	neutron.Post(cronGroup, "", createCronHandler(cronSvc),
		neutron.WithTags("crons"), neutron.WithSummary("Create cron monitor"))
	// Public checkin endpoint (no auth)
	r.HandleFunc("POST /api/v1/checkin/{slug}", cronCheckinHandler(cronSvc))
	r.HandleFunc("GET /api/v1/checkin/{slug}", cronCheckinHandler(cronSvc))

	// --- Dashboards (JWT auth) ---
	dashGroup := r.Group("/api/v1/dashboards", jwtMW)
	neutron.Get(dashGroup, "", listDashboardsHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("List dashboards"))
	neutron.Post(dashGroup, "", createDashboardHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Create dashboard"))
	neutron.Get(dashGroup, "/{dashboard_id}", getDashboardHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Get dashboard with panels"))
	neutron.Post(dashGroup, "/{dashboard_id}/panels", addPanelHandler(dashSvc),
		neutron.WithTags("dashboards"), neutron.WithSummary("Add panel to dashboard"))

	// --- Replays (JWT auth) ---
	replayGroup := r.Group("/api/v1/replays", jwtMW)
	neutron.Get(replayGroup, "", listReplaysHandler(replaySvc),
		neutron.WithTags("replays"), neutron.WithSummary("List session replays"))
	neutron.Get(replayGroup, "/{replay_id}", getReplayHandler(replaySvc),
		neutron.WithTags("replays"), neutron.WithSummary("Get replay events"))

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

	// Dashboard UI (embedded static files)
	uiSub, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		logger.Error("failed to load ui assets", "err", err)
		os.Exit(1)
	}
	r.Handle("GET /assets/", http.FileServer(http.FS(uiSub)))
	r.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		data, err := fs.ReadFile(uiSub, "index.html")
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// --- Public share dashboard ---
	r.HandleFunc("GET /share/{token}", shareViewHandler(shareSvc, uiSub))

	// Start background services directly (lifecycle hooks may not fire on all platforms)
	buf.Start()
	scheduler.Start()

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

	logger.Info("starting observe", "addr", cfg.Addr)
	if err := app.Run(cfg.Addr); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
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

// --- Site handlers ---

type listSitesResponse struct {
	Sites []sites.Site `json:"sites"`
}

func listSitesHandler(siteSvc *sites.SiteService) neutron.HandlerFunc[neutron.Empty, listSitesResponse] {
	return func(ctx context.Context, _ neutron.Empty) (listSitesResponse, error) {
		list, err := siteSvc.List(ctx)
		if err != nil {
			return listSitesResponse{}, err
		}
		return listSitesResponse{Sites: list}, nil
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

// --- Share handlers ---

type createShareInput struct {
	SiteID string `path:"site_id"`
}

func createShareHandler(shareSvc *share.ShareService) neutron.HandlerFunc[createShareInput, share.ShareLink] {
	return func(ctx context.Context, input createShareInput) (share.ShareLink, error) {
		if input.SiteID == "" {
			return share.ShareLink{}, neutron.ErrBadRequest("site_id is required")
		}
		return shareSvc.Create(ctx, input.SiteID)
	}
}

type listShareInput struct {
	SiteID string `path:"site_id"`
}

type listShareResponse struct {
	Links []share.ShareLink `json:"links"`
}

func listShareHandler(shareSvc *share.ShareService) neutron.HandlerFunc[listShareInput, listShareResponse] {
	return func(ctx context.Context, input listShareInput) (listShareResponse, error) {
		if input.SiteID == "" {
			return listShareResponse{}, neutron.ErrBadRequest("site_id is required")
		}
		links, err := shareSvc.List(ctx, input.SiteID)
		if err != nil {
			return listShareResponse{}, err
		}
		return listShareResponse{Links: links}, nil
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

func errorIngestHandler(h *obserrors.ErrorHandler) neutron.HandlerFunc[obserrors.ErrorInput, obserrors.ErrorResponse] {
	return func(ctx context.Context, input obserrors.ErrorInput) (obserrors.ErrorResponse, error) {
		siteID := input.SiteID
		if siteID == "" {
			siteID = ingest.SiteIDFromContext(ctx)
		}
		if siteID == "" {
			return obserrors.ErrorResponse{}, neutron.ErrBadRequest("missing site_id")
		}
		input.SiteID = siteID
		return h.Handle(ctx, input)
	}
}

// --- Issue management handlers ---

type listIssuesInput struct {
	SiteID string `query:"site_id"`
	Status string `query:"status"`
	Limit  int    `query:"limit"`
}

type listIssuesResponse struct {
	Issues []obserrors.Issue `json:"issues"`
}

func listIssuesHandler(svc *obserrors.IssueService) neutron.HandlerFunc[listIssuesInput, listIssuesResponse] {
	return func(ctx context.Context, input listIssuesInput) (listIssuesResponse, error) {
		if input.SiteID == "" {
			return listIssuesResponse{}, neutron.ErrBadRequest("site_id required")
		}
		issues, err := svc.ListIssues(ctx, input.SiteID, input.Status, input.Limit)
		if err != nil {
			return listIssuesResponse{}, err
		}
		if issues == nil {
			issues = []obserrors.Issue{}
		}
		return listIssuesResponse{Issues: issues}, nil
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

type issueEventsResponse struct {
	Events []obserrors.ErrorEvent `json:"events"`
}

func issueEventsHandler(svc *obserrors.IssueService) neutron.HandlerFunc[issueEventsInput, issueEventsResponse] {
	return func(ctx context.Context, input issueEventsInput) (issueEventsResponse, error) {
		if input.SiteID == "" || input.IssueID == "" {
			return issueEventsResponse{}, neutron.ErrBadRequest("site_id and issue_id required")
		}
		events, err := svc.LatestEvents(ctx, input.IssueID, input.SiteID, input.Limit)
		if err != nil {
			return issueEventsResponse{}, err
		}
		if events == nil {
			events = []obserrors.ErrorEvent{}
		}
		return issueEventsResponse{Events: events}, nil
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
}

func logSearchHandler(svc *logs.LogService) neutron.HandlerFunc[logSearchInput, []logs.Log] {
	return func(ctx context.Context, input logSearchInput) ([]logs.Log, error) {
		from, to := parseTimeRange(input.From, input.To)
		return svc.SearchLogs(ctx, input.SiteID, from, to, input.Level, input.Service, input.Query, input.Limit)
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
		return svc.GoalConversions(ctx, input.SiteID, from, to)
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
		return svc.ListMonitors(ctx, input.SiteID)
	}
}

type createMonitorInput struct {
	SiteID         string `json:"site_id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	IntervalSecs   string `json:"interval_secs"`
	ExpectedStatus string `json:"expected_status"`
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
		return svc.ListResults(ctx, input.MonitorID, input.Limit)
	}
}

// --- Cron monitor handlers ---

type listCronsInput struct {
	SiteID string `query:"site_id"`
}

func listCronsHandler(svc *monitoring.CronService) neutron.HandlerFunc[listCronsInput, []monitoring.CronMonitor] {
	return func(ctx context.Context, input listCronsInput) ([]monitoring.CronMonitor, error) {
		return svc.ListCrons(ctx, input.SiteID)
	}
}

type createCronInput struct {
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	Schedule    string `json:"schedule"`
	GracePeriod string `json:"grace_period"`
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

func cronCheckinHandler(svc *monitoring.CronService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if slug == "" {
			http.Error(w, "missing slug", http.StatusBadRequest)
			return
		}
		if err := svc.RecordCheckin(r.Context(), slug, "", "ok", "0"); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
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
		return svc.List(ctx, input.SiteID)
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
}

func listReplaysHandler(svc *replays.ReplayService) neutron.HandlerFunc[listReplaysInput, []replays.ReplaySession] {
	return func(ctx context.Context, input listReplaysInput) ([]replays.ReplaySession, error) {
		from, to := parseTimeRange(input.From, input.To)
		return svc.ListReplays(ctx, input.SiteID, from, to, input.Limit)
	}
}

type getReplayInput struct {
	ReplayID string `path:"replay_id"`
}

func getReplayHandler(svc *replays.ReplayService) neutron.HandlerFunc[getReplayInput, []replays.ReplayEvent] {
	return func(ctx context.Context, input getReplayInput) ([]replays.ReplayEvent, error) {
		return svc.GetReplayEvents(ctx, input.ReplayID)
	}
}

// --- Link handlers ---

type listLinksInput struct {
	SiteID string `query:"site_id"`
}

func listLinksHandler(svc *tracking.LinkService) neutron.HandlerFunc[listLinksInput, []tracking.TrackedLink] {
	return func(ctx context.Context, input listLinksInput) ([]tracking.TrackedLink, error) {
		return svc.ListLinks(ctx, "default", input.SiteID)
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
		data := make([]byte, 10*1024*1024) // max 10MB
		n, _ := file.Read(data)
		data = data[:n]

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
type listUsersResponse struct {
	Users []platform.User `json:"users"`
}

func listUsersHandler(svc *platform.UserService) neutron.HandlerFunc[listUsersInput, listUsersResponse] {
	return func(ctx context.Context, _ listUsersInput) (listUsersResponse, error) {
		users, err := svc.List(ctx)
		if err != nil {
			return listUsersResponse{}, err
		}
		if users == nil {
			users = []platform.User{}
		}
		return listUsersResponse{Users: users}, nil
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
		return svc.ListRules(ctx, input.SiteID)
	}
}

type createAlertRuleInput struct {
	SiteID        string `json:"site_id"`
	Name          string `json:"name"`
	Metric        string `json:"metric"`
	Operator      string `json:"operator"`
	Threshold     string `json:"threshold"`
	WindowMinutes string `json:"window_minutes"`
	Cooldown      string `json:"cooldown"`
}

func createAlertRuleHandler(svc *platform.AlertService) neutron.HandlerFunc[createAlertRuleInput, platform.AlertRule] {
	return func(ctx context.Context, input createAlertRuleInput) (platform.AlertRule, error) {
		if input.SiteID == "" || input.Name == "" || input.Metric == "" {
			return platform.AlertRule{}, neutron.ErrBadRequest("site_id, name, and metric required")
		}
		rule := platform.AlertRule{
			SiteID: input.SiteID, Name: input.Name, Metric: input.Metric,
			Operator: input.Operator, Threshold: input.Threshold,
			WindowMinutes: input.WindowMinutes, Cooldown: input.Cooldown, Enabled: "true",
		}
		if rule.Operator == "" {
			rule.Operator = "gt"
		}
		if rule.WindowMinutes == "" {
			rule.WindowMinutes = "5"
		}
		if rule.Cooldown == "" {
			rule.Cooldown = "300"
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

type alertHistoryInput struct {
	SiteID string `query:"site_id"`
	Limit  int    `query:"limit"`
}

func alertHistoryHandler(svc *platform.AlertService) neutron.HandlerFunc[alertHistoryInput, []platform.AlertHistoryEntry] {
	return func(ctx context.Context, input alertHistoryInput) ([]platform.AlertHistoryEntry, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		return svc.ListHistory(ctx, input.SiteID, input.Limit)
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
		return svc.List(ctx, input.SiteID)
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
		return svc.ListServices(ctx, input.SiteID, from, to)
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
		return svc.ListOperations(ctx, input.SiteID, input.Service, from, to)
	}
}

type searchTracesInput struct {
	SiteID      string `query:"site_id"`
	From        string `query:"from"`
	To          string `query:"to"`
	Service     string `query:"service"`
	Operation   string `query:"operation"`
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
		return svc.SearchTraces(ctx, input.SiteID, from, to, input.Service, input.Operation, input.Status, input.MinDuration, input.MaxDuration, input.Limit)
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
		return svc.GetTrace(ctx, input.TraceID, input.SiteID)
	}
}

type traceErrorsInput struct {
	TraceID string `path:"trace_id"`
	SiteID  string `query:"site_id"`
}

type traceErrorsResponse struct {
	Errors []tracing.TraceErrorHit `json:"errors"`
}

func traceErrorsHandler(svc *tracing.QueryService) neutron.HandlerFunc[traceErrorsInput, traceErrorsResponse] {
	return func(ctx context.Context, input traceErrorsInput) (traceErrorsResponse, error) {
		if input.SiteID == "" || input.TraceID == "" {
			return traceErrorsResponse{}, neutron.ErrBadRequest("site_id and trace_id required")
		}
		hits, err := svc.TraceErrors(ctx, input.TraceID, input.SiteID)
		if err != nil {
			return traceErrorsResponse{}, err
		}
		if hits == nil {
			hits = []tracing.TraceErrorHit{}
		}
		return traceErrorsResponse{Errors: hits}, nil
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
		return svc.ServiceDependencies(ctx, input.SiteID, from, to)
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

type searchIssuesResponse struct {
	Issues []obserrors.Issue `json:"issues"`
}

func searchIssuesHandler(svc *obserrors.SearchService) neutron.HandlerFunc[searchIssuesInput, searchIssuesResponse] {
	return func(ctx context.Context, input searchIssuesInput) (searchIssuesResponse, error) {
		if input.SiteID == "" || input.Query == "" {
			return searchIssuesResponse{}, neutron.ErrBadRequest("site_id and q required")
		}
		issues, err := svc.SearchIssues(ctx, input.SiteID, input.Query, input.Limit)
		if err != nil {
			return searchIssuesResponse{}, err
		}
		if issues == nil {
			issues = []obserrors.Issue{}
		}
		return searchIssuesResponse{Issues: issues}, nil
	}
}

// shareViewHandler resolves a share token and serves the dashboard UI with
// the site_id injected as a query parameter redirect.
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

		// Inject site_id as a meta tag the frontend can read.
		// Insert before </head>.
		inject := fmt.Sprintf(`<meta name="observe-site-id" content="%s"><meta name="observe-shared" content="true">`, siteID)
		html := strings.Replace(string(data), "</head>", inject+"</head>", 1)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	}
}
