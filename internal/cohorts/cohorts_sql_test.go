package cohorts

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These tests need no database: they pin the SQL the property rule builds
// and the Go-side set arithmetic used for the "!=" operator.

func TestPropertyMatchSQL(t *testing.T) {
	q := propertyMatchSQL("country")
	for _, want := range []string{"site_id = $1", "country = $2", "distinct_id != ''", "SELECT DISTINCT distinct_id"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}
	// The "!=" operator must never reach SQL as a row filter: that was the
	// bug (any non-matching event qualified the user).
	if strings.Contains(q, "!= $2") || strings.Contains(q, "<> $2") {
		t.Errorf("match query must be equality only:\n%s", q)
	}
	// The value is always a bind parameter.
	if strings.Contains(q, "'US'") {
		t.Errorf("value interpolated into SQL:\n%s", q)
	}
}

func TestPropertyUniverseSQL(t *testing.T) {
	q := propertyUniverseSQL()
	if !strings.Contains(q, "site_id = $1") || !strings.Contains(q, "distinct_id != ''") {
		t.Errorf("universe query must be site-scoped and exclude anonymous:\n%s", q)
	}
	if strings.Contains(q, "$2") {
		t.Errorf("universe query takes only the site parameter:\n%s", q)
	}
}

func TestSubtractIDs(t *testing.T) {
	// userBoth has a US and a DE event: "country = US" matches, so
	// "country != US" must exclude them (old behaviour included them).
	all := []string{"userBoth", "userDE", "userUS"}
	matchedUS := []string{"userBoth", "userUS"}
	got := subtractIDs(all, matchedUS)
	if want := []string{"userDE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractIDs = %v, want %v", got, want)
	}
	if got := subtractIDs(nil, matchedUS); len(got) != 0 {
		t.Errorf("empty universe: %v", got)
	}
	if got := subtractIDs(all, nil); !reflect.DeepEqual(got, all) {
		t.Errorf("nothing to remove: %v", got)
	}
}

func TestErrTooLargeWraps(t *testing.T) {
	err := fmt.Errorf("%w: %d members, limit %d", ErrTooLarge, MaxFilterMembers+1, MaxFilterMembers)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatal("ErrTooLarge must survive wrapping")
	}
	// Leave headroom under the 65535-parameter wire limit for the fixed
	// and dimension-filter parameters.
	if MaxFilterMembers > 60000 {
		t.Fatalf("MaxFilterMembers %d leaves no parameter headroom", MaxFilterMembers)
	}
}
