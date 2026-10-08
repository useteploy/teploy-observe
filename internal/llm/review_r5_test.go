package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// This fixture supplies scanner-result cells, including real NULL pointers and
// absent field descriptors; it does not replace them with native zero values.
type r5Rows struct {
	fields  []pgconn.FieldDescription
	values  [][]*string
	next    int
	err     error
	scanErr error
	closed  bool
}

func (r *r5Rows) Next() bool {
	if r.next == len(r.values) {
		return false
	}
	r.next++
	return true
}
func (r *r5Rows) FieldDescriptions() []pgconn.FieldDescription { return r.fields }
func (r *r5Rows) Err() error                                   { return r.err }
func (r *r5Rows) Close()                                       { r.closed = true }
func (r *r5Rows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != len(r.fields) {
		return fmt.Errorf("fixture scan arity")
	}
	for i, d := range dest {
		p, ok := d.(**string)
		if !ok {
			return fmt.Errorf("expected nullable raw text destination")
		}
		*p = r.values[r.next-1][i]
	}
	return nil
}
func r5s(v string) *string { return &v }

var r5CommonColumns = []string{
	"legacy_cost_usd", "reported_tokens_in", "reported_tokens_out", "reported_tokens_total", "reported_cost_calls", "reported_token_calls",
	"estimated_tokens_in", "estimated_tokens_out", "estimated_tokens_total", "estimated_cost_calls", "estimated_token_calls",
	"legacy_tokens_in", "legacy_tokens_out", "legacy_tokens_total", "legacy_cost_calls", "legacy_token_calls", "unattributed", "invalid_numeric_rows",
}
var r5WireFields = []string{
	"total_cost_usd", "estimated_cost_usd", "reported_cost_usd", "legacy_cost_usd", "total_tokens", "cost_unattributed",
	"reported_tokens_in", "reported_tokens_out", "estimated_tokens_in", "estimated_tokens_out", "legacy_tokens_in", "legacy_tokens_out",
	"reported_tokens_total", "estimated_tokens_total", "legacy_tokens_total", "reported_cost_calls", "estimated_cost_calls", "legacy_cost_calls",
	"reported_token_calls", "estimated_token_calls", "legacy_token_calls",
}

func r5AggregateFixture(model bool) *r5Rows {
	columns := append([]string(nil), r5CommonColumns...)
	if model {
		columns = append(columns, "model", "provider", "call_count", "total_tokens", "total_cost_usd", "estimated_cost_usd", "reported_cost_usd", "avg_latency_ms")
	} else {
		columns = append(columns, "calls", "tokens", "cost", "estimated", "reported", "latency", "errors")
	}
	r := &r5Rows{values: [][]*string{make([]*string, len(columns))}}
	for i, name := range columns {
		r.fields = append(r.fields, pgconn.FieldDescription{Name: name})
		r.values[0][i] = r5s("0")
		if name == "model" {
			r.values[0][i] = r5s("m")
		}
		if name == "provider" {
			r.values[0][i] = r5s("p")
		}
	}
	return r
}
func (r *r5Rows) set(name string, value *string) {
	for i, f := range r.fields {
		if f.Name == name {
			r.values[0][i] = value
			return
		}
	}
	panic(name)
}
func (r *r5Rows) omit(name string) {
	for i, f := range r.fields {
		if f.Name == name {
			r.fields = append(r.fields[:i], r.fields[i+1:]...)
			r.values[0] = append(r.values[0][:i], r.values[0][i+1:]...)
			return
		}
	}
	panic(name)
}
func r5Call(s *LLMService, model bool) (any, error) {
	if model {
		return s.ModelBreakdown(context.Background(), "s", time.Unix(0, 0), time.Unix(1, 0))
	}
	return s.Stats(context.Background(), "s", time.Unix(0, 0), time.Unix(1, 0))
}

func TestR5LLMScannerRequiredNumericCells(t *testing.T) {
	for _, model := range []bool{false, true} {
		fixture := r5AggregateFixture(model)
		for _, fd := range fixture.fields {
			if fd.Name == "model" || fd.Name == "provider" {
				continue
			}
			bad := []*string{nil, r5s(""), r5s("-1"), r5s("NaN"), r5s("Inf"), r5s("+Inf"), r5s("-Inf"), r5s("garbage"), r5s(" 0"), r5s("+0"), r5s("1e309"), r5s("1e-5")}
			if strings.Contains(fd.Name, "tokens") || strings.Contains(fd.Name, "calls") || strings.HasSuffix(fd.Name, "_in") || strings.HasSuffix(fd.Name, "_out") || strings.HasSuffix(fd.Name, "_total") || fd.Name == "unattributed" || fd.Name == "errors" || fd.Name == "invalid_numeric_rows" {
				bad = append(bad, r5s("9223372036854775808"), r5s("1.1"))
			} else {
				bad = append(bad, r5s(strings.Repeat("9", 400)))
			}
			for i, value := range append(bad, r5s("MISSING")) {
				t.Run(fmt.Sprintf("model=%v/%s/%d", model, fd.Name, i), func(t *testing.T) {
					r := r5AggregateFixture(model)
					if value != nil && *value == "MISSING" {
						r.omit(fd.Name)
					} else {
						r.set(fd.Name, value)
					}
					s := NewLLMService(nil)
					s.aggregateRead = func(context.Context, string, ...any) (aggregateRows, error) { return r, nil }
					_, err := r5Call(s, model)
					if err == nil || !strings.Contains(err.Error(), fd.Name) || !r.closed {
						t.Fatalf("required invalid %s: %v closed=%v", fd.Name, err, r.closed)
					}
				})
			}
		}
	}
}

