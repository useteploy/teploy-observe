package cohorts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func ev(name string) Rule   { return Rule{Type: "event", Name: name} }
func prop(k, v string) Rule { return Rule{Type: "property", Key: k, Value: v} }

func leaf(r Rule) Definition { return Definition{Leaf: &r} }

func grp(op string, kids ...Definition) Definition { return Definition{Op: op, Children: kids} }

// chain nests n "and" groups around one leaf: depth n+1.
func chain(n int) Definition {
	d := leaf(ev("x"))
	for i := 0; i < n; i++ {
		d = grp("and", d)
	}
	return d
}

func TestValidateDefinitionTable(t *testing.T) {
	manyLeaves := func(n int) Definition {
		kids := make([]Definition, n)
		for i := range kids {
			kids[i] = leaf(ev(fmt.Sprintf("e%d", i)))
		}
		return grp("or", kids...)
	}
	flat := func(n int) Definition {
		rules := make([]Rule, n)
		for i := range rules {
			rules[i] = ev(fmt.Sprintf("e%d", i))
		}
		return Definition{Op: "and", Rules: rules}
	}
	cases := []struct {
		name string
		def  Definition
		ok   bool
	}{
		{"legacy flat and", Definition{Op: "and", Rules: []Rule{ev("a"), prop("country", "US")}}, true},
		{"legacy flat empty op", Definition{Rules: []Rule{ev("a")}}, true},
		{"legacy flat or", Definition{Op: "or", Rules: []Rule{ev("a"), ev("b")}}, true},
		{"single leaf root", leaf(ev("a")), true},
		{"or of leaves", grp("or", leaf(ev("a")), leaf(ev("b"))), true},
		{"and of or", grp("and", leaf(ev("a")), grp("or", leaf(ev("b")), leaf(ev("c")))), true},
		{"not one child", grp("not", leaf(ev("a"))), true},
		{"static", Definition{Op: OpStatic}, true},

		{"empty", Definition{}, false},
		{"empty and group", grp("and"), false},
		{"empty or group", grp("or"), false},
		{"not no child", grp("not"), false},
		{"not two children", grp("not", leaf(ev("a")), leaf(ev("b"))), false},
		{"unknown op", grp("xor", leaf(ev("a"))), false},
		{"rules and children mixed", Definition{Op: "and", Rules: []Rule{ev("a")}, Children: []Definition{leaf(ev("b"))}}, false},
		{"leaf with op", Definition{Op: "and", Leaf: &Rule{Type: "event", Name: "a"}}, false},
		{"leaf with children", Definition{Leaf: &Rule{Type: "event", Name: "a"}, Children: []Definition{leaf(ev("b"))}}, false},
		{"static with rules", Definition{Op: OpStatic, Rules: []Rule{ev("a")}}, false},
		{"static nested", grp("and", Definition{Op: OpStatic}), false},
		{"event no name", leaf(Rule{Type: "event"}), false},
		{"unknown rule type", leaf(Rule{Type: "cohort", Name: "x"}), false},
		{"property bad key", leaf(prop("password", "x")), false},
		{"property sql in key", leaf(prop("country; DROP TABLE events", "x")), false},
		{"property bad operator", leaf(Rule{Type: "property", Key: "country", Operator: ">", Value: "x"}), false},
		{"property neq ok", leaf(Rule{Type: "property", Key: "country", Operator: "!=", Value: "x"}), true},
		{"bad window", leaf(Rule{Type: "event", Name: "a", Window: "forever"}), false},
		{"good window", leaf(Rule{Type: "event", Name: "a", Window: "90m"}), true},
		{"negative min_count", leaf(Rule{Type: "event", Name: "a", MinCount: -1}), false},
		{"oversize name", leaf(Rule{Type: "event", Name: strings.Repeat("a", maxRuleStr+1)}), false},

		// Depth: root group = 1, so chain(3) is 4 levels (ok), chain(4) is 5.
		{"depth 4 ok", chain(3), true},
		{"depth 5 refused", chain(4), false},
		{"flat 30 leaves ok", flat(MaxTreeLeaves), true},
		{"flat 31 leaves refused", flat(MaxTreeLeaves + 1), false},
		{"tree 30 leaves ok", manyLeaves(MaxTreeLeaves), true},
		{"tree 31 leaves refused", manyLeaves(MaxTreeLeaves + 1), false},
	}
	for _, tc := range cases {
		err := ValidateDefinition(tc.def)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: expected error", tc.name)
			} else if !errors.Is(err, ErrInvalidDefinition) {
				t.Errorf("%s: error %v does not wrap ErrInvalidDefinition", tc.name, err)
			}
		}
	}
}

