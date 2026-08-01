package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Backup archives contain password hashes, API keys, integration secrets,
// webhook secrets, and share tokens as plaintext JSON. Setting
// OBSERVE_BACKUP_ENCRYPTION_KEY wraps the archive in AES-256-GCM, chunked so
// memory stays bounded on arbitrarily large backups (a single GCM seal over
// the whole archive would mean buffering it all in memory first, defeating
// the point of streaming dumps/restores). Unset by default — existing
// plaintext backups keep working unchanged; DumpWithLog logs a one-time
// reminder that backups are unencrypted so this isn't a silent gap.

const (
	backupEncMagic     = "OBSBKv1\x00"
	backupEncChunkSize = 64 * 1024 // plaintext bytes per chunk
	backupEncNonceSize = 12        // AES-GCM standard nonce size
	backupEncKeySize   = 32        // AES-256
	backupEncPrefixLen = 8         // random bytes of the nonce, generated per-archive
)

// LoadBackupEncryptionKey reads OBSERVE_BACKUP_ENCRYPTION_KEY: standard
// base64 encoding of exactly 32 raw bytes (an AES-256 key). Returns (nil,
// nil) when the env var is unset — backups are then plaintext, the same as
// before this option existed. Returns an error when the env var is set but
// malformed, so a broken key is a loud, immediate failure rather than a
// silent fallback to plaintext.
func LoadBackupEncryptionKey() ([]byte, error) {
	raw := os.Getenv("OBSERVE_BACKUP_ENCRYPTION_KEY")
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("OBSERVE_BACKUP_ENCRYPTION_KEY: invalid base64: %w", err)
	}
	if len(key) != backupEncKeySize {
		return nil, fmt.Errorf("OBSERVE_BACKUP_ENCRYPTION_KEY: must decode to %d bytes (AES-256), got %d", backupEncKeySize, len(key))
	}
	return key, nil
}

// encryptWriter wraps an io.Writer, encrypting everything written to it in
// fixed-size chunks with AES-256-GCM. Wire format:
//
//	[8-byte magic "OBSBKv1\0"][8-byte random nonce prefix]
//	then a sequence of chunks, each:
//	  [4-byte big-endian ciphertext length][ciphertext (includes 16-byte tag)]
//
// The 12-byte nonce for chunk i is: [8-byte prefix][3-byte big-endian counter
// i][1-byte flag: 0x00 normal, 0x01 final]. The flag byte is part of the
// nonce (not a separate unauthenticated marker) specifically so a truncated
// ciphertext can never be mistaken for a complete archive: Close always
// emits one designated final chunk (even if empty), and the reader only
// accepts end-of-stream immediately after successfully authenticating a
// chunk whose nonce had the final flag set. A prefix is freshly randomized
// per archive (via newEncryptWriter), so nonce reuse across different
// backups under the same key never happens; within one archive the 3-byte
// counter gives room for 2^24 chunks (1 TiB at 64 KiB/chunk) before erroring
// rather than wrapping.
type encryptWriter struct {
	w      io.Writer
	gcm    cipher.AEAD
	prefix []byte
	buf    []byte
	idx    uint32
	closed bool
}

func newEncryptWriter(w io.Writer, key []byte) (*encryptWriter, error) {
	if len(key) != backupEncKeySize {
		return nil, fmt.Errorf("backup encryption key must be %d bytes, got %d", backupEncKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, backupEncPrefixLen)
	if _, err := rand.Read(prefix); err != nil {
		return nil, fmt.Errorf("generating archive nonce prefix: %w", err)
	}
	if _, err := w.Write([]byte(backupEncMagic)); err != nil {
		return nil, err
	}
	if _, err := w.Write(prefix); err != nil {
		return nil, err
	}
	return &encryptWriter{w: w, gcm: gcm, prefix: prefix, buf: make([]byte, 0, backupEncChunkSize)}, nil
}

// nonceFor builds the 12-byte nonce for chunk index i: prefix || counter(3) || flag(1).
func nonceFor(prefix []byte, i uint32, final bool) ([]byte, error) {
	if i >= 1<<24 {
		return nil, fmt.Errorf("backup archive exceeds the maximum chunk count (%d chunks at %d bytes each)", uint32(1)<<24, backupEncChunkSize)
	}
	n := make([]byte, backupEncNonceSize)
	copy(n, prefix)
	n[backupEncPrefixLen] = byte(i >> 16)
	n[backupEncPrefixLen+1] = byte(i >> 8)
	n[backupEncPrefixLen+2] = byte(i)
	if final {
		n[backupEncPrefixLen+3] = 0x01
	}
	return n, nil
}

func (e *encryptWriter) writeChunk(plain []byte, final bool) error {
	nonce, err := nonceFor(e.prefix, e.idx, final)
	if err != nil {
		return err
	}
	e.idx++
	ct := e.gcm.Seal(nil, nonce, plain, nil)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
	if _, err := e.w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = e.w.Write(ct)
	return err
}

func (e *encryptWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		room := backupEncChunkSize - len(e.buf)
		n := room
		if n > len(p) {
			n = len(p)
		}
		e.buf = append(e.buf, p[:n]...)
		p = p[n:]
		if len(e.buf) == backupEncChunkSize {
			if err := e.writeChunk(e.buf, false); err != nil {
				return 0, err
			}
			e.buf = e.buf[:0]
		}
	}
	return total, nil
}

