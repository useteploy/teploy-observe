package query

// O03/O04 entity model for the session-keyed query surfaces (funnels,
// retention). Vocabulary is the identity-model ADR's, verbatim
// (docs/IDENTITY_MODEL_ADR.md §2); decision D2 requires every surface to
// name its grouped entity and carry the honest limitation of that entity.
//
//	visitor-estimate  events.session_id   the monthly fingerprint
//	                                     estimate — NOT people
//	visit             events.visit_id    one UTC clock hour of one
//	                                     estimate (the column naming is
//	                                     inverted; see ADR §2)
//	person            events.distinct_id identified events only — the
//                                     anonymous bookends of a person's
//                                     activity are invisible (no
//                                     stitching in era-1)
//
// The default (empty string) is visitor-estimate: the pre-O04 behavior,
// now labeled for what it is.

import (
	"fmt"

	"github.com/neutron-build/neutron/go/neutron"
)

const (
	EntityVisitorEstimate = "visitor-estimate"
	EntityVisit           = "visit"
	EntityPerson          = "person"
)

// EntityLimitation is the one-line D2 honesty string for a mode.
func EntityLimitation(entity string) string {
	switch entity {
	case EntityVisit:
		return "visit entity: one UTC clock hour of one visitor estimate — sequences spanning an hour boundary split across visits"
	case EntityPerson:
		return "person entity: identified events only — anonymous events are excluded and pre-identify activity is invisible (no anonymous-to-person stitching)"
	default:
		return "visitor-estimate entity: monthly site+IP+UA+salt fingerprint — an estimate, not people (merges same-browser users behind one NAT; splits across devices, UA changes, salt eras, and UTC month boundaries)"
	}
}

// ValidateEntity accepts the empty default and the three ADR names.
func ValidateEntity(entity string) error {
	switch entity {
	case "", EntityVisitorEstimate, EntityVisit, EntityPerson:
		return nil
	}
	return neutron.ErrBadRequest(fmt.Sprintf("unknown entity %q: expected one of visit, person, visitor-estimate", entity))
}

// entityKeyOf returns the grouping key of an event under a mode. ok is
// false only for person-mode anonymous events (distinct_id ”), which
// belong to no person and must never merge into a shared "" group.
func entityKeyOf(entity string, e funnelEvent) (string, bool) {
	switch entity {
	case EntityVisit:
		return e.VisitID, true
	case EntityPerson:
		if e.DistinctID == "" {
			return "", false
		}
		return e.DistinctID, true
	default:
		return e.SessionID, true
	}
}

// groupFunnelByEntity partitions events into per-entity groups for the
// walk. Map iteration order never matters: the walk is per group and the
// counts are order-independent sums.
func groupFunnelByEntity(rows []funnelEvent, entity string) map[string][]funnelEvent {
	groups := make(map[string][]funnelEvent)
	for _, e := range rows {
		if key, ok := entityKeyOf(entity, e); ok {
			groups[key] = append(groups[key], e)
		}
	}
	return groups
}
