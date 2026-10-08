package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

func TestR5PropertyKeysProductionPostRead(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 50; i++ {
		props[fmt.Sprintf("k%02d", i)] = strings.Repeat("v", 100)
	}
	b, _ := json.Marshal(props)
	rows := []propertyKeyEvent{{string(b)}, {`{"k00":1}`}, {`bad-json`}}
	from := time.Unix(0, 0)
	for _, phase := range []string{"property-decode-read", "property-expand", "property-output", "property-sort", "property-complete"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := NewStatsService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 3})
			acquired := false
			s.propertyKeyRead = func(context.Context, string, ...any) ([]propertyKeyEvent, error) { acquired = true; return rows, nil }
			hits := 0
			s.postReadCheckpoint = func(p string) {
				if p == phase {
					hits++
					if hits == 2 || phase == "property-complete" {
						cancel()
					}
				}
			}
			got, err := s.EventPropertyKeys(ctx, "s", "buy", from, from.Add(time.Hour))
			if !acquired || !errors.Is(err, context.Canceled) || got != nil {
				t.Fatalf("acquired=%v hits=%d got=%v err=%v", acquired, hits, got, err)
			}
		})
	}
	s := NewStatsService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 3})
	s.propertyKeyRead = func(context.Context, string, ...any) ([]propertyKeyEvent, error) { return rows, nil }
	got, err := s.EventPropertyKeys(context.Background(), "s", "buy", from, from.Add(time.Hour))
	want := make([]PropertyKeyStat, 50)
	for i := range want {
		want[i] = PropertyKeyStat{Key: fmt.Sprintf("k%02d", i), Count: 1}
	}
	want[0].Count = 2
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("complete result: %v %v", got, err)
	}
	props["overflow"] = 1
	b, _ = json.Marshal(props)
	s.propertyKeyRead = func(context.Context, string, ...any) ([]propertyKeyEvent, error) {
		return []propertyKeyEvent{{string(b)}}, nil
	}
	if got, err := s.EventPropertyKeys(context.Background(), "s", "buy", from, from.Add(time.Hour)); err == nil || got != nil {
		t.Fatal("51 keys silently accepted/truncated")
	}
	s.propertyKeyRead = func(context.Context, string, ...any) ([]propertyKeyEvent, error) {
		return []propertyKeyEvent{{strings.Repeat("x", maxPropertyDocumentBytes+1)}}, nil
	}
	if _, err := s.EventPropertyKeys(context.Background(), "s", "buy", from, from.Add(time.Hour)); err == nil {
		t.Fatal("oversized decode accepted")
	}
	// Exact document boundary retains the full answer.
	s.propertyKeyRead = func(context.Context, string, ...any) ([]propertyKeyEvent, error) {
		return []propertyKeyEvent{{`{"x":"` + strings.Repeat("v", maxPropertyDocumentBytes-8) + `"}`}}, nil
	}
	if got, err := s.EventPropertyKeys(context.Background(), "s", "buy", from, from.Add(time.Hour)); err != nil || len(got) != 1 || got[0].Key != "x" {
		t.Fatalf("document boundary: %v %v", got, err)
	}
}

