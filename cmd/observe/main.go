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

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/auth"
	"github.com/teploy/observe/internal/config"
	"github.com/teploy/observe/internal/ingest"
	"github.com/teploy/observe/internal/jobs"
	"github.com/teploy/observe/internal/live"
	"github.com/teploy/observe/internal/query"
	"github.com/teploy/observe/internal/share"
	"github.com/teploy/observe/internal/sites"
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

	// --- Ingestion API (API key auth, wildcard CORS) ---
	ingestCORS := neutron.CORS(neutron.CORSOptions{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"POST", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "X-API-Key"},
		MaxAge:       86400,
	})
	ingestGroup := r.Group("/api/v1", ingestCORS, auth.APIKeyAuthMiddleware(authSvc, cfg.SiteID))
	neutron.Post(ingestGroup, "/events", ingest.Handler(buf, cfg.SessionSalt),
		neutron.WithTags("ingest"),
		neutron.WithSummary("Ingest analytics event"),
	)
	neutron.Post(ingestGroup, "/events/batch", ingest.BatchHandler(buf, cfg.SessionSalt),
		neutron.WithTags("ingest"),
		neutron.WithSummary("Ingest batch of analytics events"),
	)

	// --- Stats API (JWT auth) ---
	jwtMW := auth.JWTAuthMiddleware(authSvc)
	query.RegisterRoutes(r, statsSvc, jwtMW)

	// --- Live event stream (JWT auth, registered on root router to avoid group prefix bug) ---
	r.Handle("GET /api/v1/stats/live", jwtMW(liveSvc.Handler()))

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

	// Tracker script (served as static JS)
	r.HandleFunc("GET /t/observe.js", serveTracker)

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
