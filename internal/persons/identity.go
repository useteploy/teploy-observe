package persons

// C3 - person properties, explicit alias/merge and erasure (migration 061).
//
// Everything here follows docs/IDENTITY_MODEL_ADR.md: a person is still an
// aggregate over events.distinct_id; the tables added by 061 are side data
// keyed by that same person_key. Historical events are never rewritten; an
// alias is resolved forward at read time, in Go, over bounded sets.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Bounds. Every limit is enforced before any write.
const (
	MaxPropertyKeys     = 50
	MaxPropertyValueLen = 1024 // bytes of the JSON-encoded value
	MaxPropertyKeyLen   = 64
	MaxPersonKeyLen     = 256
	// MaxAliasesPerPerson bounds one merged person's alias fan-out so the
	// detail view's per-member reads stay bounded.
	MaxAliasesPerPerson = 20
	// MaxAliasesPerSite bounds the alias map loaded for every resolved read.
	MaxAliasesPerSite = 10000
	// MaxTombstonesPerSite bounds the exclusion set loaded for list reads.
	MaxTombstonesPerSite = 50000
	// resolveScanCap bounds the Go-side fold of a resolved list read.
	resolveScanCap = 5000
)

// Sentinel errors; handlers map them to HTTP statuses.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrLimit    = errors.New("limit exceeded")
	ErrErased   = errors.New("person erased")
)

