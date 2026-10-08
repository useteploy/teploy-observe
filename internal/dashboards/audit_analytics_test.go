package dashboards

import (
	"testing"
)

func TestAuditPanelTimeRange(t *testing.T) {
	a, b, err := PanelTimeRange("2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z")
	if err != nil || b-a != 86400000 {
		t.Fatalf("%d %d %v", a, b, err)
	}
	c, d, err := PanelTimeRange("1790812800000", "1790899200000")
	if err != nil || c != a || d != b {
		t.Fatalf("%d %d %v", c, d, err)
	}
	for _, tc := range [][2]string{{"garbage", ""}, {"10", "9"}, {"10", "10"}} {
		if _, _, err := PanelTimeRange(tc[0], tc[1]); err == nil {
			t.Fatal(tc)
		}
	}
	p := validPanel()
	p.PanelType = "timeseries"
	p.QueryType = "errors"
	if err := ValidatePanel(&p); err != nil {
		t.Fatal(err)
	}
}
