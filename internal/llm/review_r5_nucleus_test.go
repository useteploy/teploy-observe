package llm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// Parent-controlled source-only acceptance fixtures. Every required prerequisite
// fails when REQUIRE_NUCLEUS=1; none has been executed for this source round.
func r5LLMNativeDB(t *testing.T) *nucleus.Client {
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
		t.Fatal("fixture must use actual Nucleus")
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestR5LLMNativeLegacyNumericRefusal(t *testing.T) {
	db := r5LLMNativeDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, field := range []string{"cost_usd", "prompt_tokens", "completion_tokens", "total_tokens", "latency_ms"} {
		for _, bad := range []string{"", "bad", "-1", "NaN", "Inf", "+Inf", "9223372036854775808"} {
			// A huge decimal is finite syntax but overflows aggregate float parsing.
			if bad == "9223372036854775808" && field == "cost_usd" {
				bad = "9999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999"
			}
			t.Run(field+"/"+fmt.Sprintf("%q", bad), func(t *testing.T) {
				site := catalogSite(t)
				t.Cleanup(func() { db.SQL().Exec(ctx, `DELETE FROM llm_traces WHERE site_id=$1`, site) })
				values := map[string]string{"cost_usd": "1", "prompt_tokens": "2", "completion_tokens": "3", "total_tokens": "999", "latency_ms": "1"}
				values[field] = bad
				_, err := db.SQL().Exec(ctx, `INSERT INTO llm_traces (trace_id,site_id,timestamp,model,provider,cost_usd,prompt_tokens,completion_tokens,total_tokens,latency_ms,cost_source,token_source) VALUES ($1,$2,$3,'legacy','fixture',$4,$5,$6,$7,$8,'','')`, site+"-bad", site, now.UnixMilli(), values["cost_usd"], values["prompt_tokens"], values["completion_tokens"], values["total_tokens"], values["latency_ms"])
				if err != nil {
					t.Fatalf("legacy TEXT seed refused; fixture requires stored anomaly: %v", err)
				}
				// Earlier valid storage/read must not mask the independent anomalous row.
				_, err = db.SQL().Exec(ctx, `INSERT INTO llm_traces (trace_id,site_id,timestamp,model,provider,cost_usd,prompt_tokens,completion_tokens,total_tokens,latency_ms,cost_source,token_source) VALUES ($1,$2,$3,'legacy','fixture','2','4','5','9','1','reported','estimated')`, site+"-good", site, now.UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				s := NewLLMService(db)
				if got, err := s.Stats(ctx, site, now.Add(-time.Second), now.Add(time.Second)); err == nil {
					t.Fatalf("invalid stored %s accepted: %+v", field, got)
				}
				if got, err := s.ModelBreakdown(ctx, site, now.Add(-time.Second), now.Add(time.Second)); err == nil {
					t.Fatalf("invalid stored model %s accepted: %+v", field, got)
				}
				raw, err := s.RecentTraces(ctx, site, 10)
				if err != nil || len(raw) != 2 {
					t.Fatalf("raw trace transport changed: %v %v", raw, err)
				}
				found := false
				for _, r := range raw {
					if r.TraceID == site+"-bad" {
						found = true
						got := map[string]string{"cost_usd": r.CostUSD, "prompt_tokens": r.PromptTokens, "completion_tokens": r.CompletionTokens, "total_tokens": r.TotalTokens, "latency_ms": r.LatencyMs}
						if got[field] != bad {
							t.Fatalf("raw anomaly rewritten: %q want %q", got[field], bad)
						}
					}
				}
				if !found {
					t.Fatal("raw anomaly absent")
				}
			})
		}
	}
}

func TestR5LLMNativeIndependentLegacyTotals(t *testing.T) {
	db := r5LLMNativeDB(t)
	ctx := context.Background()
	site := catalogSite(t)
	now := time.Now().UTC()
	t.Cleanup(func() { db.SQL().Exec(ctx, `DELETE FROM llm_traces WHERE site_id=$1`, site) })
	for i, r := range []struct {
		costSource, tokenSource any
		in, out, total, usd     string
	}{
		{"reported", "estimated", "10", "20", "30", "1"}, {"estimated", "reported", "40", "50", "90", "2"},
		{"", "", "60", "70", "999", "3"}, {"future", "future", "80", "90", "170", "4"}, {nil, nil, "100", "110", "210", "5"},
	} {
		_, err := db.SQL().Exec(ctx, `INSERT INTO llm_traces (trace_id,site_id,timestamp,model,provider,cost_usd,prompt_tokens,completion_tokens,total_tokens,latency_ms,cost_source,token_source) VALUES ($1,$2,$3,'mixed','fixture',$4,$5,$6,$7,'1',$8,$9)`, fmt.Sprintf("%s-%d", site, i), site, now.UnixMilli(), r.usd, r.in, r.out, r.total, r.costSource, r.tokenSource)
		if err != nil {
			t.Fatal(err)
		}
	}
	s := NewLLMService(db)
	st, err := s.Stats(ctx, site, now.Add(-time.Second), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	models, err := s.ModelBreakdown(ctx, site, now.Add(-time.Second), now.Add(time.Second))
	if err != nil || len(models) != 1 {
		t.Fatalf("%v %v", models, err)
	}
	if st.TotalCostUSD != "15" || st.LegacyCostUSD != "12" || st.CostUnattributed != "3" || st.LegacyCostCalls != "3" || st.LegacyTokenCalls != "3" || st.LegacyTokensIn != "240" || st.LegacyTokensOut != "270" || st.LegacyTokensTotal != "1379" || st.TotalTokens != "1499" || st.ReportedCostUSD != "1" || st.EstimatedCostUSD != "2" || st.ReportedTokensIn != "40" || st.EstimatedTokensIn != "10" {
		t.Fatalf("independent classes/totals: %+v", st)
	}
	if models[0].ProvenanceStats != st.ProvenanceStats || models[0].CostUnattributed != st.CostUnattributed || models[0].TotalTokens != st.TotalTokens || models[0].TotalCostUSD != st.TotalCostUSD {
		t.Fatal("native stats/model disagreement")
	}
	empty, err := s.Stats(ctx, site, now.Add(time.Hour), now.Add(2*time.Hour))
	if err != nil || empty.TotalTokens != "0" || empty.TotalCostUSD != "0" {
		t.Fatalf("native empty success: %+v %v", empty, err)
	}
}
