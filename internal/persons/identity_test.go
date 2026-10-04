package persons

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var bg = context.Background()

func seed(m *Memory, site string, keys ...string) {
	for i, k := range keys {
		m.AddPerson(site, Person{
			DistinctID: k, FirstSeenMs: int64(100 + i), LastSeenMs: int64(1000 + i*10),
			EventCount: 2, SessionCount: 1, TopCountry: "US", TopBrowser: "b" + k,
		})
	}
}

func TestValidateProperties_Bounds(t *testing.T) {
	many := map[string]any{}
	for i := 0; i < MaxPropertyKeys+1; i++ {
		many[fmt.Sprintf("k%d", i)] = 1
	}
	cases := map[string]map[string]any{
		"too many keys":  many,
		"bad key":        {"bad key": 1},
		"key injection":  {"a'; DROP TABLE events;--": 1},
		"reserved":       {"distinct_id": "raw"},
		"reserved case":  {"User_ID": "raw"},
		"nested object":  {"a": map[string]any{"b": 1}},
		"array":          {"a": []any{1}},
		"oversize value": {"a": strings.Repeat("x", MaxPropertyValueLen)},
		"long key":       {strings.Repeat("a", MaxPropertyKeyLen+1): 1},
	}
	for name, in := range cases {
		if _, _, err := ValidateProperties(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	set, rm, err := ValidateProperties(map[string]any{"plan": "pro", "n": 3.0, "ok": true, "gone": nil, "edge": strings.Repeat("x", MaxPropertyValueLen-2)})
	if err != nil || len(set) != 4 || len(rm) != 1 || rm[0] != "gone" {
		t.Fatalf("valid batch: set=%v rm=%v err=%v", set, rm, err)
	}
}

func TestValidatePersonKey(t *testing.T) {
	for _, k := range []string{"", "a b", "a/b", "a'b", strings.Repeat("a", MaxPersonKeyLen+1), "x;y"} {
		if ValidatePersonKey(k) == nil {
			t.Errorf("%q should be invalid", k)
		}
	}
	for _, k := range []string{"0123456789abcdef", "u@x.io", "a_b-c.d:e"} {
		if err := ValidatePersonKey(k); err != nil {
			t.Errorf("%q should be valid: %v", k, err)
		}
	}
}

func TestResolver_ChainAndCycle(t *testing.T) {
	r := NewResolver([]AliasRow{{"a", "b"}, {"b", "c"}})
	if r.Canonical("a") != "c" || r.Canonical("c") != "c" || r.Canonical("zzz") != "zzz" {
		t.Fatal("chain resolution wrong")
	}
	if m := r.Members("c"); len(m) != 2 || m[0] != "a" || m[1] != "b" {
		t.Fatalf("members = %v", m)
	}
	cyc := NewResolver([]AliasRow{{"x", "y"}, {"y", "x"}})
	if cyc.Canonical("x") != "x" || cyc.Canonical("y") != "y" {
		t.Fatal("corrupt cycle must resolve to self")
	}
}

func TestMerge_Protections(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	seed(m, "s1", "a", "b", "c")
	seed(m, "s2", "z")

	if _, err := s.Merge(bg, "s1", "a", "a", "u"); !errors.Is(err, ErrInvalid) {
		t.Errorf("self-merge: %v", err)
	}
	if _, err := s.Merge(bg, "s1", "a", "b", "u"); err != nil {
		t.Fatalf("merge a->b: %v", err)
	}
	if _, err := s.Merge(bg, "s1", "a", "c", "u"); !errors.Is(err, ErrConflict) {
		t.Errorf("re-merging an alias: %v", err)
	}
	// b -> a would close a cycle (a is already an alias of b).
	if _, err := s.Merge(bg, "s1", "b", "a", "u"); !errors.Is(err, ErrConflict) {
		t.Errorf("cycle: %v", err)
	}
	// into an alias resolves to its root: c -> a  == c -> b.
	res, err := s.Merge(bg, "s1", "c", "a", "u")
	if err != nil || res.CanonicalKey != "b" {
		t.Errorf("merge into alias should canonicalize: %+v %v", res, err)
	}
	// Cross-site key (IDOR): z exists only in s2.
	if _, err := s.Merge(bg, "s1", "z", "b", "u"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-site key must be not found: %v", err)
	}
	if _, err := s.Merge(bg, "s1", "nope", "b", "u"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := s.Merge(bg, "", "a", "b", "u"); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty site: %v", err)
	}
	if _, err := s.Merge(bg, "s1", "a;b", "b", "u"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad key: %v", err)
	}
}

func TestMerge_AliasFanoutLimit(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	seed(m, "s1", "root")
	for i := 0; i < MaxAliasesPerPerson+2; i++ {
		seed(m, "s1", fmt.Sprintf("k%d", i))
	}
	var lastErr error
	for i := 0; i < MaxAliasesPerPerson+2; i++ {
		_, lastErr = s.Merge(bg, "s1", fmt.Sprintf("k%d", i), "root", "u")
		if lastErr != nil {
			break
		}
	}
	if !errors.Is(lastErr, ErrLimit) {
		t.Fatalf("want ErrLimit, got %v", lastErr)
	}
}

func TestListResolved_FoldsAliasesAndPaginates(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	seed(m, "s1", "a", "b", "c", "d") // last_seen a<b<c<d
	if _, err := s.Merge(bg, "s1", "d", "a", "u"); err != nil {
		t.Fatal(err)
	}
	r, err := s.ListResolved(bg, "s1", 0, 0, 2, 0, false)
	if err != nil || !r.Resolved || r.Total != 3 || len(r.Persons) != 2 {
		t.Fatalf("list: %+v err=%v", r, err)
	}
	if r.Persons[0].DistinctID != "a" || r.Persons[0].EventCount != 4 || r.Persons[0].LastSeenMs != 1030 || r.Persons[0].FirstSeenMs != 100 {
		t.Fatalf("folded person wrong: %+v", r.Persons[0])
	}
	r2, _ := s.ListResolved(bg, "s1", 0, 0, 2, 2, false)
	if len(r2.Persons) != 1 || r2.Total != 3 {
		t.Fatalf("page 2: %+v", r2)
	}
	// Other site sees nothing of s1's aliases.
	seed(m, "s2", "a", "d")
	o, _ := s.ListResolved(bg, "s2", 0, 0, 10, 0, false)
	if o.Total != 2 {
		t.Fatalf("aliases must not cross sites: %+v", o)
	}
}

func TestListResolved_FastPathWhenNoIdentityData(t *testing.T) {
	m := NewMemory()
	seed(m, "s1", "a", "b")
	r, err := m.Service().ListResolved(bg, "s1", 0, 0, 10, 0, false)
	if err != nil || r.Resolved || r.Total != 2 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSetProperties_MergeReplaceAndBounds(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	if _, err := s.SetProperties(bg, "s1", "k1", map[string]any{"plan": "free", "a": 1.0}, false); err != nil {
		t.Fatal(err)
	}
	doc, err := s.SetProperties(bg, "s1", "k1", map[string]any{"plan": "pro", "a": nil}, false)
	if err != nil || doc["plan"] != "pro" || len(doc) != 1 {
		t.Fatalf("merge: %v %v", doc, err)
	}
	doc, _ = s.SetProperties(bg, "s1", "k1", map[string]any{"x": "y"}, true)
	if len(doc) != 1 || doc["x"] != "y" {
		t.Fatalf("replace: %v", doc)
	}
	// The merged document may not exceed 50 keys either.
	big := map[string]any{}
	for i := 0; i < MaxPropertyKeys; i++ {
		big[fmt.Sprintf("k%d", i)] = i
	}
	if _, err := s.SetProperties(bg, "s1", "k2", big, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProperties(bg, "s1", "k2", map[string]any{"extra": 1}, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("51st key via merge must fail: %v", err)
	}
	// Site isolation: s2 does not see s1's properties.
	if _, found, _ := m.GetProps(bg, "s2", "k1"); found {
		t.Fatal("properties leaked across sites")
	}
}

func TestDetail_ResolvesAliasAndMergesProperties(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	seed(m, "s1", "a", "b")
	_, _ = s.SetProperties(bg, "s1", "a", map[string]any{"plan": "alias", "only_alias": "x"}, false)
	_, _ = s.SetProperties(bg, "s1", "b", map[string]any{"plan": "canon"}, false)
	if _, err := s.Merge(bg, "s1", "a", "b", "u"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b"} {
		d, err := s.PersonDetail(bg, "s1", k)
		if err != nil {
			t.Fatal(err)
		}
		if d.CanonicalKey != "b" || d.Person.DistinctID != "b" || d.Person.EventCount != 4 || len(d.Aliases) != 1 || d.Aliases[0] != "a" {
			t.Fatalf("detail(%s): %+v", k, d)
		}
		if d.Properties["plan"] != "canon" || d.Properties["only_alias"] != "x" {
			t.Fatalf("properties precedence wrong: %v", d.Properties)
		}
		if len(d.Timeline) != 2 {
			t.Fatalf("timeline = %d", len(d.Timeline))
		}
	}
	// Detail in another site is empty, not a leak.
	d, _ := s.PersonDetail(bg, "s2", "b")
	if d.Person.EventCount != 0 || len(d.Properties) != 0 {
		t.Fatalf("cross-site detail leaked: %+v", d)
	}
}

func TestErase_TombstoneGroupAndExclusion(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	seed(m, "s1", "a", "b", "c")
	seed(m, "s2", "b")
	_, _ = s.SetProperties(bg, "s1", "a", map[string]any{"p": 1.0}, false)
	_, _ = s.SetProperties(bg, "s2", "b", map[string]any{"p": 2.0}, false)
	if _, err := s.Merge(bg, "s1", "a", "b", "u"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Erase(bg, "s1", "b", "admin")
	if err != nil || len(res.Erased) != 2 || res.EventsDeleted {
		t.Fatalf("erase: %+v %v", res, err)
	}
	if _, err := s.PersonDetail(bg, "s1", "b"); !errors.Is(err, ErrErased) {
		t.Fatalf("detail after erase: %v", err)
	}
	if _, err := s.PersonDetail(bg, "s1", "a"); !errors.Is(err, ErrErased) {
		t.Fatalf("alias detail after erase: %v", err)
	}
	l, _ := s.ListResolved(bg, "s1", 0, 0, 10, 0, false)
	if l.Total != 1 || l.Persons[0].DistinctID != "c" {
		t.Fatalf("list after erase: %+v", l)
	}
	if raw, _, _ := m.GetProps(bg, "s1", "a"); raw != "{}" {
		t.Fatalf("props not cleared: %q", raw)
	}
	if _, err := s.SetProperties(bg, "s1", "b", map[string]any{"p": 1.0}, false); !errors.Is(err, ErrErased) {
		t.Fatalf("write after erase: %v", err)
	}
	if _, err := s.Merge(bg, "s1", "c", "b", "u"); !errors.Is(err, ErrErased) {
		t.Fatalf("merge into erased: %v", err)
	}
	// Idempotent.
	if _, err := s.Erase(bg, "s1", "b", "admin"); err != nil {
		t.Fatal(err)
	}
	// IDOR: erasing in s1 leaves s2's same-named key untouched.
	if _, err := s.PersonDetail(bg, "s2", "b"); err != nil {
		t.Fatalf("s2 person must survive: %v", err)
	}
	if raw, _, _ := m.GetProps(bg, "s2", "b"); raw == "{}" {
		t.Fatal("s2 properties erased by s1 erase")
	}
}
