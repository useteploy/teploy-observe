package flags

// O08 correctness slice: distinguish a valid false evaluation from
// unavailable configuration, and refuse to evaluate flags whose stored
// targeting/variants JSON is invalid instead of silently skipping the
// condition. Two source defects these tests pin:
//
//  1. A flags DATABASE READ FAILURE evaluated as {enabled:false} with no
//     error and no log — availability and decision correctness conflated.
//  2. MALFORMED TARGETING JSON skipped the targeting condition entirely —
//     invalid stored rules made a flag evaluable by everyone.
//
// The three conditions and their response semantics:
//
//	reason "evaluated"   a real decision from valid config (false is a
//	                     valid answer, not an error)
//	reason "unavailable" the config read failed — error class in detail,
//	                     fail-safe default documented below
//	reason "invalid"     stored config failed validation — quarantined
//	                     from evaluation, counter incremented
//
// Fail-safe default (documented, per-flag contract): the server answers
// Enabled=false for BOTH "unavailable" and "invalid". Flags gate feature
// exposure and the create-path default is disabled, so an outage or a
// corrupt config must not widen exposure. A per-flag fail-OPEN override
// (the kill-switch pattern, whose safe state is on) needs a stored
// per-flag default plus an SDK contract — recorded as an O08 residual.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// --- storage-free: the read-failure condition (defect 1) ---

func TestO08_ReadFailureIsUnavailableNotDisabled(t *testing.T) {
	svc := NewFlagService(nil)
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) {
		return nil, errors.New("connection refused (storage gone)")
	}
	res, err := svc.Evaluate(context.Background(), "s", "f", "u", nil)
	if err != nil {
		t.Fatalf("Evaluate returned an error for an unavailable store: %v", err)
	}
	if res.Reason != ReasonUnavailable {
		t.Fatalf("reason = %q, want %q — a read failure must not read as a decision", res.Reason, ReasonUnavailable)
	}
	if res.Enabled {
		t.Fatalf("unavailable must evaluate to the documented fail-safe default (false)")
	}
	if res.Detail == "" || !strings.Contains(res.Detail, "storage") {
		t.Fatalf("detail = %q, want the error class", res.Detail)
	}
	if got := svc.Stats()["config_unavailable_total"]; got != 1 {
		t.Fatalf("config_unavailable_total = %d, want 1", got)
	}
}

func TestO08_ReadFailureErrorClasses(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "deadline"},
		{context.Canceled, "canceled"},
		{errors.New("dial tcp: connection refused"), "storage"},
	}
	for _, c := range cases {
		svc := NewFlagService(nil)
		e := c.err
		svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return nil, e }
		res, err := svc.Evaluate(context.Background(), "s", "f", "u", nil)
		if err != nil {
			t.Fatalf("%v: Evaluate errored: %v", c.err, err)
		}
		if res.Reason != ReasonUnavailable || !strings.Contains(res.Detail, c.want) {
			t.Fatalf("%v: reason/detail = %q/%q, want unavailable naming %q", c.err, res.Reason, res.Detail, c.want)
		}
	}
}

// --- storage-free: a valid false is a decision, not an error ---

func TestO08_ValidFalseAndTrueBothRepresentable(t *testing.T) {
	flag := func(mut func(*FeatureFlag)) *FlagService {
		svc := NewFlagService(nil)
		f := FeatureFlag{FlagID: "id", SiteID: "s", FlagKey: "f", FlagType: "boolean", Enabled: true, RolloutPct: 100}
		if mut != nil {
			mut(&f)
		}
		ff := f
		svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return []FeatureFlag{ff}, nil }
		return svc
	}
	ctx := context.Background()

	cases := []struct {
		name    string
		svc     *FlagService
		userCtx map[string]string
		want    bool
	}{
		{"absent flag is a valid false", absentFlagSvc(), nil, false},
		{"disabled flag is a valid false", flag(func(f *FeatureFlag) { f.Enabled = false }), nil, false},
		{"rollout zero is a valid false", flag(func(f *FeatureFlag) { f.RolloutPct = 0 }), nil, false},
		{"targeting miss is a valid false", flag(func(f *FeatureFlag) {
			f.Targeting = `[{"attribute":"plan","operator":"eq","value":"pro"}]`
		}), map[string]string{"plan": "free"}, false},
		{"targeting hit is a valid true", flag(func(f *FeatureFlag) {
			f.Targeting = `[{"attribute":"plan","operator":"eq","value":"pro"}]`
		}), map[string]string{"plan": "pro"}, true},
		{"enabled no restrictions is a valid true", flag(nil), nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := c.svc.Evaluate(ctx, "s", "f", "u", c.userCtx)
			if err != nil {
				t.Fatalf("Evaluate errored: %v", err)
			}
			if res.Reason != ReasonEvaluated {
				t.Fatalf("reason = %q, want %q — this is a real decision from valid config", res.Reason, ReasonEvaluated)
			}
			if res.Enabled != c.want {
				t.Fatalf("enabled = %v, want %v", res.Enabled, c.want)
			}
		})
	}
}