func TestR5AttributionProductionPostRead(t *testing.T) {
	rows := make([]attributionEvent, 200)
	for i := range rows {
		rows[i] = attributionEvent{SessionID: fmt.Sprintf("s%d", i%2), UTMSource: fmt.Sprintf("source%03d", i), EventID: fmt.Sprint(i), Timestamp: int64(200 - i)}
	}
	for _, phase := range []string{"attribution-copy", "attribution-sort-compare", "attribution-credit", "attribution-accumulate", "attribution-output", "attribution-final-sort"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := NewAttributionService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 200})
			acquired := false
			s.readEvents = func(context.Context, string, ...any) ([]attributionEvent, error) { acquired = true; return rows, nil }
			hits := 0
			s.stats.postReadCheckpoint = func(p string) {
				if p == phase {
					hits++
					if hits == 2 {
						cancel()
					}
				}
			}
			got, err := s.AttributionByModel(ctx, "s", AttributionLinear, 0, 1000)
			if !acquired || got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("acquired=%v hits=%d got=%v err=%v", acquired, hits, got, err)
			}
		})
	}
	s := NewAttributionService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 200})
	fixture := []attributionEvent{{SessionID: "a", UTMSource: "first", Timestamp: 1, EventID: "a"}, {SessionID: "a", UTMSource: "last", Timestamp: 1, EventID: "z"}, {SessionID: "b"}}
	s.readEvents = func(context.Context, string, ...any) ([]attributionEvent, error) { return fixture, nil }
	for _, model := range []string{AttributionFirstTouch, AttributionLastTouch, AttributionLinear} {
		got, err := s.AttributionByModel(context.Background(), "s", model, 0, 1000)
		want := []AttributionRow{{Source: directBucket, Sessions: 1, Conversions: 1, ConversionPct: 100}}
		switch model {
		case AttributionFirstTouch:
			want = append(want, AttributionRow{Source: "first", Sessions: 1, Conversions: 1, ConversionPct: 100})
		case AttributionLastTouch:
			want = append(want, AttributionRow{Source: "last", Sessions: 1, Conversions: 1, ConversionPct: 100})
		case AttributionLinear:
			want = append(want, AttributionRow{Source: "first", Sessions: .5, Conversions: .5, ConversionPct: 100}, AttributionRow{Source: "last", Sessions: .5, Conversions: .5, ConversionPct: 100})
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v want %v err=%v", model, got, want, err)
		}
	}
}

func r5text(s string) *string { return &s }
func TestR5GoalCountAnomaliesAfterEarlierSuccess(t *testing.T) {
	bad := []*string{r5text(""), nil, r5text("garbage"), r5text("-1"), r5text("9223372036854775808"), r5text("1.0"), r5text("+1"), r5text(" 0")}
	for _, field := range []string{"count", "sessions", "events"} {
		for i, text := range append(bad, r5text("MISSING")) {
			t.Run(fmt.Sprintf("%s-%d", field, i), func(t *testing.T) {
				s := NewStatsService(nil)
				s.goalListRead = func(context.Context, string) ([]Goal, error) {
					return []Goal{{GoalID: "g", GoalType: "event", GoalValue: "buy", Currency: "USD", ValueSource: ValueSourceFixed, ValueMinor: math.MaxInt64}}, nil
				}
				reads := 0
				s.goalCountRead = func(context.Context, string, ...any) ([]goalCountResult, error) {
					reads++
					r := goalCountResult{"count": r5text("2")}
					if reads == 2 {
						r = goalCountResult{"sessions": r5text("1"), "events": r5text("1")}
					}
					if (field == "count" && reads == 1) || (field != "count" && reads == 2) {
						r[field] = text
						if text != nil && *text == "MISSING" {
							delete(r, field)
						}
					}
					return []goalCountResult{r}, nil
				}
				got, err := s.GoalConversions(context.Background(), "s", time.Unix(0, 0), time.Unix(1, 0))
				expectedReads := 2
				if field == "count" {
					expectedReads = 1
				}
				if err == nil || got != nil || reads != expectedReads {
					t.Fatalf("%s: reads=%d got=%v err=%v", field, reads, got, err)
				}
			})
		}
	}
	for _, value := range []string{"0", "9223372036854775807"} {
		n, err := strictGoalCount([]goalCountResult{{"count": r5text(value)}}, "count")
		if err != nil || fmt.Sprint(n) != value {
			t.Fatalf("valid count %s: %d %v", value, n, err)
		}
	}
	for _, rows := range [][]goalCountResult{nil, {{"count": r5text("0")}, {"count": r5text("0")}}} {
		if _, err := strictGoalCount(rows, "count"); err == nil {
			t.Fatal("invalid scalar cardinality accepted")
		}
	}
}
