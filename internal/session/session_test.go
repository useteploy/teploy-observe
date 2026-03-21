package session

import (
	"testing"
	"time"
)

func TestID_Deterministic(t *testing.T) {
	// Same inputs produce the same session ID
	id1 := ID("site1", "192.168.1.1", "Mozilla/5.0", "salt123")
	id2 := ID("site1", "192.168.1.1", "Mozilla/5.0", "salt123")
	if id1 != id2 {
		t.Errorf("expected deterministic IDs, got %s != %s", id1, id2)
	}
}

func TestID_DifferentInputs(t *testing.T) {
	base := ID("site1", "192.168.1.1", "Mozilla/5.0", "salt123")

	// Different site
	diffSite := ID("site2", "192.168.1.1", "Mozilla/5.0", "salt123")
	if base == diffSite {
		t.Error("different site_id should produce different session_id")
	}

	// Different IP
	diffIP := ID("site1", "10.0.0.1", "Mozilla/5.0", "salt123")
	if base == diffIP {
		t.Error("different IP should produce different session_id")
	}

	// Different UA
	diffUA := ID("site1", "192.168.1.1", "Chrome/120", "salt123")
	if base == diffUA {
		t.Error("different UA should produce different session_id")
	}

	// Different salt
	diffSalt := ID("site1", "192.168.1.1", "Mozilla/5.0", "other-salt")
	if base == diffSalt {
		t.Error("different salt should produce different session_id")
	}
}

func TestID_UUIDFormat(t *testing.T) {
	id := ID("site1", "192.168.1.1", "Mozilla/5.0", "salt123")
	// Should be UUID-like format: 8-4-4-4-12 hex chars
	if len(id) != 36 {
		t.Errorf("expected UUID length 36, got %d: %s", len(id), id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("expected UUID dash positions, got: %s", id)
	}
}

func TestVisitID_Deterministic(t *testing.T) {
	ts := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC)
	v1 := VisitID("session123", ts)
	v2 := VisitID("session123", ts)
	if v1 != v2 {
		t.Errorf("expected deterministic visit IDs, got %s != %s", v1, v2)
	}
}

func TestVisitID_RotatesOnHourBoundary(t *testing.T) {
	ts1 := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC)
	ts2 := time.Date(2026, 3, 20, 15, 30, 0, 0, time.UTC) // next hour

	v1 := VisitID("session123", ts1)
	v2 := VisitID("session123", ts2)
	if v1 == v2 {
		t.Error("visit ID should rotate across hour boundaries")
	}
}

func TestVisitID_StableWithinHour(t *testing.T) {
	ts1 := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 3, 20, 14, 59, 59, 0, time.UTC)

	v1 := VisitID("session123", ts1)
	v2 := VisitID("session123", ts2)
	if v1 != v2 {
		t.Error("visit ID should be stable within the same hour")
	}
}
