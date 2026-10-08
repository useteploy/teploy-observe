package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func r5GoalNativeDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		if os.Getenv("OBSERVE_REQUIRE_NUCLEUS") == "1" {
			t.Fatal("required native DSN absent")
		}
		t.Skip("native DSN absent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if !db.IsNucleus() {
		t.Fatal("requires actual Nucleus")
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestR5GoalNativeCountBoundary(t *testing.T) {
	db := r5GoalNativeDB(t)
	s := NewStatsService(db)
	ctx := context.Background()
	for _, v := range []struct {
		expression string
		want       int64
		bad        bool
	}{
		{"0", 0, false}, {"9223372036854775807", math.MaxInt64, false},
		{"NULL", 0, true}, {"''", 0, true}, {"'bad'", 0, true}, {"-1", 0, true}, {"'9223372036854775808'", 0, true},
	} {
		// Native projection -> nullable wire scan -> strict parser; no SQLite model.
		rows, err := s.readGoalCounts(ctx, "SELECT "+v.expression+" AS count")
		var got int64
		if err == nil {
			got, err = strictGoalCount(rows, "count")
		}
		if (err != nil) != v.bad || (!v.bad && got != v.want) {
			t.Fatalf("expression %s: %d %v", v.expression, got, err)
		}
	}
	site := goalSite("r5-empty")
	now := time.Now().UTC()
	g, err := s.CreateGoal(ctx, Goal{SiteID: site, Name: "max", GoalType: "event", GoalValue: "buy", Currency: "USD", ValueMinor: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteGoal(ctx, site, g.GoalID) })
	got, err := s.GoalConversions(ctx, site, now.Add(-time.Second), now.Add(time.Second))
	if err != nil || len(got) != 1 || got[0].Conversions != 0 || got[0].ConversionEvents != 0 || got[0].Visitors != 0 || got[0].TotalValueMinor != 0 || got[0].Rate != 0 {
		t.Fatalf("legitimate native zero: %v %v", got, err)
	}
}

// This test talks to the actual production routes of a parent-started Observe
// server backed by the SAME native DSN. No substitute httptest handlers. Supply
// an isolated fixture site and an editor JWT; required mode fails on omission.
func TestR5GoalNativeHTTPCreateReadAggregate(t *testing.T) {
	base, site, token := os.Getenv("OBSERVE_R5_HTTP_URL"), os.Getenv("OBSERVE_R5_HTTP_SITE"), os.Getenv("OBSERVE_R5_HTTP_TOKEN")
	if base == "" || site == "" || token == "" {
		if os.Getenv("OBSERVE_REQUIRE_NUCLEUS") == "1" {
			t.Fatal("required actual Observe HTTP URL/site/editor token absent")
		}
		t.Skip("actual Observe HTTP fixture absent")
	}
	db := r5GoalNativeDB(t)
	ctx := context.Background()
	client := &http.Client{Timeout: 30 * time.Second}
	now := time.Now().UTC()
	request := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	for _, v := range []struct {
		name string
		unit int64
		n    int
		want int64
		bad  bool
	}{
		{"zero", 0, 1, 0, false}, {"empty-max", math.MaxInt64, 0, 0, false},
		{"maximum-unit-and-total", math.MaxInt64, 1, math.MaxInt64, false},
		{"largest-two-event-total", math.MaxInt64 / 2, 2, math.MaxInt64 - 1, false},
		{"overflow", math.MaxInt64, 2, 0, true},
	} {
		t.Run(v.name, func(t *testing.T) {
			matcher := goalSite("r5-" + v.name)
			status, b := request(http.MethodPost, "/api/v1/goals", map[string]any{"site_id": site, "name": matcher, "goal_type": "event", "goal_value": matcher, "value_minor": v.unit, "currency": "USD", "value_source": "fixed"})
			if status < 200 || status >= 300 {
				t.Fatalf("create status %d", status)
			}
			var created Goal
			if err := json.Unmarshal(b, &created); err != nil || created.GoalID == "" || created.ValueMinor != v.unit {
				t.Fatalf("create integer parity: %v %v", created, err)
			}
			t.Cleanup(func() {
				status, _ := request(http.MethodDelete, "/api/v1/goals/"+url.PathEscape(created.GoalID)+"?site_id="+url.QueryEscape(site), nil)
				if status < 200 || status >= 300 {
					t.Errorf("fixture goal deletion status %d", status)
				}
			})
			stored, err := NewStatsService(db).getGoal(ctx, site, created.GoalID)
			if err != nil || stored.ValueMinor != v.unit || stored.ValueSource != ValueSourceFixed {
				t.Fatalf("HTTP -> native read mismatch: %v %v", stored, err)
			}
			for i := 0; i < v.n; i++ {
				insertGoalEvent(t, db, site, fmt.Sprintf("%s-%d", matcher, i), matcher, `{}`, now)
			}
			t.Cleanup(func() { db.SQL().Exec(ctx, `DELETE FROM events WHERE site_id=$1 AND event_type=$2`, site, matcher) })
			path := "/api/v1/goals?site_id=" + url.QueryEscape(site) + "&from=" + strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10) + "&to=" + strconv.FormatInt(now.Add(time.Minute).UnixMilli(), 10)
			status, b = request(http.MethodGet, path, nil)
			if v.bad {
				if status < 400 {
					t.Fatalf("overflow produced HTTP success status %d", status)
				}
				return
			}
			if status != 200 {
				t.Fatalf("aggregate status %d", status)
			}
			var rows []GoalConversion
			if err := json.Unmarshal(b, &rows); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.Goal.GoalID == created.GoalID {
					found = true
					if row.Goal.ValueMinor != v.unit || row.TotalValueMinor != v.want || row.ConversionEvents != int64(v.n) {
						t.Fatalf("HTTP read/aggregate exact integers: %+v", row)
					}
				}
			}
			if !found {
				t.Fatal("created goal absent from production response")
			}
		})
	}
}
