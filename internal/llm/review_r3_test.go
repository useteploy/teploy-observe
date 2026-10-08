package llm

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestR3ProvenanceDTOFieldsArePresent(t *testing.T) {
	b, err := json.Marshal(ModelStats{ProvenanceStats: (provenanceRow{}).dto(), ReportedCostUSD: "0", CostUnattributed: "0"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	for _, k := range []string{"legacy_cost_usd", "reported_cost_usd", "reported_tokens_in", "reported_tokens_out", "reported_tokens_total", "estimated_tokens_in", "estimated_tokens_out", "estimated_tokens_total", "legacy_tokens_in", "legacy_tokens_out", "legacy_tokens_total", "reported_cost_calls", "estimated_cost_calls", "legacy_cost_calls", "reported_token_calls", "estimated_token_calls", "legacy_token_calls", "cost_unattributed"} {
		if got[k] != "0" {
			t.Fatalf("%s=%v; absent/null/default fiction", k, got[k])
		}
	}
}
func TestR3MixedLegacyModelProvenanceNative(t *testing.T) {
	db := catalogFixture(t)
	ctx := context.Background()
	site := catalogSite(t)
	now := time.Now().UTC()
	defer db.SQL().Exec(ctx, `DELETE FROM llm_traces WHERE site_id=$1`, site)
	for i, r := range []struct {
		cost, token    string
		in, out, total int64
		usd            float64
	}{{"reported", "estimated", 10, 20, 30, 1}, {"estimated", "reported", 40, 50, 90, 2}, {"", "", 60, 70, 999, 3}, {"future", "future", 80, 90, 170, 4}} {
		_, err := db.SQL().Exec(ctx, `INSERT INTO llm_traces (trace_id,tenant_id,site_id,timestamp,model,provider,prompt_tokens,completion_tokens,total_tokens,cost_usd,cost_source,token_source,latency_ms,status) VALUES ($1,'default',$2,$3,'mixed','fixture',$4,$5,$6,$7,$8,$9,1,'ok')`, site+string(rune('a'+i)), site, now.UnixMilli(), r.in, r.out, r.total, r.usd, r.cost, r.token)
		if err != nil {
			t.Fatal(err)
		}
	}
	svc := NewLLMService(db)
	st, err := svc.Stats(ctx, site, now.Add(-time.Second), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	models, err := svc.ModelBreakdown(ctx, site, now.Add(-time.Second), now.Add(time.Second))
	if err != nil || len(models) != 1 {
		t.Fatalf("%v %v", models, err)
	}
	m := models[0]
	if st.ReportedCostUSD != "1" || st.EstimatedCostUSD != "2" || st.LegacyCostUSD != "7" || st.CostUnattributed != "2" || st.ReportedTokensIn != "40" || st.EstimatedTokensIn != "10" || st.LegacyTokensIn != "140" || st.LegacyTokensTotal != "1169" {
		t.Fatalf("independent partitions: %+v", st)
	}
	if m.ProvenanceStats != st.ProvenanceStats || m.ReportedCostUSD != st.ReportedCostUSD || m.CostUnattributed != st.CostUnattributed {
		t.Fatalf("model/stat parity: %+v %+v", m, st)
	}
}
