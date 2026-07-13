package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/useteploy/teploy-observe/internal/audit"
)

// auditStore is the slice of audit.Service the HTTP handlers need. Narrowing to
// an interface lets the handlers be unit-tested with a fake (no Nucleus).
type auditStore interface {
	List(ctx context.Context, f audit.Filter) ([]audit.AuditEvent, error)
	Record(ctx context.Context, ev audit.AuditEvent) error
}

// auditListHandler serves the admin-only audit query. Filters come from the
// query string; all are optional. site_id is an OPTIONAL filter here (unlike
// most observe reads) because the audit view is a cross-site admin surface.
func auditListHandler(store auditStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := audit.Filter{
			SiteID: q.Get("site_id"),
			Actor:  q.Get("actor"),
			Action: q.Get("action"),
			Result: q.Get("result"),
			From:   auditParseInt(q.Get("from")),
			To:     auditParseInt(q.Get("to")),
			Limit:  int(auditParseInt(q.Get("limit"))),
		}
		events, err := store.List(r.Context(), f)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		if events == nil {
			events = []audit.AuditEvent{}
		}
		json.NewEncoder(w).Encode(events)
	}
}

type auditRecordInput struct {
	SiteID    string         `json:"site_id"`
	Actor     string         `json:"actor"`
	ActorType string         `json:"actor_type"`
	Action    string         `json:"action"`
	Target    string         `json:"target"`
	Result    string         `json:"result"`
	Metadata  map[string]any `json:"metadata"`
}

// auditRecordHandler lets an authenticated producer (CLI, dash, Ship agent)
// write an audit event. Source IP + user agent are stamped server-side so a
// producer can't forge them; the received time is server-side too (Record
// defaults an empty timestamp). Gate this at editor+ in the router.
func auditRecordHandler(store auditStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in auditRecordInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid JSON"}`))
			return
		}
		ev := audit.AuditEvent{
			SiteID:    in.SiteID,
			Actor:     in.Actor,
			ActorType: in.ActorType,
			Action:    in.Action,
			Target:    in.Target,
			Result:    in.Result,
			SourceIP:  auditClientIP(r),
			UserAgent: r.UserAgent(),
			Metadata:  audit.MarshalMetadata(in.Metadata),
		}
		w.Header().Set("Content-Type", "application/json")
		if err := store.Record(r.Context(), ev); err != nil {
			// Record only errors on validation (e.g. missing action).
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))
	}
}

// auditClientIP returns the direct peer's IP (host part of RemoteAddr). Audit
// writes come from trusted internal producers, so we deliberately do NOT honor
// a client-supplied X-Forwarded-For here — that would let a producer forge the
// source IP on its own audit record.
func auditClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func auditParseInt(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
