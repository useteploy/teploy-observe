package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/useteploy/teploy-observe/internal/audit"
)

// controlStatus is one compliance control's state. Status is pass/warn/fail/info.
type controlStatus struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type complianceReport struct {
	GeneratedAt int64           `json:"generated_at"`
	Controls    []controlStatus `json:"controls"`
	Summary     map[string]int  `json:"summary"`
}

// complianceInputs are the live facts the control evaluation reads. Split out so
// the evaluation logic is pure and unit-testable.
type complianceInputs struct {
	HasRecentAudit bool
	Verify         audit.VerifyResult
	VerifyErr      bool
	AuthRequired   bool
	DemoMode       bool
	TamperKeyed    bool
}

// evaluateControls maps the live facts to a control-status list (pure).
func evaluateControls(in complianceInputs) []controlStatus {
	var c []controlStatus

	if in.HasRecentAudit {
		c = append(c, controlStatus{"audit_logging", "Audit logging", "pass", "admin actions are being recorded"})
	} else {
		c = append(c, controlStatus{"audit_logging", "Audit logging", "warn", "no audit events in the last 24h"})
	}

	switch {
	case in.VerifyErr:
		c = append(c, controlStatus{"audit_tamper_evidence", "Audit tamper-evidence", "info", "chain could not be verified"})
	case !in.Verify.Intact:
		c = append(c, controlStatus{"audit_tamper_evidence", "Audit tamper-evidence", "fail", in.Verify.Detail})
	case !in.TamperKeyed:
		c = append(c, controlStatus{"audit_tamper_evidence", "Audit tamper-evidence", "warn", "chain intact but unkeyed — set OBSERVE_AUDIT_KEY so a DB-level actor can't forge it"})
	default:
		c = append(c, controlStatus{"audit_tamper_evidence", "Audit tamper-evidence", "pass", "hash chain intact and keyed"})
	}

	if in.AuthRequired {
		c = append(c, controlStatus{"authentication", "Authentication required", "pass", "API requires a valid session"})
	} else {
		c = append(c, controlStatus{"authentication", "Authentication required", "fail", "authentication is disabled (--no-auth) — do not use in production"})
	}

	c = append(c, controlStatus{"access_control", "Role-based access control", "pass", "admin/editor/viewer roles enforced"})

	if in.DemoMode {
		c = append(c, controlStatus{"write_protection", "Write protection", "warn", "demo mode is on (writes blocked)"})
	} else {
		c = append(c, controlStatus{"write_protection", "Write protection", "pass", "mutations require editor+ role"})
	}

	c = append(c, controlStatus{"transport_security", "Transport security (TLS)", "info", "TLS is terminated by the front proxy (Caddy); verify HTTPS externally"})

	return c
}

func summarize(controls []controlStatus) map[string]int {
	m := map[string]int{"pass": 0, "warn": 0, "fail": 0, "info": 0}
	for _, c := range controls {
		m[c.Status]++
	}
	return m
}

// complianceHandler serves the admin compliance control-status report: the
// demo-able "here are your controls" surface (evidence layer for SOC2/ISO). It
// reports only what observe can actually verify — self-hosted, so the operator
// owns the compliance; this is the control plane, not a certification.
func complianceHandler(store auditStore, authRequired, demoMode, tamperKeyed bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recent, _ := store.List(r.Context(), audit.Filter{
			From:  time.Now().Add(-24 * time.Hour).UnixMilli(),
			Limit: 1,
		})
		verify, verr := store.Verify(r.Context())

		controls := evaluateControls(complianceInputs{
			HasRecentAudit: len(recent) > 0,
			Verify:         verify,
			VerifyErr:      verr != nil,
			AuthRequired:   authRequired,
			DemoMode:       demoMode,
			TamperKeyed:    tamperKeyed,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(complianceReport{
			GeneratedAt: time.Now().UnixMilli(),
			Controls:    controls,
			Summary:     summarize(controls),
		})
	}
}
