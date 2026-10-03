# Feature flags: targeting, payloads and local evaluation

Two ways to get a decision:

- `POST /api/v1/flags/evaluate` (public, rate limited): the server decides, one
  flag per call. Targeting rules are never disclosed.
- `GET /api/v1/flags/config` (API key): the server hands over every flag's
  definition and the SDK decides locally. See "Local evaluation".

## Stored config

Flag config lives in the existing `feature_flags` row (`rollout_pct`,
`variants`, `targeting`). No schema change. Config is validated on write
(400 naming the error) and again on read (a stored config that fails
validation is quarantined: it answers `enabled:false, reason:"invalid"`).

### Targeting, legacy format (unchanged)

`targeting` is a JSON array of conditions, ANDed:

```json
[{"attribute":"plan","operator":"eq","value":"pro"}]
```

Existing flags evaluate exactly as before; `internal/flags/testdata/legacy_golden.json`
pins 3,060 decisions from the pre-groups evaluator.

### Targeting, condition groups

`targeting` may instead be an object with an ordered `groups` list:

```json
{"groups":[
  {"key":"staff","conditions":[{"attribute":"email","operator":"ends_with","value":"@corp.example"}],"variant":"treatment"},
  {"key":"beta-pro","conditions":[{"attribute":"plan","operator":"eq","value":"pro"}],"rollout_pct":25},
  {"key":"everyone","rollout_pct":5}
]}
```

- A group admits a user when all its `conditions` match (empty = everyone) and
  the user falls inside the group's `rollout_pct` (omitted = 100, 0 = nobody).
- Groups are tried in order; the first admitting group wins. A group whose
  conditions match but whose rollout excludes the user does not stop the
  search; later groups are still tried.
- `variant` overrides the hash-chosen variant for users admitted by that group
  (multivariate flags; must name a declared variant).
- `key` is the group's identity and bucketing salt (default `g<index>`). Set
  explicit keys if you will reorder groups and want users to stay put.
- Limits: 32 groups, 32 conditions per group, group key 64 bytes.
- The flag-level `rollout_pct` still applies first, as a global cap.

Rolling back to a build without groups support makes a groups config read as
invalid (quarantined, disabled), never as unrestricted.

### Operators

`eq neq in not_in contains starts_with ends_with gt gte lt lte is_set not_set`

- `gt/gte/lt/lte`: both sides are parsed as finite float64 (whitespace trimmed,
  at most 64 bytes, `NaN`/`Inf`/overflow rejected). Unparseable on either side
  is no match. The rule value must be a number or numeric string (400 otherwise).
- `starts_with/ends_with`: case-sensitive, non-empty string value required.
- `is_set`: attribute present and non-empty. `not_set`: absent or empty. No value.
- There is deliberately no regex operator (ReDoS); no `semver` operator yet.

**Missing attributes fail closed for every operator except `not_set`,
including `neq` and `not_in`.** Reading "absent is not equal to x" as a match
would silently widen exposure of every existing flag that uses those operators,
which is a reassignment of users. Target absence explicitly:
`[{"attribute":"plan","operator":"not_set"}]`.

### Variants and payloads

`variants` entries may carry an optional JSON `payload` (any JSON value,
at most 4096 bytes compacted). `evaluate` returns the chosen variant's payload
as `payload`; a group variant override returns the overridden variant's payload.
Payloads reach browsers: no secrets.

### Bucketing

`bucket = (uint16_be(sha256(salt + ":" + user_id)[0:2]) % 100) + 1`, admitted
when `bucket <= pct`. Salts: flag rollout `<flag>`, variant choice
`<flag>:variant`, group rollout `<flag>:grp:<group key>`. Distinct salts make
the three decisions and every group statistically independent.

Known modulo bias: 65536 is not divisible by 100, so buckets 1..36 are 0.15%
more likely than 37..100. This is not corrected because any change to the
mapping reassigns existing users; it is immaterial at rollout granularity and
is frozen as part of the contract.

## Local evaluation

`GET /api/v1/flags/config` returns the config for one site:

```json
{"site_id":"...","generated_at":1700000000000,
 "bucketing":{ ...the algorithm above, machine readable... },
 "flags":[{"key":"checkout","type":"multivariate","enabled":true,"rollout_pct":100,
           "variants":[...],"groups":[...]}]}
```

Auth: `X-API-Key` with the telemetry capability (the same site key the browser
SDK uses). The site is taken from the key; a `site_id` query parameter must
match it or the request is refused with 403. No key: 401. Unlike `evaluate`
this endpoint exposes targeting rules, so it is not public. Because the
telemetry key is embedded in browsers by design, anyone who can load your site
can read this config; do not put secrets in targeting values or payloads. It is
also served on the ingest listener.

The response carries an `ETag` of the flag content; send `If-None-Match` to
get `304`. Flags whose stored config is invalid appear with an `invalid`
message and no rules: answer disabled, as the server does. Unlike `evaluate`,
local evaluation records no `flag_evaluations` row.

The reference evaluator is `flags.EvaluateDefinition` (pure Go, no I/O).
`internal/flags/testdata/conformance.json` holds bucket vectors and evaluation
cases (flag definition, user, context, expected result) for SDKs to run against
their own implementation.

## Not implemented

- Per-flag fail-open on unavailable/invalid config (O08 residual). The default
  has to be readable without the config it guards, which needs separate
  storage and an SDK contract. Local-evaluation SDKs can keep the last good
  config instead.
