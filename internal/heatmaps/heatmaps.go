// Package heatmaps aggregates click coordinates from session replay events
// into URL-keyed buckets so the UI overlay can render a click density map
// on top of an existing snapshot frame.
//
// Coordinates already flow into replay_events via observe-replay.js
// (cmd/observe/tracker/observe-replay.js:79). On every replay-ingest batch
// we also write per-bucket rollups here so the heatmap query is a single
// SUM(count) GROUP BY (x,y) instead of re-bucketing every replay event.
package heatmaps

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Bucket sizes in pixels. Documented in
// cmd/observe/migrations/015_click_heatmaps.up.sql so the schema and the
// code agree on a single source of truth.
const (
	bucketX = 10
	bucketY = 10
	// Viewport width is bucketed at 100px so a 1280px laptop and a 1366px
	// laptop fall into adjacent buckets — the overlay can choose to merge
	// neighbouring vw buckets when a user wants a unified view.
	bucketVW = 100
)

// ClickEvent is the minimal slice of a replay click event that the
// aggregator needs. Decoupled from replays.IngestInput so the package
// can be unit-tested without importing the replays package.
type ClickEvent struct {
	X             int
	Y             int
	ViewportWidth int // 0 = unknown
}

// Click is the point returned by Query for the UI overlay.
//
// X and Y are the centre of the bucket in pixel coordinates so the overlay
// can render directly without caring about the bucket size.
type Click struct {
	X     int `json:"x"`
	Y     int `json:"y"`
	Count int `json:"count"`
}

// Service writes per-(url, position) click rollups and serves them back
// for the heatmap overlay.
type Service struct {
	db *nucleus.Client
}

// NewService constructs a heatmaps.Service.
func NewService(db *nucleus.Client) *Service {
	return &Service{db: db}
}

// bucketKey identifies a single rollup row: (url, x_bucket, y_bucket, vw_bucket).
type bucketKey struct {
	URL string
	X   int64
	Y   int64
	VW  int64
}

// aggregate folds a slice of click events into one count per bucket.
// Pure / side-effect-free so the unit test can exercise it without a DB.
func aggregate(url string, events []ClickEvent) map[bucketKey]int64 {
	out := make(map[bucketKey]int64)
	for _, ev := range events {
		// Negative coords (off-screen / synthetic) get clamped to 0 rather
		// than producing surprising negative bucket ids.
		x := ev.X
		if x < 0 {
			x = 0
		}
		y := ev.Y
		if y < 0 {
			y = 0
		}
		vw := ev.ViewportWidth
		if vw < 0 {
			vw = 0
		}
		key := bucketKey{
			URL: url,
			X:   int64(math.Floor(float64(x) / bucketX)),
			Y:   int64(math.Floor(float64(y) / bucketY)),
			VW:  int64(math.Floor(float64(vw) / bucketVW)),
		}
		out[key]++
	}
	return out
}

// Aggregate writes one rollup row per unique bucket in `events` for the
// given (siteID, url). Called from the replay ingest path so heatmap
// rollups stay in lock-step with the underlying replay_events.
//
// Each call writes new rows (replacing_mergetree with version=now ms) —
// the read query SUMs across rows in the requested time window.
//
// A single failure inside the loop is logged via the returned error and
// aborts the rest of the batch; the caller (replays.Ingest) treats heatmap
// write failures as best-effort so a transient DB blip never breaks the
// underlying replay-event ingest.
func (s *Service) Aggregate(ctx context.Context, siteID, url string, events []ClickEvent) error {
	if len(events) == 0 {
		return nil
	}
	buckets := aggregate(url, events)
	if len(buckets) == 0 {
		return nil
	}

	sql := s.db.SQL()
	now := time.Now().UTC().UnixMilli()
	version := strconv.FormatInt(now, 10)

	for key, count := range buckets {
		_, err := sql.Exec(ctx,
			`INSERT INTO click_heatmaps (
				tenant_id, site_id, url, x_bucket, y_bucket, vw_bucket,
				count, created_at, version
			) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8)`,
			siteID, key.URL,
			dbutil.IntParam(key.X),
			dbutil.IntParam(key.Y),
			dbutil.IntParam(key.VW),
			strconv.FormatInt(count, 10),
			dbutil.IntParam(now),
			version,
		)
		if err != nil {
			return fmt.Errorf("insert click_heatmaps row: %w", err)
		}
	}
	return nil
}