var (
	propKeyRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_.$-]{0,63}$`)
)

// reservedPropertyKeys are rejected so the raw identify value (which the
// server hashes before storage) cannot be echoed back into a property and
// defeat the hashing. This is identifier hygiene, not a PII filter: any
// other key (including email) is the customer's choice.
var reservedPropertyKeys = map[string]bool{"distinct_id": true, "user_id": true}

// ValidatePersonKey accepts hashed keys and raw opt-in identities without rewriting them.
// SQL binds these values; only the storage encoding and byte bound are constrained.
func ValidatePersonKey(k string) error {
	if k == "" || len(k) > MaxPersonKeyLen || !utf8.ValidString(k) || strings.ContainsRune(k, 0) {
		return fmt.Errorf("%w: person key must be valid UTF-8, without NUL, and 1-%d bytes", ErrInvalid, MaxPersonKeyLen)
	}
	return nil
}

// ValidateProperties validates one identify() trait batch. It returns the
// scalar values to set and the keys to remove (null values). Values must be
// strings, numbers or booleans; nested objects and arrays are rejected so a
// property stays bounded and comparable.
func ValidateProperties(in map[string]any) (set map[string]any, remove []string, err error) {
	if len(in) > MaxPropertyKeys {
		return nil, nil, fmt.Errorf("%w: at most %d properties per request", ErrInvalid, MaxPropertyKeys)
	}
	set = make(map[string]any, len(in))
	for k, v := range in {
		if !propKeyRe.MatchString(k) {
			return nil, nil, fmt.Errorf("%w: property key %q must match %s", ErrInvalid, k, propKeyRe.String())
		}
		if reservedPropertyKeys[strings.ToLower(k)] {
			return nil, nil, fmt.Errorf("%w: property key %q is reserved", ErrInvalid, k)
		}
		switch v.(type) {
		case nil:
			remove = append(remove, k)
			continue
		case string, bool, float64, float32, int, int32, int64, json.Number:
		default:
			return nil, nil, fmt.Errorf("%w: property %q must be a string, number, boolean or null", ErrInvalid, k)
		}
		enc, merr := json.Marshal(v)
		if merr != nil || len(enc) > MaxPropertyValueLen {
			return nil, nil, fmt.Errorf("%w: property %q value exceeds %d bytes", ErrInvalid, k, MaxPropertyValueLen)
		}
		set[k] = v
	}
	sort.Strings(remove)
	return set, remove, nil
}

// AliasRow is one active alias edge.
type AliasRow struct {
	AliasKey     string `json:"alias_key"     db:"alias_key"`
	CanonicalKey string `json:"canonical_key" db:"canonical_key"`
}

// Resolver maps alias keys to their canonical person. Built once per read
// from a bounded alias set; chains are followed across the complete set with cycle
// detection (a corrupt cycle resolves to the key itself - fail safe, no
// merge).
type Resolver struct {
	direct  map[string]string
	members map[string][]string
	roots   map[string]string
}

// NewResolver builds a resolver from active alias edges.
func NewResolver(rows []AliasRow) *Resolver {
	r := &Resolver{direct: make(map[string]string, len(rows))}
	for _, a := range rows {
		if a.AliasKey != "" && a.CanonicalKey != "" && a.AliasKey != a.CanonicalKey {
			r.direct[a.AliasKey] = a.CanonicalKey
		}
	}
	// Memoize complete paths once. Empty roots mark corrupt cycles and paths
	// entering them, so even a 10,000-edge legacy chain resolves in linear work.
	r.roots = make(map[string]string, len(r.direct))
	for alias := range r.direct {
		if _, known := r.roots[alias]; known {
			continue
		}
		var path []string
		seen := map[string]bool{}
		cur, root := alias, ""
		for {
			if known, ok := r.roots[cur]; ok {
				root = known
				break
			}
			if seen[cur] {
				break
			}
			next, ok := r.direct[cur]
			if !ok {
				root = cur
				break
			}
			seen[cur] = true
			path = append(path, cur)
			cur = next
		}
		for _, key := range path {
			r.roots[key] = root
		}
	}
	r.members = make(map[string][]string)
	for alias := range r.direct {
		root := r.Canonical(alias)
		if root != alias {
			r.members[root] = append(r.members[root], alias)
		}
	}
	for _, m := range r.members {
		sort.Strings(m)
	}
	return r
}

// Empty reports whether no aliases exist.
func (r *Resolver) Empty() bool { return len(r.direct) == 0 }

// IsAlias reports whether key is itself merged into another person.
func (r *Resolver) IsAlias(key string) bool { _, ok := r.direct[key]; return ok }

// Canonical returns the root person key for key.
func (r *Resolver) Canonical(key string) string {
	if root, ok := r.roots[key]; ok && root != "" {
		return root
	}
	return key // service rejects corrupt cycles before exposing resolved reads
}

// Members returns every alias whose root is canonical (sorted, excluding
// canonical itself).
func (r *Resolver) Members(canonical string) []string { return r.members[canonical] }

// absorb folds o into p: exact event sum, session upper bound, widest
// seen-range, and the most recent row's country/browser.
func (p *Person) absorb(o Person) {
	p.EventCount += o.EventCount
	p.SessionCount += o.SessionCount
	if o.FirstSeenMs < p.FirstSeenMs {
		p.FirstSeenMs = o.FirstSeenMs
	}
	if o.LastSeenMs > p.LastSeenMs {
		p.LastSeenMs = o.LastSeenMs
		p.TopCountry, p.TopBrowser = o.TopCountry, o.TopBrowser
	}
}

// foldPersons collapses per-distinct_id rows onto their canonical person,
// dropping erased keys. Sums are exact for events; session_count is an
// UPPER BOUND (a monthly session estimate shared by two merged ids is
// counted twice).
func foldPersons(rows []Person, res *Resolver, tomb map[string]bool) []Person {
	acc := make(map[string]*Person, len(rows))
	order := make([]string, 0, len(rows))
	for _, p := range rows {
		if tomb[p.DistinctID] {
			continue
		}
		c := p.DistinctID
		if p.DistinctID != "" {
			c = res.Canonical(p.DistinctID)
		}
		if tomb[c] {
			continue
		}
		cur, ok := acc[c]
		if !ok {
			cp := p
			cp.DistinctID = c
			acc[c] = &cp
			order = append(order, c)
			continue
		}
		cur.absorb(p)
	}
	out := make([]Person, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastSeenMs != out[j].LastSeenMs {
			return out[i].LastSeenMs > out[j].LastSeenMs
		}
		return out[i].DistinctID < out[j].DistinctID
	})
	return out
}

// EventSource is the read side over the events table.
type EventSource interface {
	// List/Count are the SQL-paginated fast path (no aliases/tombstones).
	List(ctx context.Context, siteID string, fromMs, toMs int64, limit, offset int, includeAnonymous bool) ([]Person, error)
	Count(ctx context.Context, siteID string, fromMs, toMs int64, includeAnonymous bool) (int64, error)
	// ListAll returns up to limit per-distinct_id rows unpaginated.
	ListAll(ctx context.Context, siteID string, fromMs, toMs int64, limit int, includeAnonymous bool) ([]Person, error)
	// Aggregate returns the lifetime summary of one distinct_id.
	Aggregate(ctx context.Context, siteID, key string) (Person, bool, error)
	Timeline(ctx context.Context, siteID, key string) ([]PersonEvent, error)
	Exists(ctx context.Context, siteID, key string) (bool, error)
}

// IdentityStore is the side-data store for the 061 tables. Every method is
// scoped by siteID.
type IdentityStore interface {
	GetProps(ctx context.Context, siteID, key string) (props string, found bool, err error)
	PutProps(ctx context.Context, siteID, key, propsJSON string) error
	ListAliases(ctx context.Context, siteID string, limit int) ([]AliasRow, error)
	PutAlias(ctx context.Context, siteID, alias, canonical, actor string, active bool) error
	ListTombstones(ctx context.Context, siteID string, limit int) ([]string, error)
	IsTombstoned(ctx context.Context, siteID, key string) (bool, error)
	PutTombstone(ctx context.Context, siteID, key, actor string) error
}
