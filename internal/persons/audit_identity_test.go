package persons

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRotatingMergeRootsCountAndEraseCompleteGroup(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	for i := 0; i < MaxAliasesPerPerson+2; i++ {
		seed(m, "s", fmt.Sprintf("p%02d", i))
	}
	for i := 0; i < MaxAliasesPerPerson; i++ {
		if _, err := s.Merge(bg, "s", fmt.Sprintf("p%02d", i), fmt.Sprintf("p%02d", i+1), "actor"); err != nil {
			t.Fatalf("merge %d: %v", i, err)
		}
	}
	if _, err := s.Merge(bg, "s", "p20", "p21", "actor"); !errors.Is(err, ErrLimit) {
		t.Fatalf("group limit escaped: %v", err)
	}
	r, err := s.loadResolver(bg, "s")
	if err != nil || r.Canonical("p00") != "p20" || len(r.Members("p20")) != 20 {
		t.Fatalf("complete resolution: %+v %v", r, err)
	}
	// Erasing any member must hide/clean the complete canonical group.
	erased, err := s.Erase(bg, "s", "p00", "actor")
	if err != nil || len(erased.Erased) != 21 {
		t.Fatalf("erase: %+v %v", erased, err)
	}
	for _, key := range erased.Erased {
		if _, err := s.PersonDetail(bg, "s", key); !errors.Is(err, ErrErased) {
			t.Fatalf("visible member %q: %v", key, err)
		}
		gone, err := s.IsErased(bg, "s", key)
		if err != nil || !gone {
			t.Fatalf("admission allows %q", key)
		}
	}
}

func TestLegacyDeepChainAndCorruptGraph(t *testing.T) {
	for _, depth := range []int{15, 16, 17, 32} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			m := NewMemory()
			s := m.Service()
			for i := 0; i <= depth; i++ {
				key := fmt.Sprint(i)
				seed(m, "s", key)
				_ = m.PutProps(bg, "s", key, `{"email":"private"}`)
				if i < depth {
					_ = m.PutAlias(bg, "s", key, fmt.Sprint(i+1), "", true)
				}
			}
			got, err := s.Erase(bg, "s", fmt.Sprint(depth), "")
			if err != nil || len(got.Erased) != depth+1 {
				t.Fatalf("legacy work set: %+v %v", got, err)
			}
			for _, key := range got.Erased {
				p, _, _ := m.GetProps(bg, "s", key)
				if p != "{}" {
					t.Fatalf("properties retained %s", key)
				}
			}
		})
	}
	m := NewMemory()
	s := m.Service()
	seed(m, "s", "a", "b")
	_ = m.PutAlias(bg, "s", "a", "b", "", true)
	_ = m.PutAlias(bg, "s", "b", "a", "", true)
	if _, err := s.ListResolved(bg, "s", 0, 0, 10, 0, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle read did not fail closed: %v", err)
	}
	if _, err := s.Erase(bg, "s", "a", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle erase silently succeeded: %v", err)
	}
}

type eraseFaultStore struct {
	*Memory
	failKey, failOp string
	fired           bool
}

func (s *eraseFaultStore) PutProps(ctx context.Context, site, key, props string) error {
	if !s.fired && s.failOp == "props" && key == s.failKey {
		s.fired = true
		return errors.New("injected property write")
	}
	return s.Memory.PutProps(ctx, site, key, props)
}
func (s *eraseFaultStore) PutAlias(ctx context.Context, site, key, root, actor string, active bool) error {
	if !s.fired && s.failOp == "alias" && key == s.failKey && !active {
		s.fired = true
		return errors.New("injected alias write")
	}
	return s.Memory.PutAlias(ctx, site, key, root, actor, active)
}
func (s *eraseFaultStore) PutTombstone(ctx context.Context, site, key, actor string) error {
	if !s.fired && s.failOp == "tomb" && key == s.failKey {
		s.fired = true
		return errors.New("injected tombstone write")
	}
	return s.Memory.PutTombstone(ctx, site, key, actor)
}

