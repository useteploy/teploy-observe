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
		TamperKeyed:    true,
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
		TamperKeyed:    true,
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
		TamperKeyed:    false, // intact but no key
	})
	if got := controlByID(cs, "audit_tamper_evidence").Status; got != "warn" {
		t.Errorf("unkeyed intact chain should warn, got %q", got)
	}
}