func absentFlagSvc() *FlagService {
	svc := NewFlagService(nil)
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return nil, nil }
	return svc
}

// --- storage-free: invalid stored config is quarantined (defect 2) ---

func TestO08_InvalidStoredTargetingNeverEvaluatesUnrestricted(t *testing.T) {
	svc := NewFlagService(nil)
	f := FeatureFlag{FlagID: "id", SiteID: "s", FlagKey: "f", FlagType: "boolean", Enabled: true, RolloutPct: 100,
		Targeting: `{"broken": [`}
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return []FeatureFlag{f}, nil }

	res, err := svc.Evaluate(context.Background(), "s", "f", "u", nil)
	if err != nil {
		t.Fatalf("Evaluate errored: %v", err)
	}
	if res.Reason != ReasonInvalid {
		t.Fatalf("reason = %q, want %q — invalid stored rules must surface as invalid", res.Reason, ReasonInvalid)
	}
	if res.Enabled {
		t.Fatalf("invalid targeting evaluated ENABLED — corrupt rules made the flag unrestricted (defect 2)")
	}
	if res.Detail == "" || !strings.Contains(res.Detail, "targeting") {
		t.Fatalf("detail = %q, want it to name the quarantined part", res.Detail)
	}
	st := svc.Stats()
	if st["invalid_config_total"] != 1 {
		t.Fatalf("invalid_config_total = %d, want 1", st["invalid_config_total"])
	}
	if st["invalid_config_distinct"] != 1 {
		t.Fatalf("invalid_config_distinct = %d, want 1", st["invalid_config_distinct"])
	}
}

func TestO08_InvalidStoredVariantsQuarantined(t *testing.T) {
	svc := NewFlagService(nil)
	f := FeatureFlag{FlagID: "id", SiteID: "s", FlagKey: "f", FlagType: "multivariate", Enabled: true, RolloutPct: 100,
		Variants: `not json at all`}
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return []FeatureFlag{f}, nil }

	res, err := svc.Evaluate(context.Background(), "s", "f", "u", nil)
	if err != nil {
		t.Fatalf("Evaluate errored: %v", err)
	}
	if res.Reason != ReasonInvalid || res.Enabled {
		t.Fatalf("reason/enabled = %q/%v, want invalid/false", res.Reason, res.Enabled)
	}
}

func TestO08_DisabledFlagWithInvalidConfigStillSurfacesInvalid(t *testing.T) {
	// Validation runs before the enabled short-circuit so operators learn
	// their config is corrupt without having to enable the flag first.
	svc := NewFlagService(nil)
	f := FeatureFlag{FlagID: "id", SiteID: "s", FlagKey: "f", FlagType: "boolean", Enabled: false, RolloutPct: 100,
		Targeting: `[{"attribute":"plan","operator":"eq"}]`} // missing value
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return []FeatureFlag{f}, nil }
	res, _ := svc.Evaluate(context.Background(), "s", "f", "u", nil)
	if res.Reason != ReasonInvalid {
		t.Fatalf("reason = %q, want invalid — a disabled flag must still surface corrupt config", res.Reason)
	}
}

// --- storage-free: write-boundary validation ---

