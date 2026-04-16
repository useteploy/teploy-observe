package reports

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

type ReportService struct {
	db     *nucleus.Client
	logger *slog.Logger
}

func NewReportService(db *nucleus.Client, logger *slog.Logger) *ReportService {
	return &ReportService{db: db, logger: logger}
}

type ReportSchedule struct {
	ScheduleID string `json:"schedule_id" db:"schedule_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	Name       string `json:"name" db:"name"`
	Frequency  string `json:"frequency" db:"frequency"` // daily, weekly
	Recipients string `json:"recipients" db:"recipients"`
	Enabled    string `json:"enabled" db:"enabled"`
	LastSent   string `json:"last_sent" db:"last_sent"`
	CreatedAt  string `json:"created_at" db:"created_at"`
	Version    string `json:"-" db:"version"`
}

func (s *ReportService) Create(ctx context.Context, siteID, name, frequency, recipients string) (*ReportSchedule, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	if frequency == "" {
		frequency = "weekly"
	}
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO report_schedules (schedule_id, tenant_id, site_id, name, frequency, recipients, enabled, last_sent, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, 'true', '0', $6, $7)`,
		id, siteID, name, frequency, recipients, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create report: %w", err)
	}
	return &ReportSchedule{ScheduleID: id, SiteID: siteID, Name: name, Frequency: frequency, Recipients: recipients, Enabled: "true", CreatedAt: now}, nil
}

func (s *ReportService) List(ctx context.Context, siteID string) ([]ReportSchedule, error) {
	return nucleus.Query[ReportSchedule](ctx, s.db.SQL(),
		`SELECT schedule_id, tenant_id, site_id, name, frequency, recipients, enabled, last_sent, created_at, version
		 FROM report_schedules WHERE site_id = $1 AND enabled = 'true' ORDER BY created_at DESC`, siteID)
}

