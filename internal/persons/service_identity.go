package persons

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// ListResult is the resolved, erasure-filtered person page.
type ListResult struct {
	Persons []Person
	Total   int64
	// Resolved is true when alias resolution / erasure filtering ran in Go.
	Resolved bool
	// Truncated is true when the Go-side fold hit its scan cap, so Total
	// and ordering cover only the most recently active persons.
	Truncated bool
}

func (s *Service) loadResolver(ctx context.Context, siteID string) (*Resolver, error) {
	rows, err := s.st.ListAliases(ctx, siteID, MaxAliasesPerSite+1)
	if err != nil {
		return nil, err
	}
	return NewResolver(rows), nil
}

func (s *Service) loadTombstones(ctx context.Context, siteID string) (map[string]bool, error) {
	keys, err := s.st.ListTombstones(ctx, siteID, MaxTombstonesPerSite+1)
	if err != nil {
		return nil, err
	}
	if len(keys) > MaxTombstonesPerSite {
		// Fail closed: an incomplete exclusion set would leak erased persons.
		return nil, fmt.Errorf("%w: tombstone set exceeds %d", ErrLimit, MaxTombstonesPerSite)
	}
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m, nil
}

// ListResolved is the user-facing person list: aliases folded onto their
// canonical person, erased persons excluded. With no aliases and no
// tombstones for the site it is exactly the SQL-paginated C2 path.
func (s *Service) ListResolved(ctx context.Context, siteID string, fromMs, toMs int64, limit, offset int, includeAnonymous bool) (ListResult, error) {
	if siteID == "" {
		return ListResult{Persons: []Person{}}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	res, err := s.loadResolver(ctx, siteID)
	if err != nil {
		return ListResult{}, err
	}
	tomb, err := s.loadTombstones(ctx, siteID)
	if err != nil {
		return ListResult{}, err
	}
	if res.Empty() && len(tomb) == 0 {
		rows, err := s.ListPersons(ctx, siteID, fromMs, toMs, limit, offset, includeAnonymous)
		if err != nil {
			return ListResult{}, err
		}
		total, _ := s.CountPersons(ctx, siteID, fromMs, toMs, includeAnonymous)
		return ListResult{Persons: rows, Total: total}, nil
	}

	raw, err := s.ev.ListAll(ctx, siteID, fromMs, toMs, resolveScanCap+1, includeAnonymous)
	if err != nil {
		return ListResult{}, err
	}
	truncated := len(raw) > resolveScanCap
	if truncated {
		raw = raw[:resolveScanCap]
	}
	folded := foldPersons(raw, res, tomb)
	total := int64(len(folded))
	if offset >= len(folded) {
		folded = nil
	} else {
		folded = folded[offset:]
		if len(folded) > limit {
			folded = folded[:limit]
		}
	}
	if folded == nil {
		folded = []Person{}
	}
	return ListResult{Persons: folded, Total: total, Resolved: true, Truncated: truncated}, nil
}

// PersonDetail returns the lifetime aggregate for distinctID plus the most
// recent 100 events. If distinctID is an alias it resolves to its canonical
// person and the aggregate, timeline and properties cover the whole merged
// group. An erased person returns ErrErased. distinctID = ” returns an
// empty result on purpose — anonymous-aggregated detail makes no sense.
func (s *Service) PersonDetail(ctx context.Context, siteID, distinctID string) (PersonDetail, error) {
	if siteID == "" || distinctID == "" {
		return PersonDetail{}, nil
	}
	res, err := s.loadResolver(ctx, siteID)
	if err != nil {
		return PersonDetail{}, err
	}
	canon := res.Canonical(distinctID)
	tomb, err := s.loadTombstones(ctx, siteID)
	if err != nil {
		return PersonDetail{}, err
	}
	if tomb[distinctID] || tomb[canon] {
		return PersonDetail{}, ErrErased
	}
	members := []string{canon}
	var aliases []string
	for _, m := range res.Members(canon) {
		if !tomb[m] {
			members = append(members, m)
			aliases = append(aliases, m)
		}
	}
	if len(members) > MaxAliasesPerPerson+1 {
		members = members[:MaxAliasesPerPerson+1]
	}

	var agg Person
	have := false
	var timeline []PersonEvent
	var propParts []map[string]any
	for i, m := range members {
		p, ok, err := s.ev.Aggregate(ctx, siteID, m)
		if err != nil {
			return PersonDetail{}, err
		}
		if ok {
			if !have {
				agg, have = p, true
			} else {
				agg.absorb(p)
			}
		}
		if tl, err := s.ev.Timeline(ctx, siteID, m); err == nil {
			timeline = append(timeline, tl...)
		}
		// Canonical (index 0) must win, so apply it last.
		raw, found, err := s.st.GetProps(ctx, siteID, m)
		if err != nil {
			return PersonDetail{}, err
		}
		if found {
			var pm map[string]any
			if json.Unmarshal([]byte(raw), &pm) == nil {
				if i == 0 {
					propParts = append(propParts, pm)
				} else {
					propParts = append([]map[string]any{pm}, propParts...)
				}
			}
		}
	}
	agg.DistinctID = canon
	if !have {
		agg = Person{DistinctID: canon}
	}
	sort.SliceStable(timeline, func(i, j int) bool { return timeline[i].Timestamp > timeline[j].Timestamp })
	if len(timeline) > 100 {
		timeline = timeline[:100]
	}
	if timeline == nil {
		timeline = []PersonEvent{}
	}
	props := map[string]any{}
	for _, part := range propParts {
		for k, v := range part {
			props[k] = v
		}
	}
	if aliases == nil {
		aliases = []string{}
	}
	return PersonDetail{Person: agg, Timeline: timeline, Properties: props, Aliases: aliases, CanonicalKey: canon}, nil
}

// SetProperties applies an identify() trait batch to one person. Merge mode
// (default) sets the given keys and removes null-valued ones; replace mode
// stores exactly the given set. The resulting document is bounded to
// MaxPropertyKeys. Writes for an erased person are refused.
func (s *Service) SetProperties(ctx context.Context, siteID, key string, in map[string]any, replace bool) (map[string]any, error) {
	if siteID == "" {
		return nil, fmt.Errorf("%w: site_id required", ErrInvalid)
	}
	if err := ValidatePersonKey(key); err != nil {
		return nil, err
	}
	set, remove, err := ValidateProperties(in)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if gone, err := s.st.IsTombstoned(ctx, siteID, key); err != nil {
		return nil, err
	} else if gone {
		return nil, ErrErased
	}
	doc := map[string]any{}
	if !replace {
		raw, found, err := s.st.GetProps(ctx, siteID, key)
		if err != nil {
			return nil, err
		}
		if found {
			if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc == nil {
				doc = map[string]any{}
			}
		}
		for _, k := range remove {
			delete(doc, k)
		}
	}
	for k, v := range set {
		doc[k] = v
	}
	if len(doc) > MaxPropertyKeys {
		return nil, fmt.Errorf("%w: a person holds at most %d properties", ErrInvalid, MaxPropertyKeys)
	}
	enc, err := json.Marshal(doc) // map keys marshal sorted: deterministic
	if err != nil {
		return nil, fmt.Errorf("encode properties: %w", err)
	}
	if err := s.st.PutProps(ctx, siteID, key, string(enc)); err != nil {
		return nil, err
	}
	return doc, nil
}

// SetKnownProperties is the telemetry-key write path: merge mode only, and
// only for a person that already has events in THIS site (ErrNotFound
// otherwise), so a public key cannot create person rows for arbitrary ids.
// It never returns stored values: only the sorted names of the keys the
// caller sent.
func (s *Service) SetKnownProperties(ctx context.Context, siteID, key string, in map[string]any) ([]string, error) {
	return s.writeKnown(ctx, siteID, key, in, false)
}

// ReplaceProperties is the authenticated-editor path (never the telemetry
// key): the person's whole trait set becomes exactly in. The person must
// already exist.
func (s *Service) ReplaceProperties(ctx context.Context, siteID, key string, in map[string]any) ([]string, error) {
	return s.writeKnown(ctx, siteID, key, in, true)
}

func (s *Service) writeKnown(ctx context.Context, siteID, key string, in map[string]any, replace bool) ([]string, error) {
	if siteID == "" {
		return nil, fmt.Errorf("%w: site_id required", ErrInvalid)
	}
	if err := ValidatePersonKey(key); err != nil {
		return nil, err
	}
	if _, _, err := ValidateProperties(in); err != nil {
		return nil, err // cheap checks first: no store round-trip for junk
	}
	ok, err := s.ev.Exists(ctx, siteID, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: person", ErrNotFound)
	}
	if _, err := s.SetProperties(ctx, siteID, key, in, replace); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(in))
	for k := range in {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// MergeResult describes a completed merge.
type MergeResult struct {
	SiteID       string `json:"site_id"`
	FromKey      string `json:"from_key"`
	CanonicalKey string `json:"canonical_key"`
}

// Merge records fromKey as an alias of intoKey's canonical person. Nothing
// is rewritten: reads resolve the alias forward. Self-merge, merging an
// existing alias, and any merge that would close a cycle are refused; both
// keys must have events in THIS site (so a key from another site is a 404,
// never a cross-site link).
func (s *Service) Merge(ctx context.Context, siteID, fromKey, intoKey, actor string) (MergeResult, error) {
	if siteID == "" {
		return MergeResult{}, fmt.Errorf("%w: site_id required", ErrInvalid)
	}
	if err := ValidatePersonKey(fromKey); err != nil {
		return MergeResult{}, err
	}
	if err := ValidatePersonKey(intoKey); err != nil {
		return MergeResult{}, err
	}
	if fromKey == intoKey {
		return MergeResult{}, fmt.Errorf("%w: cannot merge a person into itself", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tomb, err := s.loadTombstones(ctx, siteID)
	if err != nil {
		return MergeResult{}, err
	}
	if tomb[fromKey] || tomb[intoKey] {
		return MergeResult{}, ErrErased
	}
	rows, err := s.st.ListAliases(ctx, siteID, MaxAliasesPerSite+1)
	if err != nil {
		return MergeResult{}, err
	}
	if len(rows) >= MaxAliasesPerSite {
		return MergeResult{}, fmt.Errorf("%w: site already has %d aliases", ErrLimit, MaxAliasesPerSite)
	}
	res := NewResolver(rows)
	if res.IsAlias(fromKey) {
		return MergeResult{}, fmt.Errorf("%w: %s is already merged into %s", ErrConflict, fromKey, res.Canonical(fromKey))
	}
	root := res.Canonical(intoKey)
	if root == fromKey {
		return MergeResult{}, fmt.Errorf("%w: %s is already an alias of %s (merge would create a cycle)", ErrConflict, intoKey, fromKey)
	}
	if tomb[root] {
		return MergeResult{}, ErrErased
	}
	if len(res.Members(root))+1+len(res.Members(fromKey))+1 > MaxAliasesPerPerson {
		return MergeResult{}, fmt.Errorf("%w: a person holds at most %d aliases", ErrLimit, MaxAliasesPerPerson)
	}
	for _, k := range []string{fromKey, intoKey} {
		ok, err := s.ev.Exists(ctx, siteID, k)
		if err != nil {
			return MergeResult{}, err
		}
		if !ok {
			return MergeResult{}, fmt.Errorf("%w: no person %q in this site", ErrNotFound, k)
		}
	}
	if err := s.st.PutAlias(ctx, siteID, fromKey, root, actor, true); err != nil {
		return MergeResult{}, err
	}
	return MergeResult{SiteID: siteID, FromKey: fromKey, CanonicalKey: root}, nil
}

// EraseResult describes a completed erasure.
type EraseResult struct {
	SiteID string   `json:"site_id"`
	Erased []string `json:"erased_keys"`
	// EventsDeleted is always false: see docs/IDENTITY_MODEL_ADR.md. The
	// person is tombstoned (excluded from listing/detail, properties and
	// aliases cleared) but historical event rows remain until retention.
	EventsDeleted bool `json:"events_deleted"`
}

// Erase is the GDPR erasure path. For the key and, if it is a canonical
// person, every alias merged into it: write a tombstone, deactivate the
// alias edge and blank the stored properties. Idempotent. Events are NOT
// deleted (no proven per-key DELETE on events; see the ADR).
func (s *Service) Erase(ctx context.Context, siteID, key, actor string) (EraseResult, error) {
	if siteID == "" {
		return EraseResult{}, fmt.Errorf("%w: site_id required", ErrInvalid)
	}
	if err := ValidatePersonKey(key); err != nil {
		return EraseResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.loadResolver(ctx, siteID)
	if err != nil {
		return EraseResult{}, err
	}
	keys := []string{key}
	keys = append(keys, res.Members(key)...)
	// Tombstones first: the exclusion is the load-bearing part, so a later
	// failure leaves the person hidden rather than half-cleared and visible.
	for _, k := range keys {
		if err := s.st.PutTombstone(ctx, siteID, k, actor); err != nil {
			return EraseResult{}, err
		}
	}
	for _, k := range keys {
		if canon := res.Canonical(k); canon != k {
			if err := s.st.PutAlias(ctx, siteID, k, canon, actor, false); err != nil {
				return EraseResult{}, err
			}
		}
		if _, found, err := s.st.GetProps(ctx, siteID, k); err != nil {
			return EraseResult{}, err
		} else if found {
			if err := s.st.PutProps(ctx, siteID, k, "{}"); err != nil {
				return EraseResult{}, err
			}
		}
	}
	return EraseResult{SiteID: siteID, Erased: keys}, nil
}