func TestO08_ValidateTargetingRules(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"empty is unrestricted", "", ""},
		{"null is unrestricted", "null", ""},
		{"empty array is unrestricted", "[]", ""},
		{"valid rule", `[{"attribute":"plan","operator":"eq","value":"pro"}]`, ""},
		{"valid in rule", `[{"attribute":"c","operator":"in","value":["US","CA"]}]`, ""},
		{"malformed json", `{"broken": [`, "invalid JSON"},
		{"not an array", `{"attribute":"plan"}`, "invalid JSON"},
		{"unknown operator", `[{"attribute":"plan","operator":"regex","value":".*"}]`, "operator"},
		{"empty attribute", `[{"attribute":"","operator":"eq","value":"x"}]`, "attribute"},
		{"missing value", `[{"attribute":"plan","operator":"eq"}]`, "value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateTargeting(c.raw)
			if c.wantErr == "" && err != nil {
				t.Fatalf("ValidateTargeting(%q) = %v, want valid", c.raw, err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("ValidateTargeting(%q) = %v, want error naming %q", c.raw, err, c.wantErr)
			}
		})
	}
}

func TestO08_ValidateVariantsRules(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"empty allowed", "", ""},
		{"valid variants", `[{"key":"a","name":"A","rollout_pct":50},{"key":"b","name":"B","rollout_pct":50}]`, ""},
		{"malformed json", `[{"key":`, "invalid JSON"},
		{"empty key", `[{"key":"","rollout_pct":100}]`, "key"},
		{"pct over 100", `[{"key":"a","rollout_pct":101}]`, "rollout_pct"},
		{"negative pct", `[{"key":"a","rollout_pct":-1}]`, "rollout_pct"},
		{"duplicate keys", `[{"key":"a","rollout_pct":50},{"key":"a","rollout_pct":50}]`, "duplicate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateVariants(c.raw)
			if c.wantErr == "" && err != nil {
				t.Fatalf("ValidateVariants(%q) = %v, want valid", c.raw, err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("ValidateVariants(%q) = %v, want error naming %q", c.raw, err, c.wantErr)
			}
		})
	}
}

func TestO08_CreateRejectsMalformedJSONWithoutStoring(t *testing.T) {
	dsn := nucleustest.DSN(t)
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "feature_flags", flagColumns,
		"(tenant_id, site_id, flag_id)", "version")

	svc := NewFlagService(db)
	ctx := context.Background()

	// Valid JSON, wrong shape: the JSONB column accepts it, only the write
	// boundary can reject it (a syntactically broken payload the store
	// itself refuses with a 500 — the hole is shape and semantics).
	if _, err := svc.Create(ctx, "o08-site", "bad-targeting", "Bad", "", "boolean", "", `{"attribute":"plan","operator":"eq","value":"pro"}`, 100); err == nil {
		t.Fatalf("Create stored a non-array targeting ruleset — the write boundary must reject it naming the error")
	} else if !strings.Contains(err.Error(), "targeting") {
		t.Fatalf("Create error = %v, want it to name targeting", err)
	}

	if _, err := svc.Create(ctx, "o08-site", "bad-operator", "Bad", "", "boolean", "", `[{"attribute":"plan","operator":"regex","value":".*"}]`, 100); err == nil {
		t.Fatalf("Create stored an unknown operator — matchesTargeting fails closed per rule but the boundary should refuse it")
	} else if !strings.Contains(err.Error(), "operator") {
		t.Fatalf("Create error = %v, want it to name the operator", err)
	}

	if _, err := svc.Create(ctx, "o08-site", "bad-variants", "Bad", "", "multivariate", `{"key":"a"}`, "", 100); err == nil {
		t.Fatalf("Create stored malformed variants — the write boundary must reject it naming the error")
	} else if !strings.Contains(err.Error(), "variants") {
		t.Fatalf("Create error = %v, want it to name variants", err)
	}

	// The rejected flag must not exist.
	rows, err := nucleus.Query[FeatureFlag](ctx, db.SQL(),
		`SELECT flag_id FROM feature_flags WHERE site_id = $1 AND flag_key = $2`, "o08-site", "bad-targeting")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rejected Create left %d rows behind", len(rows))
	}

	// Valid rules still store.
	if _, err := svc.Create(ctx, "o08-site", "good", "Good", "", "boolean", "",
		`[{"attribute":"plan","operator":"eq","value":"pro"}]`, 100); err != nil {
		t.Fatalf("Create with valid targeting failed: %v", err)
	}
}

