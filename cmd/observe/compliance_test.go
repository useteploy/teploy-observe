package main

import (
	"testing"

	"github.com/useteploy/teploy-observe/internal/audit"
)

func controlByID(cs []controlStatus, id string) controlStatus {
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	return controlStatus{}
}

func TestEvaluateControls_Healthy(t *testing.T) {
	cs := evaluateControls(complianceInputs{
		HasRecentAudit: true,
		Verify:         audit.VerifyResult{Intact: true, Count: 10},
		AuthRequired:   true,
		DemoMode:       false,
		TamperKeyState: "dedicated",
	})
	for _, id := range []string{"audit_logging", "audit_tamper_evidence", "authentication", "write_protection"} {
		if got := controlByID(cs, id).Status; got != "pass" {
			t.Errorf("%s should pass in a healthy config, got %q", id, got)
		}
	}
	if summarize(cs)["fail"] != 0 {
		t.Errorf("healthy config should have no failures: %+v", summarize(cs))
	}
}

func TestEvaluateControls_Problems(t *testing.T) {
	cs := evaluateControls(complianceInputs{
		HasRecentAudit: false,
		Verify:         audit.VerifyResult{Intact: false, BrokenAtSeq: 3, Detail: "hash mismatch"},
		AuthRequired:   false, // --no-auth
		DemoMode:       true,
		TamperKeyState: "dedicated",
	})
	if controlByID(cs, "audit_tamper_evidence").Status != "fail" {
		t.Error("broken chain must fail")
	}
	if controlByID(cs, "authentication").Status != "fail" {
		t.Error("no-auth must fail")
	}
	if controlByID(cs, "audit_logging").Status != "warn" {
		t.Error("no recent audit must warn")
	}
	if controlByID(cs, "write_protection").Status != "warn" {
		t.Error("demo mode must warn")
	}
}

func TestEvaluateControls_UnkeyedChainWarns(t *testing.T) {
	cs := evaluateControls(complianceInputs{
		HasRecentAudit: true,
		Verify:         audit.VerifyResult{Intact: true},
		AuthRequired:   true,
		TamperKeyState: "unkeyed",
	})
	if got := controlByID(cs, "audit_tamper_evidence").Status; got != "warn" {
		t.Errorf("unkeyed intact chain should warn, got %q", got)
	}
}

// F46: the JWT-fallback key state warns (chain keyed, but by a secret shared
// with the session domain); the persistent generated key passes.
func TestEvaluateControls_KeyStates(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  string
	}{
		{"dedicated", "pass"},
		{"persistent", "pass"},
		{"jwt-fallback", "warn"},
		{"unkeyed", "warn"},
	} {
		cs := evaluateControls(complianceInputs{
			HasRecentAudit: true,
			Verify:         audit.VerifyResult{Intact: true},
			AuthRequired:   true,
			TamperKeyState: tc.state,
		})
		if got := controlByID(cs, "audit_tamper_evidence").Status; got != tc.want {
			t.Errorf("key state %q should be %q, got %q", tc.state, tc.want, got)
		}
	}
}
