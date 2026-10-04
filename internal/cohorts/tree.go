package cohorts

// Rule trees (C2 depth, 2026-10): OR, NOT and nesting on top of the v1
// flat-AND definition.
//
// A Definition is either
//
//   - legacy flat:  {"op":"and","rules":[<rule>,...]}   (v1, still read and
//     written by older clients; "op":"or" over rules is also accepted now),
//   - a tree:       {"op":"and|or|not","children":[<node>,...]} whose leaves
//     are {"leaf":<rule>} nodes,
//   - static:       {"op":"static"} - membership lives in cohort_members.
//
// Evaluation stays the strategy v1 used: each leaf is one bounded query
// returning a set of distinct_ids, and the tree is combined in Go with set
// algebra (intersection / union / difference). No subqueries or engine-side
// boolean composition are involved, so nothing new is asked of Nucleus.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Limits on a rule tree, enforced on write.
const (
	// MaxTreeDepth is the longest root-to-leaf path in nodes. A flat v1
	// definition is depth 2 (the root group plus its leaves).
	MaxTreeDepth = 4
	// MaxTreeLeaves is the largest number of leaf conditions in one tree.
	MaxTreeLeaves = 30
	// MaxLeafRows bounds how many rows one leaf query may return. Past it
	// the evaluation fails (ErrTooLarge) instead of materialising an
	// unbounded set in memory.
	MaxLeafRows = 200000
	// MaxEvalSet bounds an intermediate set produced by a union or a
	// complement, for the same reason.
	MaxEvalSet = 1000000
)

// OpStatic marks a static (list-backed) cohort definition.
const OpStatic = "static"

// ErrInvalidDefinition is returned (wrapped) for a rule definition that fails
// validation; handlers map it to 422.
var ErrInvalidDefinition = errors.New("invalid cohort definition")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, fmt.Sprintf(format, args...))
}

// node is the normalised internal form of a Definition: a group (op +
// kids) or a leaf (leaf != nil).
type node struct {
	op   string
	kids []node
	leaf *Rule
}

// normalize converts a Definition (legacy flat, tree or leaf) to a node.
// It is purely structural; validate does the checking.
func normalize(def Definition) node {
	if def.Leaf != nil {
		return node{leaf: def.Leaf}
	}
	op := def.Op
	if op == "" {
		op = "and"
	}
	n := node{op: op}
	for i := range def.Rules {
		r := def.Rules[i]
		n.kids = append(n.kids, node{leaf: &r})
	}
	for _, c := range def.Children {
		n.kids = append(n.kids, normalize(c))
	}
	return n
}

// ValidateDefinition checks a definition for a write. It rejects bad shape
// (unknown op, wrong child counts, depth/leaf limits, a leaf mixed with
// children) and, per leaf, an unusable rule (empty event name, property key
// outside the allow-list, unsupported operator, malformed window).
func ValidateDefinition(def Definition) error {
	return validateDefinition(def, true)
}

// validateShape is the evaluation-time subset: structure and limits only.
// Leaf field problems surface from the leaf evaluators exactly as in v1, so
// a rule saved before write validation existed keeps its old behaviour.
func validateShape(def Definition) error {
	return validateDefinition(def, false)
}

func validateDefinition(def Definition, strictLeaves bool) error {
	if def.Op == OpStatic {
		if def.Leaf != nil || len(def.Rules) != 0 || len(def.Children) != 0 {
			return invalidf("a static cohort carries no rules")
		}
		return nil
	}
	if err := checkMixing(def); err != nil {
		return err
	}
	n := normalize(def)
	leaves := 0
	if err := checkNode(n, 1, &leaves, strictLeaves); err != nil {
		return err
	}
	if leaves == 0 {
		return invalidf("at least one condition is required")
	}
	if leaves > MaxTreeLeaves {
		return invalidf("%d conditions, limit %d", leaves, MaxTreeLeaves)
	}
	return nil
}

// checkMixing rejects ambiguous nodes before normalisation flattens them.
func checkMixing(def Definition) error {
	if def.Leaf != nil {
		if def.Op != "" || len(def.Rules) != 0 || len(def.Children) != 0 {
			return invalidf("a leaf node carries no op, rules or children")
		}
		return nil
	}
	if len(def.Rules) != 0 && len(def.Children) != 0 {
		return invalidf("use either rules (flat) or children (tree), not both")
	}
	for _, c := range def.Children {
		if c.Op == OpStatic {
			return invalidf("static is a top-level cohort kind, not a tree node")
		}
		if err := checkMixing(c); err != nil {
			return err
		}
	}
	return nil
}

func checkNode(n node, depth int, leaves *int, strict bool) error {
	if depth > MaxTreeDepth {
		return invalidf("nesting deeper than %d levels", MaxTreeDepth)
	}
	if n.leaf != nil {
		*leaves++
		if strict {
			return validateRule(*n.leaf)
		}
		return nil
	}
	switch n.op {
	case "and", "or":
		if len(n.kids) == 0 {
			return invalidf("%q group needs at least one child", n.op)
		}
	case "not":
		if len(n.kids) != 1 {
			return invalidf("\"not\" takes exactly one child, got %d", len(n.kids))
		}
	default:
		return invalidf("unknown op %q (supported: and, or, not)", n.op)
	}
	for _, k := range n.kids {
		if err := checkNode(k, depth+1, leaves, strict); err != nil {
			return err
		}
	}
	return nil
}