func TestLegacyJSONStillParses(t *testing.T) {
	// Exactly what v1 wrote.
	raw := `{"op":"and","rules":[{"type":"event","name":"purchase","window":"30d","min_count":2},{"type":"property","key":"country","operator":"=","value":"US"}]}`
	def, err := ParseDefinition(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDefinition(def); err != nil {
		t.Fatalf("legacy definition no longer valid: %v", err)
	}
	n := normalize(def)
	if n.op != "and" || len(n.kids) != 2 || n.kids[0].leaf == nil || n.kids[0].leaf.Name != "purchase" {
		t.Fatalf("normalize(legacy) = %+v", n)
	}
	// v1 also stored an empty rules array with no omitempty.
	def, err = ParseDefinition(`{"op":"and","rules":null}`)
	if err != nil || len(def.Rules) != 0 {
		t.Fatalf("%v %+v", err, def)
	}
}

func TestTreeJSONRoundTrip(t *testing.T) {
	raw := `{"op":"and","children":[{"leaf":{"type":"event","name":"signup"}},{"op":"not","children":[{"leaf":{"type":"property","key":"country","value":"US"}}]}]}`
	def, err := ParseDefinition(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDefinition(def); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(def)
	var again Definition
	if err := json.Unmarshal(out, &again); err != nil || !reflect.DeepEqual(def, again) {
		t.Fatalf("round trip: %v\n%s", err, out)
	}
	// A bare leaf root must not be defaulted to op "and".
	d, _ := ParseDefinition(`{"leaf":{"type":"event","name":"a"}}`)
	if d.Op != "" || ValidateDefinition(d) != nil {
		t.Fatalf("leaf root: %+v", d)
	}
}

func TestShapeCheckIsLenientOnLeafFields(t *testing.T) {
	// A pre-validation legacy rule with a junk window must still evaluate
	// (parseWindow defaults it) - only the write path is strict.
	d := Definition{Op: "and", Rules: []Rule{{Type: "event", Name: "a", Window: "soon"}}}
	if validateShape(d) != nil {
		t.Fatal("shape check must not police leaf fields")
	}
	if ValidateDefinition(d) == nil {
		t.Fatal("write validation must reject it")
	}
}

func setOf(ids ...string) idSet { return toSet(ids) }

func TestSetAlgebra(t *testing.T) {
	a, b := setOf("1", "2", "3"), setOf("2", "3", "4")
	if got := intersectSets(a, b).sorted(); !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Errorf("intersect = %v", got)
	}
	if got := intersectSets(b, a).sorted(); !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Errorf("intersect commutes: %v", got)
	}
	u, err := unionSets(a, b)
	if err != nil || !reflect.DeepEqual(u.sorted(), []string{"1", "2", "3", "4"}) {
		t.Errorf("union = %v %v", u.sorted(), err)
	}
	if got := differenceSets(a, b).sorted(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("difference = %v", got)
	}
	if got := differenceSets(a, a); len(got) != 0 {
		t.Errorf("a - a = %v", got)
	}
	// Inputs are never mutated.
	if len(a) != 3 || len(b) != 3 {
		t.Errorf("inputs mutated: %v %v", a, b)
	}
	// Anonymous ids never enter a set.
	if len(toSet([]string{"", "x"})) != 1 {
		t.Error("empty id kept")
	}
	// De Morgan: not(a or b) == not(a) and not(b), over a universe.
	all := setOf("1", "2", "3", "4", "5")
	ab, _ := unionSets(a, b)
	left := differenceSets(all, ab)
	right := intersectSets(differenceSets(all, a), differenceSets(all, b))
	if !reflect.DeepEqual(left.sorted(), right.sorted()) {
		t.Errorf("de morgan: %v vs %v", left.sorted(), right.sorted())
	}
}

// fakeEnv resolves leaves by event/property name from fixed sets and counts
// universe loads.
func fakeEnv(leaves map[string][]string, universe []string, loads *int) treeEnv {
	return treeEnv{
		leaf: func(_ context.Context, r Rule) (idSet, error) {
			key := r.Name
			if r.Type == "property" {
				key = r.Key + "=" + r.Value
			}
			ids, ok := leaves[key]
			if !ok {
				return nil, fmt.Errorf("fake: no leaf %q", key)
			}
			return toSet(ids), nil
		},
		universe: func(context.Context) (idSet, error) {
			*loads++
			return toSet(universe), nil
		},
	}
}

