package dashboards

import "testing"

// IsValidPanelType is the surface a typo'd panel_type would crash through
// later in render. Pin the allow-list so a future panel_type addition has
// to also flip this test.
func TestIsValidPanelType(t *testing.T) {
	for _, ok := range []string{"metric", "timeseries", "table", "bar", "metric_series"} {
		if !IsValidPanelType(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "heatmap", "metric_serieses", "MetricSeries"} {
		if IsValidPanelType(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
