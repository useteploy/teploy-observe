package dashboards

import (
	"testing"
)

func validPanel() Panel {
	return Panel{
		PanelType: "metric", QueryType: "pageviews",
		PositionX: "0", PositionY: "0", Width: "6", Height: "4",
	}
}

// R36 (round 4): create and update share one validation gate — an empty
// panel type (an instant tombstone), invalid config JSON, nonnumeric layout
// fields, and out-of-range geometry are rejected before any write.
func TestValidatePanelRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Panel)
	}{
		{"empty panel type", func(p *Panel) { p.PanelType = "" }},
		{"unknown panel type", func(p *Panel) { p.PanelType = "hologram" }},
		{"empty query type", func(p *Panel) { p.QueryType = "" }},
		{"unsupported query type", func(p *Panel) { p.QueryType = "custom_sql" }},
		{"invalid config json", func(p *Panel) { p.QueryConfig = "{not json" }},
		{"nonnumeric position", func(p *Panel) { p.PositionX = "left" }},
		{"out of range width", func(p *Panel) { p.Width = "99" }},
		{"zero height", func(p *Panel) { p.Height = "0" }},
		{"negative position", func(p *Panel) { p.PositionY = "-1" }},
		{"metric series without metric", func(p *Panel) {
			p.PanelType = "metric_series"
			p.QueryType = "metric_series"
			p.QueryConfig = `{"step":"60s"}`
		}},
	} {
		p := validPanel()
		tc.mut(&p)
		if err := validatePanel(&p); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
}

func TestValidatePanelNormalizesDefaults(t *testing.T) {
	p := Panel{PanelType: "metric", QueryType: "visitors"}
	if err := validatePanel(&p); err != nil {
		t.Fatalf("valid panel rejected: %v", err)
	}
	if p.Width != "6" || p.Height != "4" || p.PositionX != "0" || p.PositionY != "0" {
		t.Fatalf("defaults not applied: %+v", p)
	}
}

// R19 (round 4): replacement versions never tie or regress.
func TestNextVersionMS(t *testing.T) {
	if got := nextVersionMS(1000, 1000); got != "1001" {
		t.Fatalf("same-ms tie: got %s want 1001", got)
	}
	if got := nextVersionMS(2000, 1500); got != "2001" {
		t.Fatalf("rollback: got %s want 2001", got)
	}
	if got := nextVersionMS(1000, 1500); got != "1500" {
		t.Fatalf("advance: got %s want 1500", got)
	}
}
