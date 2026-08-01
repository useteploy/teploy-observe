package backup

import (
	"bytes"
	"io"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	cases := []int{0, 1, 100, backupEncChunkSize - 1, backupEncChunkSize, backupEncChunkSize + 1, backupEncChunkSize*3 + 12345}
	for _, size := range cases {
		plain := bytes.Repeat([]byte{0xAB}, size)
		var out bytes.Buffer
		ew, err := newEncryptWriter(&out, key)
		if err != nil {
			t.Fatalf("size %d: newEncryptWriter: %v", size, err)
		}
		if _, err := ew.Write(plain); err != nil {
			t.Fatalf("size %d: write: %v", size, err)
		}
		if err := ew.Close(); err != nil {
			t.Fatalf("size %d: close: %v", size, err)
		}

		dr, err := newDecryptReader(bytes.NewReader(out.Bytes()), key)
		if err != nil {
			t.Fatalf("size %d: newDecryptReader: %v", size, err)
		}
		got, err := io.ReadAll(dr)
		if err != nil {
			t.Fatalf("size %d: read: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round-trip mismatch: got %d bytes, want %d bytes", size, len(got), len(plain))
		}
	}
}

func TestDecrypt_WrongKeyFailsClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	wrongKey := bytes.Repeat([]byte{0x99}, 32)
	plain := []byte("some backup content that must never leak")

	var out bytes.Buffer
	ew, err := newEncryptWriter(&out, key)
	if err != nil {
		t.Fatal(err)
	}
	ew.Write(plain)
	ew.Close()

	dr, err := newDecryptReader(bytes.NewReader(out.Bytes()), wrongKey)
	if err != nil {
		t.Fatal(err) // header/magic parse itself doesn't need the key
	}
	if _, err := io.ReadAll(dr); err == nil {
		t.Fatal("expected wrong key to fail closed, got no error")
	}
}

func TestDecrypt_TamperedCiphertextFailsClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plain := bytes.Repeat([]byte{0xCD}, backupEncChunkSize+500) // spans two chunks

	var out bytes.Buffer
	ew, err := newEncryptWriter(&out, key)
	if err != nil {
		t.Fatal(err)
	}
	ew.Write(plain)
	ew.Close()

	tampered := append([]byte(nil), out.Bytes()...)
	tampered[len(tampered)-1] ^= 0xFF

	dr, err := newDecryptReader(bytes.NewReader(tampered), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(dr); err == nil {
		t.Fatal("expected tampered ciphertext to fail closed, got no error")
	}
}

func TestDecrypt_TruncatedStreamFailsClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plain := bytes.Repeat([]byte{0xEF}, backupEncChunkSize*2)

	var out bytes.Buffer
	ew, err := newEncryptWriter(&out, key)
	if err != nil {
		t.Fatal(err)
	}
	ew.Write(plain)
	ew.Close()

	truncated := out.Bytes()[:out.Len()-50]
	dr, err := newDecryptReader(bytes.NewReader(truncated), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(dr); err == nil {
		t.Fatal("expected truncated stream to fail closed, got no error")
	}
}

func TestDecrypt_BadMagicRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	if _, err := newDecryptReader(bytes.NewReader([]byte("not an encrypted backup at all!!")), key); err == nil {
		t.Fatal("expected bad magic to be rejected")
	}
}

func TestLoadBackupEncryptionKey(t *testing.T) {
	t.Run("unset returns nil, nil", func(t *testing.T) {
		t.Setenv("OBSERVE_BACKUP_ENCRYPTION_KEY", "")
		key, err := LoadBackupEncryptionKey()
		if err != nil || key != nil {
			t.Fatalf("expected (nil, nil), got (%v, %v)", key, err)
		}
	})
	t.Run("valid base64 32 bytes", func(t *testing.T) {
		t.Setenv("OBSERVE_BACKUP_ENCRYPTION_KEY", "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoxMjM0NTY=")
		key, err := LoadBackupEncryptionKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(key) != 32 {
			t.Fatalf("expected 32-byte key, got %d", len(key))
		}
	})
	t.Run("invalid base64 rejected", func(t *testing.T) {
		t.Setenv("OBSERVE_BACKUP_ENCRYPTION_KEY", "not-valid-base64!!!")
		if _, err := LoadBackupEncryptionKey(); err == nil {
			t.Fatal("expected an error for invalid base64")
		}
	})
	t.Run("wrong length rejected", func(t *testing.T) {
		t.Setenv("OBSERVE_BACKUP_ENCRYPTION_KEY", "dG9vc2hvcnQ=") // "tooshort", 8 bytes
		if _, err := LoadBackupEncryptionKey(); err == nil {
			t.Fatal("expected an error for a key of the wrong length")
		}
	})
}
