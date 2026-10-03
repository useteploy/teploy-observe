package logs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

func refusalOf(t *testing.T, err error) *queryguard.Refusal {
	t.Helper()
	var ref *queryguard.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a *queryguard.Refusal, got %v", err)
	}
	return ref
}

func guardedLogs(rows int64, timeout time.Duration, l *queryguard.Limiter) *LogService {
	b := queryguard.DefaultBudgets()
	b.MaxScanRows = rows
	b.Timeout = timeout
	return (&LogService{}).WithQueryGuard(l, b)
}

func TestHistogramRowCapRefuses(t *testing.T) {
	s := guardedLogs(2, time.Second, nil)
	var gotSQL string
	s.queryHistogram = func(_ context.Context, q string, _ ...any) ([]HistogramBucket, error) {
		gotSQL = q
		return make([]HistogramBucket, 3), nil
	}
	_, err := s.Histogram(context.Background(), "site", time.Unix(0, 0), time.Unix(100, 0), 1000)
	if ref := refusalOf(t, err); ref.Code != queryguard.CodeBudgetRows || ref.Status != 429 {
		t.Fatalf("refusal = %+v", ref)
	}
	if !strings.Contains(gotSQL, "LIMIT 3") {
		t.Fatalf("SQL must bound the read at budget+1 rows:\n%s", gotSQL)
	}
	// At the cap it still answers.
	s.queryHistogram = func(context.Context, string, ...any) ([]HistogramBucket, error) {
		return make([]HistogramBucket, 2), nil
	}
	if out, err := s.Histogram(context.Background(), "site", time.Unix(0, 0), time.Unix(100, 0), 1000); err != nil || len(out) != 2 {
		t.Fatalf("at cap: %v %d", err, len(out))
	}
}

func TestHistogramTimeBudgetIs504(t *testing.T) {
	s := guardedLogs(100, 20*time.Millisecond, nil)
	s.queryHistogram = func(ctx context.Context, _ string, _ ...any) ([]HistogramBucket, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := s.Histogram(context.Background(), "site", time.Unix(0, 0), time.Unix(100, 0), 1000)
	if ref := refusalOf(t, err); ref.Code != queryguard.CodeBudgetTime || ref.Status != 504 {
		t.Fatalf("refusal = %+v", ref)
	}
}

// Concurrency admission refuses before any engine access (the service has
// no db, so reaching the query would panic).
func TestLogReadsConcurrencyRefusal(t *testing.T) {
	l := queryguard.NewLimiter(1, 1)
	hold, err := l.Acquire(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	s := guardedLogs(100, time.Second, l)
	ctx := context.Background()
	from, to := time.Unix(0, 0), time.Unix(100, 0)

	if _, err := s.SearchLogs(ctx, "site", from, to, "", "", "", 10, 0); refusalOf(t, err).Status != 429 {
		t.Fatalf("SearchLogs not refused: %v", err)
	}
	if _, err := s.Histogram(ctx, "site", from, to, 1000); refusalOf(t, err).Status != 429 {
		t.Fatalf("Histogram not refused: %v", err)
	}
	if _, err := s.LogStats(ctx, "site", from, to); refusalOf(t, err).Status != 429 {
		t.Fatalf("LogStats not refused: %v", err)
	}
}

// A page that would reach past the row budget is refused, not shortened.
func TestSearchLogsPageBeyondBudgetRefuses(t *testing.T) {
	s := guardedLogs(100, time.Second, nil) // nil db: must refuse before querying
	from, to := time.Unix(0, 0), time.Unix(100, 0)
	for _, c := range []struct{ limit, offset int }{{101, 0}, {50, 51}, {1 << 40, 0}} {
		_, err := s.SearchLogs(context.Background(), "site", from, to, "", "", "", c.limit, c.offset)
		if ref := refusalOf(t, err); ref.Code != queryguard.CodeBudgetRows {
			t.Fatalf("limit=%d offset=%d: %+v", c.limit, c.offset, ref)
		}
	}
}