func (s *ReportService) Delete(ctx context.Context, scheduleID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO report_schedules (schedule_id, tenant_id, site_id, name, frequency, recipients, enabled, last_sent, created_at, version)
		 SELECT schedule_id, tenant_id, site_id, name, frequency, recipients, 'false', last_sent, created_at, $2
		 FROM report_schedules WHERE schedule_id = $1`,
		scheduleID, now)
	return err
}

// ReportData holds the metrics for a report email.
type ReportData struct {
	SiteID     string
	SiteName   string
	Period     string
	Pageviews  string
	Visitors   string
	Sessions   string
	Errors     string
	TopPages   []string
}

// RunScheduled checks all enabled schedules and sends reports that are due.
func (s *ReportService) RunScheduled(ctx context.Context, smtpHost, smtpPort, smtpUser, smtpPass, fromEmail string) {
	schedules, err := nucleus.Query[ReportSchedule](ctx, s.db.SQL(),
		`SELECT schedule_id, tenant_id, site_id, name, frequency, recipients, enabled, last_sent, created_at, version
		 FROM report_schedules WHERE enabled = 'true'`)
	if err != nil {
		s.logger.Error("report scheduler query failed", "err", err)
		return
	}

	now := time.Now().UTC()
	for _, sched := range schedules {
		lastSent, _ := strconv.ParseInt(sched.LastSent, 10, 64)
		lastTime := time.UnixMilli(lastSent)

		var due bool
		switch sched.Frequency {
		case "daily":
			due = now.Sub(lastTime) >= 24*time.Hour
		case "weekly":
			due = now.Sub(lastTime) >= 7*24*time.Hour
		default:
			due = now.Sub(lastTime) >= 7*24*time.Hour
		}

		if !due {
			continue
		}

		data := s.gatherData(ctx, sched)
		html := buildEmailHTML(data)

		if smtpHost != "" && sched.Recipients != "" {
			err := sendEmail(smtpHost, smtpPort, smtpUser, smtpPass, fromEmail, sched.Recipients, "Observe Report: "+data.Period, html)
			if err != nil {
				s.logger.Error("report email failed", "schedule", sched.Name, "err", err)
				continue
			}
		}

		// Update last_sent (best-effort — failure means report may re-send next cycle)
		nowStr := strconv.FormatInt(now.UnixMilli(), 10)
		if _, err := s.db.SQL().Exec(ctx,
			`INSERT INTO report_schedules (schedule_id, tenant_id, site_id, name, frequency, recipients, enabled, last_sent, created_at, version)
			 SELECT schedule_id, tenant_id, site_id, name, frequency, recipients, enabled, $2, created_at, $3
			 FROM report_schedules WHERE schedule_id = $1`,
			sched.ScheduleID, nowStr, nowStr); err != nil {
			s.logger.Error("report last_sent update failed", "schedule", sched.Name, "err", err)
		}

		s.logger.Info("report sent", "schedule", sched.Name, "recipients", sched.Recipients)
	}
}

func (s *ReportService) gatherData(ctx context.Context, sched ReportSchedule) ReportData {
	now := time.Now().UTC()
	var from time.Time
	period := "Last 7 days"
	if sched.Frequency == "daily" {
		from = now.Add(-24 * time.Hour)
		period = "Last 24 hours"
	} else {
		from = now.Add(-7 * 24 * time.Hour)
	}

	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(now.UnixMilli())

	type countRow struct {
		Count string `db:"count"`
	}

	data := ReportData{SiteID: sched.SiteID, SiteName: sched.Name, Period: period}

	// Pageviews
	rows, _ := nucleus.Query[countRow](ctx, s.db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS count FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND event_type = 'pageview'`,
		sched.SiteID, fromMs, toMs)
	if len(rows) > 0 {
		data.Pageviews = rows[0].Count
	}

	// Visitors
	rows, _ = nucleus.Query[countRow](ctx, s.db.SQL(),
		`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		sched.SiteID, fromMs, toMs)
	if len(rows) > 0 {
		data.Visitors = rows[0].Count
	}

	// Errors
	rows, _ = nucleus.Query[countRow](ctx, s.db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS count FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		sched.SiteID, fromMs, toMs)
	if len(rows) > 0 {
		data.Errors = rows[0].Count
	}

	return data
}

func buildEmailHTML(data ReportData) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family:-apple-system,sans-serif;background:#f4f4f5;padding:20px;">
<div style="max-width:600px;margin:0 auto;background:#fff;border-radius:8px;padding:32px;">
<h1 style="font-size:20px;color:#09090b;margin:0 0 4px;">Observe Report</h1>
<p style="color:#71717a;font-size:14px;margin:0 0 24px;">%s</p>
<table style="width:100%%;border-collapse:collapse;">
<tr><td style="padding:12px;border-bottom:1px solid #e4e4e7;color:#71717a;font-size:13px;">Pageviews</td><td style="padding:12px;border-bottom:1px solid #e4e4e7;font-size:20px;font-weight:600;text-align:right;">%s</td></tr>
<tr><td style="padding:12px;border-bottom:1px solid #e4e4e7;color:#71717a;font-size:13px;">Visitors</td><td style="padding:12px;border-bottom:1px solid #e4e4e7;font-size:20px;font-weight:600;text-align:right;">%s</td></tr>
<tr><td style="padding:12px;color:#71717a;font-size:13px;">Errors</td><td style="padding:12px;font-size:20px;font-weight:600;text-align:right;color:%s;">%s</td></tr>
</table>
</div>
</body></html>`, data.Period, data.Pageviews, data.Visitors,
		func() string { if data.Errors != "0" { return "#ef4444" }; return "#09090b" }(),
		data.Errors)
}

func sendEmail(host, port, user, pass, from, to, subject, html string) error {
	if port == "" {
		port = "587"
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s",
		from, to, subject, html)
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}
	return smtp.SendMail(host+":"+port, auth, from, strings.Split(to, ","), []byte(msg))
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
