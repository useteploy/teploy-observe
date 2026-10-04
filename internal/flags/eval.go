package flags

// Pure flag evaluation: no storage, no clock, no randomness. Everything the
// server decides at /api/v1/flags/evaluate is decided here, and the same
// function is what SDKs reimplement for local evaluation against
// GET /api/v1/flags/config (see BucketingSpec and testdata/conformance.json).
//
// Stored-config compatibility. Two targeting formats coexist in the existing
// `targeting` column, distinguished by their first token:
//
//	legacy  [ {attribute, operator, value}, ... ]      one implicit AND group
//	groups  { "groups": [ {key, conditions[], rollout_pct, variant}, ... ] }
//
// A legacy config evaluates exactly as it always did (legacy_golden_test.go
// pins that); the groups format is only ever produced by new writes. A binary
// rolled back to before groups existed reads a groups config as invalid and
// quarantines it (fails closed to disabled) rather than evaluating it wrongly.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// MaxPayloadBytes bounds one variant payload (compacted JSON).
	MaxPayloadBytes = 4096
	// Bounds on the groups format. The legacy flat format is unbounded for
	// compatibility; new writes are not.
	maxGroups          = 32
	maxGroupConditions = 32
	maxGroupKeyLen     = 64
)

// ConditionGroup is one release condition: all Conditions must match (AND);
// then the user must fall inside RolloutPct using the group's own bucket.
type ConditionGroup struct {
	// Key is the group's stable identity and its bucketing salt. Omitted, it
	// defaults to "g<index>" (position). Set it explicitly if groups may be
	// reordered without reshuffling users.
	Key        string          `json:"key,omitempty"`
	Name       string          `json:"name,omitempty"`
	Conditions []TargetingRule `json:"conditions"`
	// RolloutPct is 0..100; omitted means 100. 0 admits nobody.
	RolloutPct *int `json:"rollout_pct,omitempty"`
	// Variant, when set, overrides the hash-based variant choice for users
	// admitted through this group. Multivariate flags only.
	Variant string `json:"variant,omitempty"`
}

type groupsEnvelope struct {
	Groups []ConditionGroup `json:"groups"`
}

// FlagDefinition is the evaluable form of a flag: the parsed, validated
// config. It is also the wire shape of GET /api/v1/flags/config.
type FlagDefinition struct {
	Key        string           `json:"key"`
	Type       string           `json:"type"`
	Enabled    bool             `json:"enabled"`
	RolloutPct int              `json:"rollout_pct"`
	Variants   []Variant        `json:"variants,omitempty"`
	Rules      []TargetingRule  `json:"rules,omitempty"`  // legacy flat AND list
	Groups     []ConditionGroup `json:"groups,omitempty"` // mutually exclusive with Rules
	// Invalid, when non-empty, marks a flag whose stored config failed
	// validation. It is quarantined: SDKs must answer disabled, never
	// evaluate it. Rules/Groups/Variants are omitted for such flags.
	Invalid string `json:"invalid,omitempty"`
}

// ConfigError is a read-boundary validation failure naming the stored part
// ("targeting" or "variants") so quarantine can attribute it.
type ConfigError struct {
	Part string
	Err  error
}

