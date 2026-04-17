package jobs

import "crypto/rand"

// cryptoRead fills b with random bytes from crypto/rand. Wrapper exists
// so callers can be tested with a deterministic reader if ever needed.
func cryptoRead(b []byte) (int, error) {
	return rand.Read(b)
}
