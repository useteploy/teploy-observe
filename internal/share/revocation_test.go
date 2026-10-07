package share

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestConfirmedRevocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []row
		want error
	}{
		{"zero matching rows", nil, ErrNotFound},
		{"still active after update", []row{{Token: "fixture"}}, ErrRevokeUnconfirmed},
		{"one still active duplicate", []row{{RevokedAt: 1}, {}}, ErrRevokeUnconfirmed},
		{"confirmed", []row{{RevokedAt: 1}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := confirmedRevocation(tc.rows)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTokenForID(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	id := shareLinkID(token)
	if got, err := tokenForID([]row{{Token: token}}, id); err != nil || got != token {
		t.Fatalf("ID lookup: %q, %v", got, err)
	}
	for _, bad := range []string{"", token, maskToken(token), "unknown"} {
		if _, err := tokenForID([]row{{Token: token}}, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("non-ID %q: %v", bad, err)
		}
	}
	if _, err := tokenForID([]row{{Token: token}, {Token: token}}, id); !errors.Is(err, ErrAmbiguousID) {
		t.Fatalf("duplicate ID must not choose a row: %v", err)
	}
}

// OBS26-03: exercise the actual list -> ID revoke -> reload -> original raw
// token refused contract on Nucleus, not a fake store with invented semantics.
func TestShare_ListRevokeByID(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)
	site := fmt.Sprintf("share-id-%d", time.Now().UnixNano())
	created, err := svc.Create(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	links, err := svc.List(ctx, site)
	if err != nil || len(links) != 1 {
		t.Fatalf("list: count=%d err=%v", len(links), err)
	}
	listed := links[0]
	if listed.ID != created.ID || listed.Token == created.Token {
		t.Fatal("list must retain the ID and mask the token")
	}
	if _, err := svc.Resolve(ctx, listed.ID); err == nil {
		t.Fatal("a management ID must not resolve as a capability")
	}
	for _, target := range [][2]string{{site + "-other", listed.ID}, {site, listed.Token}, {site, "unknown"}} {
		if _, err := svc.RevokeByID(ctx, target[0], target[1]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wrong site/ID: %v", err)
		}
	}
	if got, err := svc.Resolve(ctx, created.Token); err != nil || got != site {
		t.Fatalf("rejected revocation must leave the real token active: site=%q err=%v", got, err)
	}
	revoked, err := svc.RevokeByID(ctx, site, listed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.ID != listed.ID || revoked.Token == created.Token || revoked.Status != "revoked" || revoked.RevokedAt == 0 {
		t.Fatal("revoke response must confirm the same row, masked and revoked")
	}
	if _, err := svc.Resolve(ctx, created.Token); err == nil {
		t.Fatal("successful revocation must immediately invalidate the original raw capability")
	}
	again, err := svc.RevokeByID(ctx, site, listed.ID)
	if err != nil || again.RevokedAt != revoked.RevokedAt {
		t.Fatalf("idempotent revoke: %v", err)
	}
	links, err = svc.List(ctx, site)
	if err != nil || len(links) != 1 || links[0].Status != "revoked" || links[0].ID != listed.ID {
		t.Fatalf("revoked history must survive reload: %v", err)
	}
}

func TestShare_LegacyRevokeRejectsZeroRows(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)
	link, err := svc.Create(ctx, fmt.Sprintf("share-legacy-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "unknown", maskToken(link.Token), link.ID} {
		if err := svc.Revoke(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("zero-row revoke must not succeed: %v", err)
		}
	}
	if _, err := svc.Resolve(ctx, link.Token); err != nil {
		t.Fatalf("masked/unknown revocations must not invalidate the real link: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.Revoke(ctx, link.Token); err != nil {
			t.Fatalf("legacy known-token revoke %d: %v", i, err)
		}
	}
	if _, err := svc.Resolve(ctx, link.Token); err == nil {
		t.Fatal("legacy revocation must invalidate the raw token")
	}
}