func (e *ConfigError) Error() string { return e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// ParseDefinition validates a stored flag and returns its evaluable form.
// Order matches the original evaluator: targeting first, then variants (only
// for multivariate flags that carry any).
func ParseDefinition(f FeatureFlag) (FlagDefinition, *ConfigError) {
	def := FlagDefinition{Key: f.FlagKey, Type: f.FlagType, Enabled: f.Enabled, RolloutPct: f.RolloutPct}
	rules, groups, err := parseTargetingConfig(f.Targeting)
	if err != nil {
		return def, &ConfigError{Part: "targeting", Err: err}
	}
	def.Rules, def.Groups = rules, groups
	if f.FlagType == "multivariate" && f.Variants != "" {
		def.Variants, err = ValidateVariants(f.Variants)
		if err != nil {
			return def, &ConfigError{Part: "variants", Err: err}
		}
	}
	if err := checkGroupVariants(groups, def.Variants, f.FlagType == "multivariate"); err != nil {
		return def, &ConfigError{Part: "targeting", Err: err}
	}
	return def, nil
}

// ValidateConfig is the write-boundary check: targeting (either format) and
// variants, plus the cross-check that a group's variant override names a
// declared variant of a multivariate flag.
func ValidateConfig(flagType, variants, targeting string) error {
	_, groups, err := parseTargetingConfig(targeting)
	if err != nil {
		return err
	}
	vs, err := ValidateVariants(variants)
	if err != nil {
		return err
	}
	return checkGroupVariants(groups, vs, flagType == "multivariate")
}

func checkGroupVariants(groups []ConditionGroup, variants []Variant, multivariate bool) error {
	for i, g := range groups {
		if g.Variant == "" {
			continue
		}
		found := false
		if multivariate {
			for _, v := range variants {
				if v.Key == g.Variant {
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf("targeting: group %d: variant %q is not a declared variant of this multivariate flag", i, g.Variant)
		}
	}
	return nil
}

// parseTargetingConfig dispatches on format. Anything that is not a JSON
// object carrying a "groups" key goes down the legacy path unchanged, so a
// malformed legacy config keeps its original error text.
func parseTargetingConfig(raw string) ([]TargetingRule, []ConditionGroup, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &probe) == nil {
			if _, ok := probe["groups"]; ok {
				groups, err := parseGroups(trimmed)
				return nil, groups, err
			}
		}
	}
	rules, err := ValidateTargeting(raw)
	return rules, nil, err
}

func parseGroups(raw string) ([]ConditionGroup, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var env groupsEnvelope
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("targeting: invalid groups JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("targeting: trailing data after groups object")
	}
	if len(env.Groups) == 0 {
		return nil, fmt.Errorf("targeting: groups must contain at least one group")
	}
	if len(env.Groups) > maxGroups {
		return nil, fmt.Errorf("targeting: %d groups exceeds the limit of %d", len(env.Groups), maxGroups)
	}
	seen := map[string]bool{}
	for i := range env.Groups {
		g := &env.Groups[i]
		if g.Key == "" {
			g.Key = "g" + strconv.Itoa(i)
		}
		if len(g.Key) > maxGroupKeyLen {
			return nil, fmt.Errorf("targeting: group %d: key longer than %d bytes", i, maxGroupKeyLen)
		}
		if seen[g.Key] {
			return nil, fmt.Errorf("targeting: duplicate group key %q", g.Key)
		}
		seen[g.Key] = true
		if g.RolloutPct != nil && (*g.RolloutPct < 0 || *g.RolloutPct > 100) {
			return nil, fmt.Errorf("targeting: group %d: rollout_pct %d out of range 0..100", i, *g.RolloutPct)
		}
		if len(g.Conditions) > maxGroupConditions {
			return nil, fmt.Errorf("targeting: group %d: %d conditions exceeds the limit of %d", i, len(g.Conditions), maxGroupConditions)
		}
		if err := validateRules(g.Conditions, fmt.Sprintf("group %d: condition", i)); err != nil {
			return nil, err
		}
	}
	return env.Groups, nil
}

// --- operators ---

var targetingOperators = map[string]bool{
	"eq": true, "neq": true, "in": true, "not_in": true, "contains": true,
	"gt": true, "gte": true, "lt": true, "lte": true,
	"starts_with": true, "ends_with": true,
	"is_set": true, "not_set": true,
}

// validateRules checks a rule list; what names the unit in errors, e.g.
// "rule" or "group 1: condition". Error text for the legacy "rule" unit is
// byte-for-byte what it always was.
func validateRules(rules []TargetingRule, what string) error {
	for i, r := range rules {
		if r.Attribute == "" {
			return fmt.Errorf("targeting: %s %d: attribute is required", what, i)
		}
		if !targetingOperators[r.Operator] {
			return fmt.Errorf("targeting: %s %d: unknown operator %q", what, i, r.Operator)
		}
		switch r.Operator {
		case "is_set", "not_set":
			// Value is ignored; none required.
			continue
		}
		if r.Value == nil {
			return fmt.Errorf("targeting: %s %d: value is required", what, i)
		}
		switch r.Operator {
		case "gt", "gte", "lt", "lte":
			if _, ok := numericValue(r.Value); !ok {
				return fmt.Errorf("targeting: %s %d: operator %q needs a finite numeric value", what, i, r.Operator)
			}
		case "starts_with", "ends_with":
			s, ok := r.Value.(string)
			if !ok || s == "" {
				return fmt.Errorf("targeting: %s %d: operator %q needs a non-empty string value", what, i, r.Operator)
			}
		}
	}
	return nil
}

// numericValue converts a rule value (JSON number or numeric string) to a
// finite float64.
func numericValue(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return x, true
	case string:
		return parseNumeric(x)
	}
	return 0, false
}

