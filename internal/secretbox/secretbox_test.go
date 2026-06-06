package secretbox

import "testing"

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "test-master-key")
	enc, err := Encrypt("sk-supersecret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !IsEncrypted(enc) {
		t.Fatalf("expected encrypted marker, got %q", enc)
	}
	if enc == "sk-supersecret" {
		t.Fatal("ciphertext equals plaintext")
	}
	dec, err := Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != "sk-supersecret" {
		t.Fatalf("round-trip mismatch: %q", dec)
	}
}

func TestEncryptFailsClosedWithoutKey(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "")
	if _, err := Encrypt("secret"); err == nil {
		t.Fatal("expected error when no master key is set")
	}
}

func TestDecryptPlaintextPassThrough(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "")
	// Legacy plaintext (no marker) decrypts as-is even without a key.
	got, err := Decrypt("legacy-plaintext")
	if err != nil {
		t.Fatalf("decrypt plaintext: %v", err)
	}
	if got != "legacy-plaintext" {
		t.Fatalf("want passthrough, got %q", got)
	}
}

func TestDecryptMarkedFailsWithoutKey(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "k")
	enc, _ := Encrypt("secret")
	t.Setenv("OBSERVE_SECRET_KEY", "")
	if _, err := Decrypt(enc); err == nil {
		t.Fatal("expected fail-closed decrypt without key")
	}
}

func TestEmptyStays(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "")
	got, err := Encrypt("")
	if err != nil || got != "" {
		t.Fatalf("empty should stay empty with no error, got %q err %v", got, err)
	}
}