func TestInterruptedEraseRetriesCompleteCleanup(t *testing.T) {
	for _, op := range []string{"props", "alias", "tomb"} {
		for _, key := range []string{"root", "a", "b"} {
			if op == "alias" && key == "root" {
				continue
			}
			for _, retry := range []string{"root", "a"} {
				t.Run(op+"/"+key+"/"+retry, func(t *testing.T) {
					m := NewMemory()
					seed(m, "s", "root", "a", "b")
					for _, k := range []string{"root", "a", "b"} {
						_ = m.PutProps(bg, "s", k, `{"email":"private"}`)
					}
					_ = m.PutAlias(bg, "s", "a", "root", "", true)
					_ = m.PutAlias(bg, "s", "b", "root", "", true)
					st := &eraseFaultStore{Memory: m, failKey: key, failOp: op}
					s := NewServiceFromParts(m, st)
					if _, err := s.Erase(bg, "s", "root", ""); err == nil {
						t.Fatal("injected failure missing")
					}
					if _, err := s.Erase(bg, "s", retry, ""); err != nil {
						t.Fatal(err)
					}
					// If an edge was already removed, its properties were durably cleared
					// before that phase began. Remaining connected aliases finish on retry.
					for _, k := range []string{"root", "a", "b"} {
						p, _, _ := m.GetProps(bg, "s", k)
						if p != "{}" {
							t.Fatalf("%s retained %s", k, p)
						}
						gone, _ := m.IsTombstoned(bg, "s", k)
						if !gone {
							t.Fatalf("%s not hidden", k)
						}
					}
				})
			}
		}
	}
}

func TestRawIdentitiesManagedWithoutRewriting(t *testing.T) {
	for _, key := range []string{"auth0|fixture-user", "用户", "a/b", "a b", "a'b;x", strings.Repeat("x", MaxPersonKeyLen), "0123456789abcdef"} {
		t.Run(key, func(t *testing.T) {
			m := NewMemory()
			s := m.Service()
			seed(m, "s", key, "root")
			if _, err := s.SetKnownProperties(bg, "s", key, map[string]any{"email": "private"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PersonDetail(bg, "s", key); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Merge(bg, "s", key, "root", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Erase(bg, "s", key, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetKnownProperties(bg, "s", key, map[string]any{"email": "new"}); !errors.Is(err, ErrErased) {
				t.Fatalf("erased key writable: %v", err)
			}
		})
	}
}

func TestPartialTombstoneFailureStillRefusesAliasProperties(t *testing.T) {
	m := NewMemory()
	seed(m, "s", "root", "alias")
	_ = m.PutAlias(bg, "s", "alias", "root", "", true)
	st := &eraseFaultStore{Memory: m, failKey: "alias", failOp: "tomb"}
	s := NewServiceFromParts(m, st)
	if _, err := s.Erase(bg, "s", "root", ""); err == nil {
		t.Fatal("expected tombstone failure")
	}
	if _, err := s.SetProperties(bg, "s", "alias", map[string]any{"email": "private"}, false); !errors.Is(err, ErrErased) {
		t.Fatalf("hidden alias properties writable: %v", err)
	}
}

func TestResolverCompletesSiteBoundInLinearConstruction(t *testing.T) {
	rows := make([]AliasRow, MaxAliasesPerSite)
	for i := range rows {
		rows[i] = AliasRow{fmt.Sprint(i), fmt.Sprint(i + 1)}
	}
	r, err := checkedResolver(rows)
	if err != nil {
		t.Fatal(err)
	}
	if r.Canonical("0") != fmt.Sprint(MaxAliasesPerSite) || len(r.Members(fmt.Sprint(MaxAliasesPerSite))) != MaxAliasesPerSite {
		t.Fatal("site-bound chain truncated")
	}
	if _, err := checkedResolver(append(rows, AliasRow{"extra", "root"})); !errors.Is(err, ErrLimit) {
		t.Fatalf("incomplete alias set accepted: %v", err)
	}
}