func TestEvalNode(t *testing.T) {
	leaves := map[string][]string{
		"a":          {"u1", "u2", "u3"},
		"b":          {"u3", "u4"},
		"c":          {"u9"},
		"country=US": {"u1", "u4"},
	}
	universe := []string{"u1", "u2", "u3", "u4", "u5", "u9"}
	cases := []struct {
		name string
		def  Definition
		want []string
	}{
		{"legacy and", Definition{Op: "and", Rules: []Rule{ev("a"), ev("b")}}, []string{"u3"}},
		{"legacy or", Definition{Op: "or", Rules: []Rule{ev("a"), ev("b")}}, []string{"u1", "u2", "u3", "u4"}},
		{"leaf", leaf(ev("c")), []string{"u9"}},
		{"not leaf", grp("not", leaf(ev("a"))), []string{"u4", "u5", "u9"}},
		{"and with not", grp("and", leaf(ev("a")), grp("not", leaf(ev("b")))), []string{"u1", "u2"}},
		{"or of and", grp("or",
			grp("and", leaf(ev("a")), leaf(prop("country", "US"))),
			leaf(ev("c"))), []string{"u1", "u9"}},
		{"nested not not", grp("not", grp("not", leaf(ev("b")))), []string{"u3", "u4"}},
		{"empty intersection", grp("and", leaf(ev("c")), leaf(ev("a"))), []string{}},
	}
	for _, tc := range cases {
		loads := 0
		got, err := evalNode(context.Background(), normalize(tc.def), fakeEnv(leaves, universe, &loads))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got.sorted(), tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got.sorted(), tc.want)
		}
	}
}

func TestEvalNodeLeafErrorPropagates(t *testing.T) {
	loads := 0
	_, err := evalNode(context.Background(), normalize(grp("or", leaf(ev("a")), leaf(ev("missing")))),
		fakeEnv(map[string][]string{"a": {"u1"}}, nil, &loads))
	if err == nil {
		t.Fatal("a failing leaf must fail the whole evaluation, not be dropped from the OR")
	}
}

func TestEvalNodeAndShortCircuits(t *testing.T) {
	calls := 0
	env := treeEnv{
		leaf: func(_ context.Context, r Rule) (idSet, error) {
			calls++
			if r.Name == "empty" {
				return idSet{}, nil
			}
			return setOf("u1"), nil
		},
		universe: func(context.Context) (idSet, error) { return idSet{}, nil },
	}
	d := Definition{Op: "and", Rules: []Rule{ev("empty"), ev("x"), ev("y")}}
	got, err := evalNode(context.Background(), normalize(d), env)
	if err != nil || len(got) != 0 || calls != 1 {
		t.Fatalf("got %v err %v calls %d", got, err, calls)
	}
}

func TestUnionCap(t *testing.T) {
	big := make(idSet, MaxEvalSet)
	for i := 0; i < MaxEvalSet; i++ {
		big[fmt.Sprint(i)] = struct{}{}
	}
	if _, err := unionSets(big, setOf("extra-1")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("union past cap: %v", err)
	}
	// Overlapping union that stays within the cap is fine.
	if _, err := unionSets(big, setOf("1", "2")); err != nil {
		t.Fatalf("overlap union: %v", err)
	}
}

func TestLeafLimitSQL(t *testing.T) {
	if !strings.HasSuffix(leafLimitSQL(), fmt.Sprint(MaxLeafRows+1)) || !strings.HasPrefix(leafLimitSQL(), " LIMIT ") {
		t.Fatalf("leafLimitSQL = %q", leafLimitSQL())
	}
}

func TestEvaluateCohortRejectsBeforeQuerying(t *testing.T) {
	// A nil-db Service must refuse malformed trees without touching the db.
	s := &Service{}
	bad := []Definition{
		grp("xor", leaf(ev("a"))),
		grp("not", leaf(ev("a")), leaf(ev("b"))),
		chain(MaxTreeDepth),
		{Op: OpStatic},
	}
	for i, d := range bad {
		if _, err := s.EvaluateCohort(context.Background(), "site", d); !errors.Is(err, ErrInvalidDefinition) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if ids, err := s.EvaluateCohort(context.Background(), "site", Definition{}); err != nil || len(ids) != 0 {
		t.Errorf("empty def: %v %v", ids, err)
	}
}