func TestR5LLMLegacyGuardAndEmptyEvidence(t *testing.T) {
	for _, model := range []bool{false, true} {
		r := r5AggregateFixture(model)
		r.set("invalid_numeric_rows", r5s("1"))
		s := NewLLMService(nil)
		s.aggregateRead = func(context.Context, string, ...any) (aggregateRows, error) { return r, nil }
		if _, err := r5Call(s, model); err == nil {
			t.Fatal("invalid legacy cells fabricated aggregate success")
		}
		for _, nonempty := range []bool{false, true} {
			queries := 0
			s.aggregateRead = func(_ context.Context, sql string, _ ...any) (aggregateRows, error) {
				queries++
				if queries == 1 {
					return &r5Rows{}, nil
				}
				if !strings.Contains(sql, "SELECT trace_id") {
					t.Fatal("no raw emptiness proof")
				}
				r := &r5Rows{}
				if nonempty {
					r.values = [][]*string{{r5s("trace")}}
				}
				return r, nil
			}
			_, err := r5Call(s, model)
			if (err != nil) != nonempty || queries != 2 {
				t.Fatalf("empty=%v queries=%d err=%v", !nonempty, queries, err)
			}
		}
		for _, failure := range []string{"query", "scan", "rows"} {
			sentinel := errors.New(failure + " failed")
			s.aggregateRead = func(context.Context, string, ...any) (aggregateRows, error) {
				r := r5AggregateFixture(model)
				switch failure {
				case "query":
					return nil, sentinel
				case "scan":
					r.scanErr = sentinel
				case "rows":
					r.err = sentinel
				}
				return r, nil
			}
			if _, err := r5Call(s, model); !errors.Is(err, sentinel) {
				t.Fatalf("%s swallowed: %v", failure, err)
			}
		}
	}
}

func TestR5LLMExplicitZeroWireContract(t *testing.T) {
	for _, model := range []bool{false, true} {
		s := NewLLMService(nil)
		s.aggregateRead = func(context.Context, string, ...any) (aggregateRows, error) { return r5AggregateFixture(model), nil }
		dto, err := r5Call(s, model)
		if err != nil {
			t.Fatal(err)
		}
		if model {
			dto = dto.([]ModelStats)[0]
		}
		b, _ := json.Marshal(dto)
		var wire map[string]any
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		for _, field := range r5WireFields {
			if wire[field] != "0" {
				t.Fatalf("%s missing explicit zero: %s", field, b)
			}
		}
	}
	// Strict string-count max remains exact; float costs never enter count parsing.
	r := r5AggregateFixture(false)
	r.set("tokens", r5s("9223372036854775807"))
	s := NewLLMService(nil)
	s.aggregateRead = func(context.Context, string, ...any) (aggregateRows, error) { return r, nil }
	dto, err := s.Stats(context.Background(), "s", time.Unix(0, 0), time.Unix(1, 0))
	if err != nil || dto.TotalTokens != "9223372036854775807" {
		t.Fatalf("max token text: %v %v", dto, err)
	}
}

func TestR5LLMIndependentStoredPartitionFixture(t *testing.T) {
	want := map[string]string{"total_cost_usd": "15", "reported_cost_usd": "1", "estimated_cost_usd": "2", "legacy_cost_usd": "12", "cost_unattributed": "3", "legacy_cost_calls": "3", "legacy_token_calls": "3", "legacy_tokens_in": "240", "legacy_tokens_out": "270", "legacy_tokens_total": "1379", "total_tokens": "1499"}
	for _, model := range []bool{false, true} {
		r := r5AggregateFixture(model)
		for name, v := range want {
			alias := name
			if name == "cost_unattributed" {
				alias = "unattributed"
			}
			if !model {
				switch name {
				case "total_cost_usd":
					alias = "cost"
				case "reported_cost_usd":
					alias = "reported"
				case "estimated_cost_usd":
					alias = "estimated"
				case "total_tokens":
					alias = "tokens"
				}
			}
			r.set(alias, r5s(v))
		}
		s := NewLLMService(nil)
		s.aggregateRead = func(context.Context, string, ...any) (aggregateRows, error) { return r, nil }
		dto, err := r5Call(s, model)
		if err != nil {
			t.Fatal(err)
		}
		if model {
			dto = dto.([]ModelStats)[0]
		}
		b, _ := json.Marshal(dto)
		var wire map[string]any
		json.Unmarshal(b, &wire)
		got := map[string]string{}
		for field := range want {
			got[field], _ = wire[field].(string)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("independent partition: %v want %v", got, want)
		}
	}
}