// --- Nucleus-gated: the stored-corruption path end to end ---

// TestO08_LegacyCorruptRowIsInvalidNotUnrestricted writes a corrupt
// targeting column directly (the write boundary rejects new writes, but
// rows written before it — or corruption behind it — must not become
// unrestricted).
func TestO08_LegacyCorruptRowIsInvalidNotUnrestricted(t *testing.T) {
	dsn := nucleustest.DSN(t)
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "feature_flags", flagColumns,
		"(tenant_id, site_id, flag_id)", "version")

	svc := NewFlagService(db)
	ctx := context.Background()
	const site = "o08-corrupt-site"
	const key = "checkout"

	flag, err := svc.Create(ctx, site, key, "Checkout", "", "boolean", "", "", 100)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Toggle(ctx, flag.FlagID, true); err != nil {
		t.Fatalf("toggle: %v", err)
	}

	// Corrupt the latest version's targeting behind the write boundary with
	// VALID JSON of the wrong shape — exactly what a buggy writer produces
	// and what the JSONB column happily stores (an unarrayed rule object).
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO feature_flags (flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, targeting, created_at, version)
		 SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, $2, created_at, version + 1
		 FROM `+flagsLatest("flag_id = $1"), flag.FlagID, `{"attribute":"plan","operator":"eq","value":"pro"}`); err != nil {
		t.Fatalf("corrupt rewrite: %v", err)
	}

	res, err := svc.Evaluate(ctx, site, key, "user-1", nil)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Reason != ReasonInvalid {
		t.Fatalf("reason = %q, want %q — corrupt stored rules must not evaluate as a decision", res.Reason, ReasonInvalid)
	}
	if res.Enabled {
		t.Fatalf("corrupt targeting evaluated ENABLED for an unrestricted user (defect 2)")
	}
	if svc.Stats()["invalid_config_total"] < 1 {
		t.Fatalf("quarantine counter did not increment")
	}

	// Repair the row: evaluation resumes (quarantine is per-read, not sticky).
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO feature_flags (flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, targeting, created_at, version)
		 SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, $2, created_at, version + 1
		 FROM `+flagsLatest("flag_id = $1"), flag.FlagID,
		`[{"attribute":"plan","operator":"eq","value":"pro"}]`); err != nil {
		t.Fatalf("repair rewrite: %v", err)
	}
	res, err = svc.Evaluate(ctx, site, key, "user-1", map[string]string{"plan": "free"})
	if err != nil {
		t.Fatalf("evaluate after repair: %v", err)
	}
	if res.Reason != ReasonEvaluated || res.Enabled {
		t.Fatalf("after repair reason/enabled = %q/%v, want evaluated/false for a non-matching user", res.Reason, res.Enabled)
	}
	res, err = svc.Evaluate(ctx, site, key, "user-2", map[string]string{"plan": "pro"})
	if err != nil {
		t.Fatalf("evaluate after repair: %v", err)
	}
	if res.Reason != ReasonEvaluated || !res.Enabled {
		t.Fatalf("after repair reason/enabled = %q/%v, want evaluated/true for a matching user", res.Reason, res.Enabled)
	}
}

