// Package dogfood self-instruments Observe so it can monitor itself.
// On boot it ensures a `_meta` site + API key exist, then exposes HTTP
// middleware that emits one trace span per request plus a panic-recovery
// middleware that ships crashes through the Go SDK back into Observe.
//
// All self-events land under site_id=_meta so they never pollute the
// user's primary site dashboards.
package dogfood

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	observe "github.com/useteploy/teploy-observe/sdk/go"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// MetaSiteID is the dedicated site_id used for self-monitoring data.
const MetaSiteID = "_meta"

// Self is the handle returned by Setup. Use it to wrap router middleware
// and obtain a slog.Handler for application logs.
type Self struct {
	Client *observe.Client
	logger *slog.Logger
}

// Logger returns a slog.Logger that writes to BOTH the wrapped handler
// (typically stderr) and Observe.
func (s *Self) Logger() *slog.Logger {
	if s == nil {
		return slog.Default()
	}
	return s.logger
}

// Close flushes pending spans/logs.
func (s *Self) Close() error {
	if s == nil || s.Client == nil {
		return nil
	}
	return s.Client.Close()
}

// Setup bootstraps the _meta site + API key, builds the SDK client, and
// returns a *Self that wires middleware into Observe's HTTP server.
//
// `endpoint` is the URL Observe binds to (e.g. http://localhost:3000).
// Pass the wrappedSlog handler that Observe normally writes to so logs
// keep flowing to stderr in addition to Observe.
func Setup(ctx context.Context, db *nucleus.Client, authSvc *auth.AuthService, endpoint string, wrappedSlog slog.Handler) (*Self, error) {
	if err := ensureMetaSite(ctx, db); err != nil {
		return nil, fmt.Errorf("dogfood: ensure site: %w", err)
	}
	apiKey, err := ensureMetaAPIKey(ctx, db, authSvc)
	if err != nil {
		return nil, fmt.Errorf("dogfood: ensure api key: %w", err)
	}

	client, err := observe.New(observe.Options{
		Endpoint:    endpoint,
		APIKey:      apiKey,
		SiteID:      MetaSiteID,
		ServiceName: "observe",
		Environment: "self",
	})
	if err != nil {
		return nil, fmt.Errorf("dogfood: client: %w", err)
	}

	logger := slog.New(client.NewSlogHandler(slog.LevelInfo, wrappedSlog))
	return &Self{Client: client, logger: logger}, nil
}

// TraceMiddleware emits one OTLP span per HTTP request. Paths that are part
// of Observe's own ingest pipeline are skipped to prevent self-feedback loops
// (a trace span that itself triggers a trace span ad infinitum).
func (s *Self) TraceMiddleware(next http.Handler) http.Handler {
	if s == nil || s.Client == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldSkip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, span := s.Client.StartSpan(r.Context(), r.Method+" "+r.URL.Path)
		span.SetAttribute("http.method", r.Method)
		span.SetAttribute("http.target", r.URL.Path)
		if ua := r.Header.Get("User-Agent"); ua != "" {
			span.SetAttribute("http.user_agent", ua)
		}

		// Capture status code so the span can be marked as error on 5xx.
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r.WithContext(ctx))

		span.SetAttribute("http.status_code", strconv.Itoa(rec.status))
		if rec.status >= 500 {
			span.SetStatus(2, http.StatusText(rec.status))
		} else if rec.status >= 200 && rec.status < 400 {
			span.SetStatus(1, "")
		}
		span.End()
	})
}

// RecoverMiddleware catches panics, ships them to /api/v1/errors via the
// SDK, and returns a 500 to the client.
func (s *Self) RecoverMiddleware(next http.Handler) http.Handler {
	if s == nil || s.Client == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rv := recover()
			if rv == nil {
				return
			}
			err, ok := rv.(error)
			if !ok {
				err = fmt.Errorf("%v", rv)
			}
			_ = s.Client.CaptureException(err)
			s.logger.Error("panic recovered", "path", r.URL.Path, "err", err, "stack", string(debug.Stack()))
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// shouldSkip returns true for paths that we never trace because tracing them
// would generate the very requests being traced.
func shouldSkip(path string) bool {
	switch {
	case path == "/v1/traces":
		return true
	case strings.HasPrefix(path, "/api/v1/v1/traces"):
		return true
	case strings.HasPrefix(path, "/api/v1/logs"):
		return true
	case strings.HasPrefix(path, "/api/v1/errors"):
		return true
	case strings.HasPrefix(path, "/api/v1/events"):
		return true
	case strings.HasPrefix(path, "/api/v1/replays"):
		return true
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/assets/"):
		return true
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

func ensureMetaSite(ctx context.Context, db *nucleus.Client) error {
	type countRow struct {
		Count int64 `db:"count"`
	}
	rows, err := nucleus.Query[countRow](ctx, db.SQL(),
		"SELECT COUNT(*) AS count FROM sites WHERE site_id = $1", MetaSiteID,
	)
	if err != nil {
		return err
	}
	if len(rows) > 0 && rows[0].Count > 0 {
		return nil
	}
	now := dbutil.IntParam(time.Now().UnixMilli())
	_, err = db.SQL().Exec(ctx,
		`INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt)
		 VALUES ($1, 'default', '', 'Self-monitoring (Observe internal)', $2, $3)`,
		MetaSiteID, now, MetaSiteID+"-salt",
	)
	return err
}

func ensureMetaAPIKey(ctx context.Context, db *nucleus.Client, authSvc *auth.AuthService) (string, error) {
	// Look for an existing dogfood key. We store it in instance_settings under
	// a stable key so subsequent boots reuse the same plaintext key (the auth
	// table only stores the hash and can't return the plaintext).
	const settingKey = "dogfood.meta_api_key"
	type settingRow struct {
		Value string `db:"setting_value"`
	}
	rows, err := nucleus.Query[settingRow](ctx, db.SQL(),
		"SELECT setting_value FROM instance_settings WHERE setting_key = $1", settingKey,
	)
	if err == nil && len(rows) > 0 && rows[0].Value != "" {
		return rows[0].Value, nil
	}
	plaintext, _, err := authSvc.CreateAPIKey(ctx, MetaSiteID, "dogfood")
	if err != nil {
		return "", err
	}
	now := dbutil.IntParam(time.Now().UnixMilli())
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO instance_settings (setting_key, setting_value, updated_at)
		 VALUES ($1, $2, $3)`,
		settingKey, plaintext, now,
	); err != nil {
		// Non-fatal: the key still works for THIS run, but next boot will
		// generate a fresh one (the old one stays valid in api_keys).
		return plaintext, nil
	}
	return plaintext, nil
}
