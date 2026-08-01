package export

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRows is a minimal pgx.Rows that yields a fixed number of "rows" (Next
// returns true that many times) and then reports rowsErr from Err() — used to
// simulate a connection dying partway through a real result set without a
// live database.
type fakeRows struct {
	remaining int
	rowsErr   error
}

func (f *fakeRows) Close()                                       {}
func (f *fakeRows) Err() error                                   { return f.rowsErr }
func (f *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (f *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (f *fakeRows) Scan(dest ...any) error                       { return nil }
func (f *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (f *fakeRows) RawValues() [][]byte                          { return nil }
func (f *fakeRows) Conn() *pgx.Conn                              { return nil }
func (f *fakeRows) Next() bool {
	if f.remaining <= 0 {
		return false
	}
	f.remaining--
	return true
}

// TestStreamCSV_FailurePartwayLeavesSentinelNotSilentTruncation is the
// regression for OBS-028: a scan/write failure used to `break` with no
// signal at all — the response just stopped, indistinguishable from a
// dataset that happened to end there. Now a sentinel row marks it.
func TestStreamCSV_FailurePartwayLeavesSentinelNotSilentTruncation(t *testing.T) {
	rows := &fakeRows{remaining: 5}
	scanCount := 0
	scan := func() (string, error) {
		scanCount++
		if scanCount == 3 {
			return "", errors.New("simulated scan failure")
		}
		return "ok", nil
	}
	toRow := func(v string) []string { return []string{v} }

	rec := httptest.NewRecorder()
	streamCSV(rec, rows, []string{"col"}, scan, toRow, "test")

	body := rec.Body.String()
	if !strings.Contains(body, "__export_incomplete__") {
		t.Errorf("expected an incompleteness sentinel in a failed export, got:\n%s", body)
	}
	// Exactly 2 successful rows were written before the simulated failure on
	// the 3rd scan.
	if strings.Count(body, "ok\n") != 2 {
		t.Errorf("expected exactly 2 successful rows before the failure, got:\n%s", body)
	}
}

func TestStreamCSV_SuccessHasNoSentinel(t *testing.T) {
	rows := &fakeRows{remaining: 3}
	scan := func() (string, error) { return "ok", nil }
	toRow := func(v string) []string { return []string{v} }

	rec := httptest.NewRecorder()
	streamCSV(rec, rows, []string{"col"}, scan, toRow, "test")

	if strings.Contains(rec.Body.String(), "__export_incomplete__") {
		t.Errorf("successful export should not contain the incompleteness sentinel, got:\n%s", rec.Body.String())
	}
}

// TestStreamJSON_FailurePartwayLeavesInvalidJSON is the regression for
// OBS-028's worst case: on failure the array used to still be closed with
// `]`, so a truncated export was syntactically valid JSON and looked
// complete. It must now be left deliberately unparseable.
func TestStreamJSON_FailurePartwayLeavesInvalidJSON(t *testing.T) {
	rows := &fakeRows{remaining: 5}
	scanCount := 0
	scan := func() (map[string]string, error) {
		scanCount++
		if scanCount == 3 {
			return nil, errors.New("simulated scan failure")
		}
		return map[string]string{"v": "ok"}, nil
	}

	rec := httptest.NewRecorder()
	streamJSON(rec, rows, scan, "test")

	body := rec.Body.String()
	if strings.HasSuffix(strings.TrimSpace(body), "]") {
		t.Errorf("failed JSON export must NOT be closed with a trailing ] (would look complete), got:\n%s", body)
	}
	var parsed []any
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		t.Errorf("expected truncated JSON export to fail parsing as valid JSON, but it parsed cleanly: %v", parsed)
	}
}

func TestStreamJSON_SuccessIsValidJSON(t *testing.T) {
	rows := &fakeRows{remaining: 3}
	scan := func() (map[string]string, error) { return map[string]string{"v": "ok"}, nil }

	rec := httptest.NewRecorder()
	streamJSON(rec, rows, scan, "test")

	var parsed []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("successful export should be valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if len(parsed) != 3 {
		t.Errorf("expected 3 rows, got %d", len(parsed))
	}
}