// TestO08_EvaluateDecisionsOnLiveStore covers the real query path: absent
// flag, disabled, rollout, targeting hit/miss — every false here is a
// VALID decision carrying reason "evaluated".
func TestO08_EvaluateDecisionsOnLiveStore(t *testing.T) {
	dsn := nucleustest.DSN(t)
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "feature_flags", flagColumns,
		"(tenant_id, site_id, flag_id)", "version")

	svc := NewFlagService(db)
	ctx := context.Background()
	const site = "o08-decisions-site"

	// Absent flag: a valid false.
	res, err := svc.Evaluate(ctx, site, "missing", "u", nil)
	if err != nil {
		t.Fatalf("evaluate absent: %v", err)
	}
	if res.Reason != ReasonEvaluated || res.Enabled {
		t.Fatalf("absent flag reason/enabled = %q/%v, want evaluated/false", res.Reason, res.Enabled)
	}

	disabled, _ := svc.Create(ctx, site, "disabled", "D", "", "boolean", "", "", 100)
	res, _ = svc.Evaluate(ctx, site, "disabled", "u", nil)
	if res.Reason != ReasonEvaluated || res.Enabled {
		t.Fatalf("disabled flag reason/enabled = %q/%v, want evaluated/false", res.Reason, res.Enabled)
	}
	_ = disabled

	half, _ := svc.Create(ctx, site, "half-rollout", "H", "", "boolean", "", "", 50)
	if err := svc.Toggle(ctx, half.FlagID, true); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	// Choose users deterministically via the same bucket function: one
	// inside the 50% rollout, one outside it (Create normalizes pct<=0 to
	// 100, so a partial rollout is the representable miss case).
	var uIn, uOut string
	for i := 0; ; i++ {
		id := "u" + strconv.Itoa(i)
		if hashUser("half-rollout", id) <= 50 {
			uIn = id
		} else {
			uOut = id
		}
		if uIn != "" && uOut != "" {
			break
		}
	}
	res, _ = svc.Evaluate(ctx, site, "half-rollout", uIn, nil)
	if res.Reason != ReasonEvaluated || !res.Enabled {
		t.Fatalf("rollout hit reason/enabled = %q/%v, want evaluated/true", res.Reason, res.Enabled)
	}
	res, _ = svc.Evaluate(ctx, site, "half-rollout", uOut, nil)
	if res.Reason != ReasonEvaluated || res.Enabled {
		t.Fatalf("rollout miss reason/enabled = %q/%v, want evaluated/false — a VALID false, not an error", res.Reason, res.Enabled)
	}

	targeted, _ := svc.Create(ctx, site, "targeted", "T", "", "boolean", "",
		`[{"attribute":"plan","operator":"eq","value":"pro"}]`, 100)
	if err := svc.Toggle(ctx, targeted.FlagID, true); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	res, _ = svc.Evaluate(ctx, site, "targeted", "u", map[string]string{"plan": "pro"})
	if res.Reason != ReasonEvaluated || !res.Enabled {
		t.Fatalf("targeting hit reason/enabled = %q/%v, want evaluated/true", res.Reason, res.Enabled)
	}
	res, _ = svc.Evaluate(ctx, site, "targeted", "u", map[string]string{"plan": "free"})
	if res.Reason != ReasonEvaluated || res.Enabled {
		t.Fatalf("targeting miss reason/enabled = %q/%v, want evaluated/false — a VALID false, not an error", res.Reason, res.Enabled)
	}
}

// --- consumer shapes stay additive ---

func TestO08_ResponseShapeIsAdditiveForLegacyConsumers(t *testing.T) {
	// The UI/SDK consumer reads {enabled, variant?}; every new field is
	// additive and the legacy decode must keep working for all conditions.
	type legacy struct {
		Enabled bool   `json:"enabled"`
		Variant string `json:"variant"`
	}
	newSvc := func(res *EvaluationResult) []byte {
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	for name, body := range map[string][]byte{
		"decision": newSvc(&EvaluationResult{Enabled: true, Variant: "a", Reason: ReasonEvaluated}),
		"false":    newSvc(&EvaluationResult{Enabled: false, Reason: ReasonEvaluated, Detail: "flag disabled"}),
		"unavail":  newSvc(&EvaluationResult{Enabled: false, Reason: ReasonUnavailable, Detail: "flags config read failed: storage"}),
		"invalid":  newSvc(&EvaluationResult{Enabled: false, Reason: ReasonInvalid, Detail: "stored targeting failed validation"}),
	} {
		var l legacy
		if err := json.Unmarshal(body, &l); err != nil {
			t.Fatalf("%s: legacy decode failed: %v", name, err)
		}
		var full EvaluationResult
		if err := json.Unmarshal(body, &full); err != nil {
			t.Fatalf("%s: full decode failed: %v", name, err)
		}
		if full.Reason == "" {
			t.Fatalf("%s: reason missing from response — every answer must state its condition", name)
		}
	}
}
