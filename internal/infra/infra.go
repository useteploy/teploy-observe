package infra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

type InfraService struct {
	db *nucleus.Client
}

func NewInfraService(db *nucleus.Client) *InfraService {
	return &InfraService{db: db}
}

// HostMetric represents a single host metric snapshot.
type HostMetric struct {
	MetricID      string  `json:"metric_id" db:"metric_id"`
	TenantID      string  `json:"-" db:"tenant_id"`
	SiteID        string  `json:"site_id" db:"site_id"`
	Hostname      string  `json:"hostname"`
	Timestamp     int64   `json:"timestamp"`
	CPUPercent    float64 `json:"cpu_percent" db:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent" db:"memory_percent"`
	MemoryUsedMB  int64   `json:"memory_used_mb" db:"memory_used_mb"`
	MemoryTotalMB int64   `json:"memory_total_mb" db:"memory_total_mb"`
	DiskPercent   float64 `json:"disk_percent" db:"disk_percent"`
	DiskUsedGB    int64   `json:"disk_used_gb" db:"disk_used_gb"`
	DiskTotalGB   int64   `json:"disk_total_gb" db:"disk_total_gb"`
	NetRXBytes    int64   `json:"net_rx_bytes" db:"net_rx_bytes"`
	NetTXBytes    int64   `json:"net_tx_bytes" db:"net_tx_bytes"`
	Load1m        float64 `json:"load_1m" db:"load_1m"`
	Load5m        float64 `json:"load_5m" db:"load_5m"`
	Load15m       float64 `json:"load_15m" db:"load_15m"`
}

// MetricInput is the agent's report payload.
type MetricInput struct {
	SiteID        string  `json:"site_id"`
	Hostname      string  `json:"hostname"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	MemoryUsedMB  int     `json:"memory_used_mb"`
	MemoryTotalMB int     `json:"memory_total_mb"`
	DiskPercent   float64 `json:"disk_percent"`
	DiskUsedGB    int     `json:"disk_used_gb"`
	DiskTotalGB   int     `json:"disk_total_gb"`
	NetRXBytes    int64   `json:"net_rx_bytes"`
	NetTXBytes    int64   `json:"net_tx_bytes"`
	Load1m        float64 `json:"load_1m"`
	Load5m        float64 `json:"load_5m"`
	Load15m       float64 `json:"load_15m"`
}

func (s *InfraService) Report(ctx context.Context, input MetricInput) error {
	id := genID()
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO host_metrics (metric_id, tenant_id, site_id, hostname, timestamp,
			cpu_percent, memory_percent, memory_used_mb, memory_total_mb,
			disk_percent, disk_used_gb, disk_total_gb,
			net_rx_bytes, net_tx_bytes, load_1m, load_5m, load_15m)
		 VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		id, input.SiteID, input.Hostname, now,
		fmt.Sprintf("%.1f", input.CPUPercent), fmt.Sprintf("%.1f", input.MemoryPercent),
		fmt.Sprintf("%d", input.MemoryUsedMB), fmt.Sprintf("%d", input.MemoryTotalMB),
		fmt.Sprintf("%.1f", input.DiskPercent), fmt.Sprintf("%d", input.DiskUsedGB), fmt.Sprintf("%d", input.DiskTotalGB),
		fmt.Sprintf("%d", input.NetRXBytes), fmt.Sprintf("%d", input.NetTXBytes),
		fmt.Sprintf("%.2f", input.Load1m), fmt.Sprintf("%.2f", input.Load5m), fmt.Sprintf("%.2f", input.Load15m),
	)
	return err
}

// HostSummary is a host with its latest metrics.
type HostSummary struct {
	Hostname      string  `json:"hostname"`
	CPUPercent    float64 `json:"cpu_percent" db:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent" db:"memory_percent"`
	DiskPercent   float64 `json:"disk_percent" db:"disk_percent"`
	Load1m        float64 `json:"load_1m" db:"load_1m"`
	LastSeen      string  `json:"last_seen" db:"last_seen"`
}

func (s *InfraService) ListHosts(ctx context.Context, siteID string) ([]HostSummary, error) {
	// One CURRENT row per host. host_metrics is append-only (one row per report
	// tick), so an unaggregated SELECT returned every historical row per host —
	// the UI showed dozens of duplicate, stale entries. argMax(col, timestamp)
	// collapses to the newest sample per hostname.
	return nucleus.Query[HostSummary](ctx, s.db.SQL(),
		`SELECT hostname,
			argMax(cpu_percent, timestamp)    AS cpu_percent,
			argMax(memory_percent, timestamp) AS memory_percent,
			argMax(disk_percent, timestamp)   AS disk_percent,
			argMax(load_1m, timestamp)        AS load_1m,
			CAST(max(timestamp) AS TEXT)      AS last_seen
		 FROM host_metrics WHERE site_id = $1
		 GROUP BY hostname
		 ORDER BY hostname`,
		siteID,
	)
}

func (s *InfraService) HostHistory(ctx context.Context, siteID, hostname string, from, to time.Time, limit int) ([]HostMetric, error) {
	if limit <= 0 {
		limit = 100
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	return nucleus.Query[HostMetric](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT metric_id, tenant_id, site_id, hostname, timestamp,
			cpu_percent, memory_percent, memory_used_mb, memory_total_mb,
			disk_percent, disk_used_gb, disk_total_gb,
			net_rx_bytes, net_tx_bytes, load_1m, load_5m, load_15m
		 FROM host_metrics WHERE site_id = $1 AND hostname = $2 AND timestamp >= $3 AND timestamp < $4
		 ORDER BY timestamp DESC LIMIT %d`, limit),
		siteID, hostname, fromMs, toMs,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
