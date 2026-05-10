package identity

import "testing"

func TestHashDistinctID_StableForSameInputs(t *testing.T) {
	a := HashDistinctID("user-123", "salt-A")
	b := HashDistinctID("user-123", "salt-A")
	if a != b {
		t.Fatalf("same input must hash to same output: %q vs %q", a, b)
	}
}

func TestHashDistinctID_DifferentSaltDifferentOutput(t *testing.T) {
	a := HashDistinctID("user-123", "salt-A")
	b := HashDistinctID("user-123", "salt-B")
	if a == b {
		t.Fatalf("salt is the keying material — different salts must yield different hashes (got %q both)", a)
	}
}

func TestHashDistinctID_DifferentInputsDifferentOutput(t *testing.T) {
	a := HashDistinctID("user-1", "shared-salt")
	b := HashDistinctID("user-2", "shared-salt")
	if a == b {
		t.Fatalf("different raw IDs must hash differently (collision: %q)", a)
	}
}

func TestHashDistinctID_TruncatedTo16Hex(t *testing.T) {
	got := HashDistinctID("anything", "any-salt")
	if len(got) != HashedIDLength {
		t.Fatalf("len = %d, want %d (got=%q)", len(got), HashedIDLength, got)
	}
	for i, r := range got {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("char %d = %q is not lowercase hex", i, r)
		}
	}
}

func TestHashDistinctID_EmptyRawReturnsEmpty(t *testing.T) {
	// Anonymous events should not store an HMAC of "" — that would be a
	// constant value across every site and defeat the privacy purpose.
	if got := HashDistinctID("", "salt"); got != "" {
		t.Fatalf("empty raw must return empty, got %q", got)
	}
}

func TestMaybeHashDistinctID_RawOptInBypassesHash(t *testing.T) {
	raw := "user-123"
	got := MaybeHashDistinctID(raw, "salt", true)
	if got != raw {
		t.Fatalf("raw opt-in must return the input unchanged, got %q", got)
	}
}

func TestMaybeHashDistinctID_DefaultHashes(t *testing.T) {
	raw := "user-123"
	got := MaybeHashDistinctID(raw, "salt", false)
	if got == raw {
		t.Fatalf("default behavior must hash, got passthrough %q", got)
	}
	if len(got) != HashedIDLength {
		t.Fatalf("hash length = %d, want %d", len(got), HashedIDLength)
	}
}