// Close flushes any buffered plaintext as the final, specially-flagged chunk.
// Always emits a chunk (even an empty one), so the stream has an unambiguous
// authenticated end marker instead of relying on the underlying writer's EOF.
func (e *encryptWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	return e.writeChunk(e.buf, true)
}

// decryptReader is the inverse of encryptWriter — an io.Reader that decrypts
// on the fly and returns io.EOF only after successfully authenticating the
// designated final chunk. A stream that ends before that point (truncated,
// corrupted, or wrong key) returns an explicit error, never a silent
// truncated-but-looks-complete read.
type decryptReader struct {
	r      io.Reader
	gcm    cipher.AEAD
	prefix []byte
	idx    uint32
	buf    []byte // decrypted plaintext not yet returned to the caller
	done   bool
}

func newDecryptReader(r io.Reader, key []byte) (*decryptReader, error) {
	if len(key) != backupEncKeySize {
		return nil, fmt.Errorf("backup encryption key must be %d bytes, got %d", backupEncKeySize, len(key))
	}
	magic := make([]byte, len(backupEncMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("reading encryption header: %w", err)
	}
	if string(magic) != backupEncMagic {
		return nil, errors.New("not an encrypted observe backup (bad magic) — check OBSERVE_BACKUP_ENCRYPTION_KEY is set correctly and the archive is actually encrypted")
	}
	prefix := make([]byte, backupEncPrefixLen)
	if _, err := io.ReadFull(r, prefix); err != nil {
		return nil, fmt.Errorf("reading encryption nonce prefix: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &decryptReader{r: r, gcm: gcm, prefix: prefix}, nil
}

// maxEncChunkCiphertext bounds a single chunk's declared ciphertext length,
// so a corrupted or hostile length prefix can't trigger an unbounded
// allocation before the (later) authentication check would reject it anyway.
const maxEncChunkCiphertext = backupEncChunkSize + 64

func (d *decryptReader) fillChunk() error {
	if d.done {
		return io.EOF
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(d.r, lenBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("encrypted backup stream ended unexpectedly before its authenticated end marker (truncated, corrupted, or wrong key)")
		}
		return err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxEncChunkCiphertext {
		return fmt.Errorf("encrypted backup chunk length %d exceeds the maximum %d (corrupt stream or wrong key)", n, maxEncChunkCiphertext)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		return fmt.Errorf("reading encrypted chunk: %w", err)
	}

	// The nonce's final-chunk flag is not transmitted separately — it's
	// authenticated data we must guess before we can verify. Try "not final"
	// first (the common case), then "final". Trying both leaks nothing an
	// attacker doesn't already know (the flag is a single protocol-structure
	// bit, not a secret), and GCM's authentication failure is not vulnerable
	// to an oracle attack from a failed-then-retried verification.
	nonNonce, err := nonceFor(d.prefix, d.idx, false)
	if err != nil {
		return err
	}
	if plain, err := d.gcm.Open(nil, nonNonce, ct, nil); err == nil {
		d.idx++
		d.buf = append(d.buf, plain...)
		return nil
	}
	finalNonce, err := nonceFor(d.prefix, d.idx, true)
	if err != nil {
		return err
	}
	plain, err := d.gcm.Open(nil, finalNonce, ct, nil)
	if err != nil {
		return fmt.Errorf("decrypting backup chunk failed authentication (wrong key or corrupted archive)")
	}
	d.idx++
	d.buf = append(d.buf, plain...)
	d.done = true
	// A well-formed stream has nothing after the final chunk. Trailing bytes
	// mean the archive was tampered with or concatenated — reject rather than
	// silently ignore extra data.
	var extra [1]byte
	if n, _ := d.r.Read(extra[:]); n > 0 {
		return fmt.Errorf("encrypted backup stream has trailing data after its authenticated end marker")
	}
	return nil
}

func (d *decryptReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if err := d.fillChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}
