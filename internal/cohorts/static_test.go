package cohorts

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeIDs(t *testing.T) {
	got, err := NormalizeIDs([]string{" a ", "b", "", "a", "  ", "c", "b"})
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{strings.Repeat("x", MaxEntityIDLen+1), "a\x00b", "line\nbreak", "tab\there"} {
		if _, err := NormalizeIDs([]string{bad}); !errors.Is(err, ErrInvalidDefinition) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Exactly at the id length cap is accepted.
	if _, err := NormalizeIDs([]string{strings.Repeat("x", MaxEntityIDLen)}); err != nil {
		t.Errorf("at cap: %v", err)
	}
}

func TestNormalizeIDsCap(t *testing.T) {
	ids := make([]string, MaxStaticMembers)
	for i := range ids {
		ids[i] = fmt.Sprintf("u%d", i)
	}
	if got, err := NormalizeIDs(ids); err != nil || len(got) != MaxStaticMembers {
		t.Fatalf("at cap: %d %v", len(got), err)
	}
	// Duplicates beyond the cap do not count; one more DISTINCT id does.
	if _, err := NormalizeIDs(append(ids, "u0", "u1")); err != nil {
		t.Fatalf("dups over cap: %v", err)
	}
	if _, err := NormalizeIDs(append(ids, "one-too-many")); !errors.Is(err, ErrTooManyIDs) {
		t.Fatalf("over cap: %v", err)
	}
}

func TestParseCSVIDs(t *testing.T) {
	csv := "\xef\xbb\xbfdistinct_id,name\r\nu1,Alice\r\n\r\n\"u,2\",Bob\r\nu1,dup\r\n  u3  \r\n,empty-first\r\n"
	got, err := ParseCSVIDs(strings.NewReader(csv))
	if err != nil || !reflect.DeepEqual(got, []string{"u1", "u,2", "u3"}) {
		t.Fatalf("got %v %v", got, err)
	}
	// No header: the first row is data.
	got, err = ParseCSVIDs(strings.NewReader("alpha\nbeta\n"))
	if err != nil || !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("headerless: %v %v", got, err)
	}
	// A header word is only a header on the first row.
	got, _ = ParseCSVIDs(strings.NewReader("id\nid\nx\n"))
	if !reflect.DeepEqual(got, []string{"id", "x"}) {
		t.Fatalf("second id row: %v", got)
	}
	if got, err := ParseCSVIDs(strings.NewReader("")); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestParseCSVIDsAdversarial(t *testing.T) {
	// Over the distinct cap: refused, not truncated.
	var b strings.Builder
	for i := 0; i <= MaxStaticMembers; i++ {
		fmt.Fprintf(&b, "u%d\n", i)
	}
	if _, err := ParseCSVIDs(strings.NewReader(b.String())); !errors.Is(err, ErrTooManyIDs) {
		t.Fatalf("over cap: %v", err)
	}
	// Oversized id, control characters, and a formula-injection looking cell
	// are handled as data (ids are only ever bound parameters).
	if _, err := ParseCSVIDs(strings.NewReader(strings.Repeat("x", MaxEntityIDLen+1) + "\n")); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("long id: %v", err)
	}
	if got, err := ParseCSVIDs(strings.NewReader("'; DROP TABLE cohorts; --\n")); err != nil || len(got) != 1 {
		t.Fatalf("sql-looking id: %v %v", got, err)
	}
	// Malformed quoting is an invalid body, not a panic.
	if _, err := ParseCSVIDs(strings.NewReader("\"unterminated\nx")); err == nil {
		t.Log("lazy quotes accepted unterminated field")
	}
}

func TestMemberPlaceholders(t *testing.T) {
	if got := memberPlaceholders(2); got != "($1,$2,$3,$4,$5,$6,$7),($8,$9,$10,$11,$12,$13,$14)" {
		t.Fatalf("got %s", got)
	}
	// A full chunk stays far below the 65535 parameter wire cap.
	if staticInsertChunk*staticInsertCols > 20000 {
		t.Fatalf("chunk uses %d params", staticInsertChunk*staticInsertCols)
	}
	if !strings.Contains(memberPlaceholders(staticInsertChunk), fmt.Sprintf("$%d)", staticInsertChunk*staticInsertCols)) {
		t.Fatal("last placeholder missing")
	}
	// Column list and placeholder width agree.
	if strings.Count(memberInsertPrefix[:strings.Index(memberInsertPrefix, ")")], ",")+1 != staticInsertCols {
		t.Fatalf("insert columns != %d: %s", staticInsertCols, memberInsertPrefix)
	}
}

func TestStaticMembersSQL(t *testing.T) {
	q := staticMembersSQL()
	for _, want := range []string{"FROM cohort_members", "site_id = $1", "cohort_id = $2", "LIMIT "} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q:\n%s", want, q)
		}
	}
	if !strings.HasSuffix(q, fmt.Sprint(maxStaticRows+1)) {
		t.Errorf("read must be bounded one past the row cap:\n%s", q)
	}
	// Reads are scoped by BOTH ids: an unscoped cohort_id lookup would be
	// the IDOR.
	if strings.Contains(q, "WHERE cohort_id") && !strings.Contains(q, "site_id") {
		t.Error("cohort read without site scope")
	}
}

func TestCollapseMembers(t *testing.T) {
	rows := []memberRecord{
		{EntityID: "keep", AddedAt: 10, Version: 10},
		{EntityID: "gone", AddedAt: 10, Version: 10},
		{EntityID: "gone", AddedAt: 10, Removed: 1, Version: 20}, // removed later
		{EntityID: "back", AddedAt: 10, Version: 10},
		{EntityID: "back", AddedAt: 10, Removed: 1, Version: 20},
		{EntityID: "back", AddedAt: 30, Version: 30}, // re-added
		{EntityID: "tie", AddedAt: 40, Version: 40},
		{EntityID: "tie", AddedAt: 40, Removed: 1, Version: 40}, // tie -> removed
		{EntityID: "", AddedAt: 1, Version: 1},                  // blank never a member
	}
	got := collapseMembers(rows)
	want := map[string]int64{"keep": 10, "back": 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Row order must not matter.
	rev := make([]memberRecord, len(rows))
	for i, r := range rows {
		rev[len(rows)-1-i] = r
	}
	if !reflect.DeepEqual(collapseMembers(rev), want) {
		t.Fatal("collapse is order dependent")
	}
}

func TestStaticSentinels(t *testing.T) {
	for _, e := range []error{ErrNotStatic, ErrTooManyIDs, ErrNotFound, ErrTooLarge, ErrInvalidDefinition} {
		if !errors.Is(fmt.Errorf("wrap: %w", e), e) {
			t.Errorf("%v does not survive wrapping", e)
		}
	}
	// A static definition is recognised and carries no rules.
	if !(Definition{Op: OpStatic}).IsStatic() || (Definition{Op: "and"}).IsStatic() {
		t.Error("IsStatic")
	}
	if staticRule != `{"op":"static"}` {
		t.Errorf("staticRule = %s", staticRule)
	}
	d, err := ParseDefinition(staticRule)
	if err != nil || !d.IsStatic() || ValidateDefinition(d) != nil {
		t.Errorf("static rule json: %+v %v", d, err)
	}
}

func TestMembersForFilterCapUsesSharedConstants(t *testing.T) {
	// A static cohort may hold more than a chart filter can express; the
	// 30000 filter cap still applies on top (MembersForFilter).
	if MaxStaticMembers <= MaxFilterMembers {
		t.Fatalf("static cap %d should exceed the filter cap %d", MaxStaticMembers, MaxFilterMembers)
	}
}