// parseNumeric parses a user-supplied attribute safely: bounded length,
// surrounding whitespace tolerated, and NaN/Inf (which ParseFloat accepts as
// words) rejected so a hostile attribute cannot make every comparison
// quietly false-or-true through IEEE semantics.
func parseNumeric(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// matchRule evaluates one condition. A missing attribute fails closed for
// every operator except not_set -- including neq and not_in, whose
// "absent != x" reading was deliberately NOT adopted: it would widen
// exposure of every existing flag using them (see docs/flags.md). Use
// not_set / is_set to target absence explicitly.
func matchRule(r TargetingRule, userCtx map[string]string) bool {
	actual, present := userCtx[r.Attribute]
	switch r.Operator {
	case "is_set":
		return present && actual != ""
	case "not_set":
		return !present || actual == ""
	}
	if !present {
		return false
	}
	switch r.Operator {
	case "eq":
		return actual == fmt.Sprintf("%v", r.Value)
	case "neq":
		return actual != fmt.Sprintf("%v", r.Value)
	case "in":
		for _, v := range toStringSlice(r.Value) {
			if actual == v {
				return true
			}
		}
		return false
	case "not_in":
		for _, v := range toStringSlice(r.Value) {
			if actual == v {
				return false
			}
		}
		return true
	case "contains":
		return strings.Contains(actual, fmt.Sprintf("%v", r.Value))
	case "starts_with":
		s, ok := r.Value.(string)
		return ok && s != "" && strings.HasPrefix(actual, s)
	case "ends_with":
		s, ok := r.Value.(string)
		return ok && s != "" && strings.HasSuffix(actual, s)
	case "gt", "gte", "lt", "lte":
		a, ok := parseNumeric(actual)
		if !ok {
			return false
		}
		b, ok := numericValue(r.Value)
		if !ok {
			return false
		}
		switch r.Operator {
		case "gt":
			return a > b
		case "gte":
			return a >= b
		case "lt":
			return a < b
		default:
			return a <= b
		}
	}
	// Fail closed: an unknown operator must not silently match (which
	// would expose a flag to everyone), so treat the rule as unmatched.
	return false
}

func matchesTargeting(rules []TargetingRule, userCtx map[string]string) bool {
	for _, r := range rules {
		if !matchRule(r, userCtx) {
			return false
		}
	}
	return true
}

// --- bucketing ---

// Bucket returns the user's bucket in 1..100 for a salt: the first two bytes
// of sha256(salt ":" userID) as a big-endian uint16, modulo 100, plus 1.
// Modulo bias: 65536 = 655*100 + 36, so buckets 1..36 each hold 656 of 65536
// inputs and 37..100 hold 655 (a 0.15% relative skew toward low buckets).
// The mapping is frozen: changing it reassigns every existing user.
func Bucket(salt, userID string) int {
	h := sha256.Sum256([]byte(salt + ":" + userID))
	val := int(h[0])<<8 | int(h[1])
	return val%100 + 1
}

func groupSalt(flagKey, groupKey string) string { return flagKey + ":grp:" + groupKey }

// BucketingSpec documents the algorithm for SDK authors; it is embedded in
// the GET /api/v1/flags/config response so the document and the server
// cannot drift apart.
var BucketingSpec = map[string]any{
	"hash":         "sha256",
	"bucket":       "uint16 big-endian from digest bytes 0 and 1, modulo 100, plus 1 (range 1..100)",
	"bucket_input": "<salt> + \":\" + <user_id> (UTF-8)",
	"flag_rollout": map[string]string{
		"salt":  "<flag key>",
		"admit": "rollout_pct >= 100 admits everyone without hashing; otherwise admit when bucket <= rollout_pct",
	},
	"group_rollout": map[string]string{
		"salt":  "<flag key> + \":grp:\" + <group key>  (group key defaults to \"g<index>\" when omitted)",
		"admit": "rollout_pct omitted or >= 100 admits everyone without hashing; otherwise admit when bucket <= rollout_pct (0 admits nobody)",
	},
	"variant": map[string]string{
		"salt":   "<flag key> + \":variant\"",
		"choose": "walk variants in order accumulating rollout_pct; pick the first whose cumulative sum >= bucket; if none, the first variant. A matched group's variant override replaces this choice.",
	},
	"evaluation_order": []string{
		"flag disabled -> disabled (detail \"flag disabled\")",
		"flag rollout miss -> disabled (detail \"rollout\")",
		"invalid flag -> disabled (reason \"invalid\")",
		"legacy rules: all must match else disabled (detail \"targeting\")",
		"groups: first group whose conditions all match AND whose rollout admits the user wins; none -> disabled (detail \"targeting\" if no group's conditions matched, \"group_rollout\" otherwise)",
		"multivariate: pick variant (group override, else hash); attach that variant's payload",
	},
	"conditions": "operators eq neq in not_in contains starts_with ends_with gt gte lt lte is_set not_set; a missing attribute fails closed for every operator except not_set; eq/neq/in/not_in/contains compare the attribute string with the %v formatting of the rule value; gt/gte/lt/lte parse both sides as finite float64 and are false when either side does not parse; is_set means present and non-empty",
}

// --- evaluation ---

// EvaluateDefinition is the reference evaluator. It is deterministic and
// side-effect free. A definition marked Invalid answers disabled with reason
// "invalid".
func EvaluateDefinition(def FlagDefinition, userID string, userCtx map[string]string) EvaluationResult {
	if def.Invalid != "" {
		return EvaluationResult{Enabled: false, Reason: ReasonInvalid, Detail: clampDetail(def.Invalid)}
	}
	if !def.Enabled {
		return EvaluationResult{Enabled: false, Reason: ReasonEvaluated, Detail: "flag disabled"}
	}
	if def.RolloutPct < 100 && Bucket(def.Key, userID) > def.RolloutPct {
		return EvaluationResult{Enabled: false, Reason: ReasonEvaluated, Detail: "rollout"}
	}

	var group *ConditionGroup
	groupKey := ""
	if len(def.Groups) > 0 {
		conditionMatched := false
		for i := range def.Groups {
			g := &def.Groups[i]
			if !matchesTargeting(g.Conditions, userCtx) {
				continue
			}
			conditionMatched = true
			key := g.Key
			if key == "" {
				key = "g" + strconv.Itoa(i)
			}
			if g.RolloutPct != nil && *g.RolloutPct < 100 {
				if Bucket(groupSalt(def.Key, key), userID) > *g.RolloutPct {
					continue
				}
			}
			group, groupKey = g, key
			break
		}
		if group == nil {
			detail := "targeting"
			if conditionMatched {
				detail = "group_rollout"
			}
			return EvaluationResult{Enabled: false, Reason: ReasonEvaluated, Detail: detail}
		}
	} else if len(def.Rules) > 0 && !matchesTargeting(def.Rules, userCtx) {
		return EvaluationResult{Enabled: false, Reason: ReasonEvaluated, Detail: "targeting"}
	}

	res := EvaluationResult{Enabled: true, Reason: ReasonEvaluated}
	res.Group = groupKey
	if def.Type == "multivariate" && len(def.Variants) > 0 {
		chosen := -1
		if group != nil && group.Variant != "" {
			for i, v := range def.Variants {
				if v.Key == group.Variant {
					chosen = i
					break
				}
			}
		}
		if chosen < 0 {
			hash := Bucket(def.Key+":variant", userID)
			cumulative := 0
			for i, v := range def.Variants {
				cumulative += v.RolloutPct
				if hash <= cumulative {
					chosen = i
					break
				}
			}
		}
		if chosen < 0 {
			chosen = 0
		}
		res.Variant = def.Variants[chosen].Key
		if p := def.Variants[chosen].Payload; len(p) > 0 && !bytes.Equal(p, []byte("null")) {
			res.Payload = p
		}
	}
	return res
}