var windowRe = regexp.MustCompile(`^[0-9]{1,4}[dhm]$`)

const (
	maxRuleStr      = 512
	maxRuleMinCount = 1000000
)

// validateRule is the strict write-time check of one leaf.
func validateRule(r Rule) error {
	if len(r.Name) > maxRuleStr || len(r.Value) > maxRuleStr {
		return invalidf("rule name/value longer than %d bytes", maxRuleStr)
	}
	switch r.Type {
	case "event":
		if strings.TrimSpace(r.Name) == "" {
			return invalidf("event rule requires name")
		}
		if r.MinCount < 0 || r.MinCount > maxRuleMinCount {
			return invalidf("min_count out of range")
		}
		if r.Window != "" && !windowRe.MatchString(strings.ToLower(strings.TrimSpace(r.Window))) {
			return invalidf("window %q must look like 24h, 7d or 90m", r.Window)
		}
	case "property":
		if r.Key == "" {
			return invalidf("property rule requires key")
		}
		if !isAllowedPropertyKey(r.Key) {
			return invalidf("property key %q not allowed (allowed: %s)", r.Key, strings.Join(allowedPropertyKeys(), ", "))
		}
		if r.Operator != "" && r.Operator != "=" && r.Operator != "!=" {
			return invalidf("operator %q not supported (supported: =, !=)", r.Operator)
		}
	default:
		return invalidf("unsupported rule type %q (supported: event, property)", r.Type)
	}
	return nil
}

// idSet is the unit of the set algebra.
type idSet map[string]struct{}

func toSet(ids []string) idSet {
	s := make(idSet, len(ids))
	for _, id := range ids {
		if id != "" {
			s[id] = struct{}{}
		}
	}
	return s
}

func (s idSet) sorted() []string {
	out := make([]string, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// intersectSets returns a new set with the ids present in both.
func intersectSets(a, b idSet) idSet {
	if len(b) < len(a) {
		a, b = b, a
	}
	out := make(idSet, len(a))
	for id := range a {
		if _, ok := b[id]; ok {
			out[id] = struct{}{}
		}
	}
	return out
}

// unionSets returns a new set with the ids present in either; it fails
// (ErrTooLarge) rather than building a set larger than MaxEvalSet.
func unionSets(a, b idSet) (idSet, error) {
	if len(a)+len(b) > MaxEvalSet {
		// Only an upper bound; count exactly before refusing.
		n := len(a)
		for id := range b {
			if _, ok := a[id]; !ok {
				n++
			}
		}
		if n > MaxEvalSet {
			return nil, fmt.Errorf("%w: intermediate set over %d ids", ErrTooLarge, MaxEvalSet)
		}
	}
	out := make(idSet, len(a)+len(b))
	for id := range a {
		out[id] = struct{}{}
	}
	for id := range b {
		out[id] = struct{}{}
	}
	return out, nil
}

// differenceSets returns the ids of a that are not in b.
func differenceSets(a, b idSet) idSet {
	out := make(idSet, len(a))
	for id := range a {
		if _, ok := b[id]; !ok {
			out[id] = struct{}{}
		}
	}
	return out
}

// treeEnv supplies the two data sources a tree evaluation needs; tests
// substitute in-memory fakes.
type treeEnv struct {
	leaf     func(ctx context.Context, r Rule) (idSet, error)
	universe func(ctx context.Context) (idSet, error)
}

// evalNode evaluates a node to its member set. "and" short-circuits on an
// empty intersection; "not" is the complement against the site's identified
// users (the universe), loaded at most once per evaluation.
func evalNode(ctx context.Context, n node, env treeEnv) (idSet, error) {
	if n.leaf != nil {
		return env.leaf(ctx, *n.leaf)
	}
	switch n.op {
	case "and":
		var acc idSet
		for i, k := range n.kids {
			s, err := evalNode(ctx, k, env)
			if err != nil {
				return nil, err
			}
			if i == 0 {
				acc = s
			} else {
				acc = intersectSets(acc, s)
			}
			if len(acc) == 0 {
				return idSet{}, nil
			}
		}
		return acc, nil
	case "or":
		acc := idSet{}
		for _, k := range n.kids {
			s, err := evalNode(ctx, k, env)
			if err != nil {
				return nil, err
			}
			if acc, err = unionSets(acc, s); err != nil {
				return nil, err
			}
		}
		return acc, nil
	case "not":
		if len(n.kids) != 1 {
			return nil, invalidf("\"not\" takes exactly one child")
		}
		s, err := evalNode(ctx, n.kids[0], env)
		if err != nil {
			return nil, err
		}
		all, err := env.universe(ctx)
		if err != nil {
			return nil, err
		}
		return differenceSets(all, s), nil
	}
	return nil, invalidf("unknown op %q", n.op)
}
