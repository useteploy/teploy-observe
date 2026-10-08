package mcp

import (
	"context"
	"sync"
	"testing"
)

type memoryTokenRepository struct {
	mu          sync.Mutex
	rows        []Token
	readStarted chan struct{}
	resumeRead  chan struct{}
	once        sync.Once
}

func (m *memoryTokenRepository) read(_ context.Context, _ string, _ ...any) ([]Token, error) {
	m.mu.Lock()
	latest := m.rows[len(m.rows)-1]
	for _, row := range m.rows {
		if row.RevokedAt > latest.RevokedAt {
			latest.RevokedAt = row.RevokedAt
		}
	}
	m.mu.Unlock()
	m.once.Do(func() {
		if m.readStarted != nil {
			close(m.readStarted)
			<-m.resumeRead
		}
	})
	return []Token{latest}, nil
}
func (m *memoryTokenRepository) write(_ context.Context, tok Token, version int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok.UpdatedAt = version
	m.rows = append(m.rows, tok)
	return nil
}
func TestVerifyRefreshCannotRaceRevoke(t *testing.T) {
	plain := TokenPrefix + "fixture"
	repo := &memoryTokenRepository{rows: []Token{{ID: "fixture", Hash: hashToken(plain), UpdatedAt: 1}}, readStarted: make(chan struct{}), resumeRead: make(chan struct{})}
	store := &TokenStore{repository: repo}
	verified := make(chan bool, 1)
	go func() { _, ok := store.Verify(context.Background(), plain); verified <- ok }()
	<-repo.readStarted
	// The verifier is paused after its authoritative read. Its critical section
	// must still be held while a revocation is queued.
	if store.mu.TryLock() {
		store.mu.Unlock()
		t.Error("Verify released the credential lock before persisting last-used")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- store.Revoke(context.Background(), "fixture") }()
	close(repo.resumeRead)
	if !<-verified {
		t.Fatal("initial live token denied")
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(context.Background(), plain); ok {
		t.Fatal("revoked secret authenticated")
	}
	list, err := store.List(context.Background())
	if err != nil || len(list) != 1 || !list[0].Revoked() {
		t.Fatalf("revoked List: %v %v", list, err)
	}
}
func TestRevocationSurvivesStaleVersion(t *testing.T) {
	// Even a stale last-used append from another store instance cannot override
	// a historical revocation. The actual SQL MAX collapse is tested live below.
	plain := TokenPrefix + "fixture"
	repo := &memoryTokenRepository{rows: []Token{
		{ID: "fixture", Hash: hashToken(plain), RevokedAt: 2, UpdatedAt: 2},
		{ID: "fixture", Hash: hashToken(plain), RevokedAt: 0, UpdatedAt: 3},
	}}
	store := &TokenStore{repository: repo}
	if _, ok := store.Verify(context.Background(), plain); ok {
		t.Fatal("stale write resurrected token")
	}
}
func TestTokenRevocationIrreversibleAtEngine(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	plain, tok, err := store.Create(ctx, "irreversible fixture", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := store.read(ctx, "token_id = $1", tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok.LastUsedAt = rows[0].UpdatedAt + 10
	if err := store.write(ctx, tok, rows[0].UpdatedAt, tok.LastUsedAt); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(ctx, plain); ok {
		t.Fatal("engine collapse resurrected credential")
	}
	rows, err = store.read(ctx, "token_id = $1", tok.ID)
	if err != nil || !rows[0].Revoked() {
		t.Fatalf("revoked row lost: %v %v", rows, err)
	}
}
