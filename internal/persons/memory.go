package persons

import (
	"context"
	"sort"
	"sync"
)

// Memory is an in-memory EventSource + IdentityStore. It exists so unit and
// handler tests (here and in cmd/observe) can exercise the service without a
// Nucleus. Every operation is keyed by site, mirroring the SQL scoping.
type Memory struct {
	mu     sync.Mutex
	events map[string]map[string][]PersonEvent // site -> key -> events
	aggs   map[string]map[string]Person        // site -> key -> aggregate
	props  map[string]map[string]string
	alias  map[string]map[string]AliasRow // site -> alias -> edge (active only)
	tomb   map[string]map[string]bool
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{
		events: map[string]map[string][]PersonEvent{},
		aggs:   map[string]map[string]Person{},
		props:  map[string]map[string]string{},
		alias:  map[string]map[string]AliasRow{},
		tomb:   map[string]map[string]bool{},
	}
}

// Service builds a Service over m.
func (m *Memory) Service() *Service { return NewServiceFromParts(m, m) }

// AddPerson seeds a person aggregate (and one timeline event) for a site.
func (m *Memory) AddPerson(site string, p Person) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.aggs[site] == nil {
		m.aggs[site] = map[string]Person{}
		m.events[site] = map[string][]PersonEvent{}
	}
	m.aggs[site][p.DistinctID] = p
	m.events[site][p.DistinctID] = []PersonEvent{{EventID: "e-" + p.DistinctID, EventType: "pageview", Timestamp: p.LastSeenMs}}
}

func (m *Memory) List(_ context.Context, site string, _, _ int64, limit, offset int, anon bool) ([]Person, error) {
	all, _ := m.ListAll(context.Background(), site, 0, 0, 1<<30, anon)
	if offset >= len(all) {
		return nil, nil
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (m *Memory) Count(ctx context.Context, site string, _, _ int64, anon bool) (int64, error) {
	all, _ := m.ListAll(ctx, site, 0, 0, 1<<30, anon)
	return int64(len(all)), nil
}

func (m *Memory) ListAll(_ context.Context, site string, _, _ int64, limit int, anon bool) ([]Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Person
	for _, p := range m.aggs[site] {
		if p.DistinctID == "" && !anon {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenMs > out[j].LastSeenMs })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Aggregate(_ context.Context, site, key string) (Person, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.aggs[site][key]
	return p, ok, nil
}

func (m *Memory) Timeline(_ context.Context, site, key string) ([]PersonEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]PersonEvent(nil), m.events[site][key]...), nil
}

func (m *Memory) Exists(_ context.Context, site, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.aggs[site][key]
	return ok, nil
}

func (m *Memory) GetProps(_ context.Context, site, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.props[site][key]
	return v, ok, nil
}

func (m *Memory) PutProps(_ context.Context, site, key, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.props[site] == nil {
		m.props[site] = map[string]string{}
	}
	m.props[site][key] = p
	return nil
}

func (m *Memory) ListAliases(_ context.Context, site string, limit int) ([]AliasRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AliasRow
	for _, a := range m.alias[site] {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AliasKey < out[j].AliasKey })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) PutAlias(_ context.Context, site, alias, canonical, _ string, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.alias[site] == nil {
		m.alias[site] = map[string]AliasRow{}
	}
	if active {
		m.alias[site][alias] = AliasRow{AliasKey: alias, CanonicalKey: canonical}
	} else {
		delete(m.alias[site], alias)
	}
	return nil
}

func (m *Memory) ListTombstones(_ context.Context, site string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.tomb[site] {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) IsTombstoned(_ context.Context, site, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tomb[site][key], nil
}

func (m *Memory) PutTombstone(_ context.Context, site, key, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tomb[site] == nil {
		m.tomb[site] = map[string]bool{}
	}
	m.tomb[site][key] = true
	return nil
}