// row is the on-the-wire shape for a SELECT — strings for the numerics so
// nucleus' pgwire layer (which advertises BIGINT as TEXT, see dogfood
// finding #6) doesn't trip the typed scanner.
type row struct {
	XBucket string `json:"x_bucket"`
	YBucket string `json:"y_bucket"`
	Count   string `json:"count"`
}

// Query returns aggregated clicks for the given site + url, summed across
// all rollups whose `created_at` falls in [fromMs, toMs).
//
// Emitted X/Y are the centre of each bucket in pixel coordinates so the
// canvas overlay can render without needing to know the bucket size.
//
// Rows are de-duplicated/SUMmed in Go because nucleus rejects nested SUM
// inside CASE-WHEN (dogfood finding #15) — same pattern as ListServices.
func (s *Service) Query(ctx context.Context, siteID, url string, fromMs, toMs int64) ([]Click, error) {
	rows, err := nucleus.Query[row](ctx, s.db.SQL(),
		`SELECT
			CAST(x_bucket AS TEXT) AS x_bucket,
			CAST(y_bucket AS TEXT) AS y_bucket,
			count
		 FROM click_heatmaps
		 WHERE site_id = $1 AND url = $2
		   AND CAST(created_at AS BIGINT) >= CAST($3 AS BIGINT)
		   AND CAST(created_at AS BIGINT) <  CAST($4 AS BIGINT)`,
		siteID, url, dbutil.IntParam(fromMs), dbutil.IntParam(toMs),
	)
	if err != nil {
		return nil, err
	}

	type xy struct{ x, y int64 }
	totals := make(map[xy]int64, len(rows))
	for _, r := range rows {
		x, _ := strconv.ParseInt(r.XBucket, 10, 64)
		y, _ := strconv.ParseInt(r.YBucket, 10, 64)
		c, _ := strconv.ParseInt(r.Count, 10, 64)
		totals[xy{x, y}] += c
	}

	out := make([]Click, 0, len(totals))
	for k, c := range totals {
		out = append(out, Click{
			X:     int(k.x*bucketX + bucketX/2),
			Y:     int(k.y*bucketY + bucketY/2),
			Count: int(c),
		})
	}
	return out, nil
}

// ExtractClicks plucks click events out of an arbitrary replay-event slice
// shaped like the one in replays.IngestInput. The replay tracker already
// validates that click data carries x/y, but defensive parsing keeps a
// malformed event from killing the whole rollup write.
//
// `data` is the same `any` value the replay SDK sent: typically
// map[string]any{"x":N, "y":N, "target":"..."}.
func ExtractClicks(events []RawEvent) []ClickEvent {
	out := make([]ClickEvent, 0, len(events))
	for _, ev := range events {
		if ev.Type != "click" {
			continue
		}
		x, y, ok := xyFromAny(ev.Data)
		if !ok {
			continue
		}
		out = append(out, ClickEvent{X: x, Y: y, ViewportWidth: ev.ViewportWidth})
	}
	return out
}

// RawEvent is the minimal shape ExtractClicks needs. It is structurally
// compatible with the replay SDK's per-event struct, but lives here so
// the heatmaps package has zero replay dependencies.
type RawEvent struct {
	Type          string
	Data          any
	ViewportWidth int
}

func xyFromAny(data any) (int, int, bool) {
	switch d := data.(type) {
	case map[string]any:
		x, okx := numericField(d, "x")
		y, oky := numericField(d, "y")
		if okx && oky {
			return x, y, true
		}
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(d, &m) == nil {
			return xyFromAny(m)
		}
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(d), &m) == nil {
			return xyFromAny(m)
		}
	}
	return 0, 0, false
}

func numericField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true
		}
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	}
	return 0, false
}
