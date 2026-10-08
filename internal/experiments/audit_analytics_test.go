package experiments

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestAuditChiSquareHalfConvertedOracle(t *testing.T) {
	for _, c := range [][]int64{{50, 65}, {65, 50}, {50, 35}, {35, 50}} {
		got := chiSquareOmnibus([]int64{100, 100}, c)
		if math.Abs(got.Stat-4.01023017902813) > 1e-10 || math.Abs(got.PValue-0.0452249761387) > 1e-10 {
			t.Fatalf("%v: %+v", c, got)
		}
	}
}
func TestAuditAllLosingTreatmentsNeverWin(t *testing.T) {
	for _, kind := range []string{KindBinary, KindCount, KindMean} {
		in := baseInputs(100)
		in.Cfg.MetricKind = kind
		for _, arm := range []string{"control", "b"} {
			addUsers(&in, arm, arm, 1000, 1000)
			n := 200
			if arm == "control" {
				n = 800
			}
			for i := 0; i < n; i++ {
				u := fmt.Sprintf("%s%d", arm, i)
				in.BinaryConversions = append(in.BinaryConversions, userVariant{u, arm})
				in.Events[PrimaryMetricKey] = append(in.Events[PrimaryMetricKey], metricEvent{u, 2000, 1})
			}
		}
		r := computeResults(in)
		if r.Winner != "" || r.Analysis.PValue >= 0.05 {
			t.Fatalf("%s: %+v", kind, r)
		}
	}
}
func TestAuditKeyedWeightsAndMissingArms(t *testing.T) {
	for _, raw := range []string{`[{"key":"b","rollout_pct":80},{"key":"control","rollout_pct":20}]`, `[{"key":"control","rollout_pct":20},{"key":"b","rollout_pct":80}]`} {
		in := baseInputs(100)
		in.Exp.Variants = raw
		addUsers(&in, "c", "control", 200, 1000)
		addUsers(&in, "b", "b", 800, 1000)
		r := computeResults(in)
		if r.Analysis.SRM.Detected || r.Analysis.SRM.ChiSquare != 0 {
			t.Fatalf("misaligned: %+v", r.Analysis.SRM)
		}
	}
	for _, kind := range []string{KindBinary, KindCount, KindMean} {
		for _, arm := range []string{"control", "b", ""} {
			in := baseInputs(1)
			in.Cfg.MetricKind = kind
			if arm != "" {
				addUsers(&in, "u", arm, 1000, 1000)
			}
			r := computeResults(in)
			if len(r.Variants) != 2 || r.Analysis.HorizonMet || r.Winner != "" {
				t.Fatalf("missing %s/%s: %+v", kind, arm, r)
			}
			if _, err := json.Marshal(r); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func TestAuditDefinitionAdmission(t *testing.T) {
	for _, raw := range []string{"control,treatment", `[]`, `[{"key":"a"},{"key":"a"}]`, `[{"key":"a","weight":-1},{"key":"b"}]`, `[{"key":"a","weight":1e308},{"key":"b","weight":1e308}]`, `[{"key":"a","weight":80},{"key":"b"}]`} {
		if ValidateExperimentDefinition(raw, 100) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := ValidateExperimentDefinition(`[{"key":"baseline","weight":20},{"key":"new","weight":80}]`, 10000); err != nil {
		t.Fatal(err)
	}
}
