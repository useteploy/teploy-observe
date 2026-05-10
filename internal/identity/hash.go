// Package identity centralizes the privacy-by-default hashing of
// user-supplied IDs (the SDK contract `identify(userId, traits?)`).
//
// The contract:
//   - Server takes the raw distinct_id from an SDK payload.
//   - Hashes it with the per-site session_salt using HMAC-SHA256.
//   - Stores the truncated 16-hex-char digest in events / error_events /
//     replay_sessions (matches the session-id length so persons-UI joins
//     line up with sessions-UI joins).
//
// Per-site opt-out: if the site setting raw_distinct_id is true, the
// raw value is returned unchanged. Operators who need server-side
// identity resolution (e.g. joining to a CRM by email) can flip the
// flag.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HashedIDLength is the length in characters of the hex digest stored on
// events. Matches the truncation used for session_id elsewhere in Observe
// so future joins between persons and sessions don't need a re-hash.
const HashedIDLength = 16

// HashDistinctID returns the privacy-stable digest of `raw` keyed by `salt`.
// An empty `raw` returns "" — the SDK call without identify() yields no
// user identifier and we never store an HMAC of empty.
func HashDistinctID(raw, salt string) string {
	if raw == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(raw))
	full := hex.EncodeToString(mac.Sum(nil))
	if len(full) <= HashedIDLength {
		return full
	}
	return full[:HashedIDLength]
}

// MaybeHashDistinctID applies HashDistinctID unless `rawDistinctIDOptIn`
// is true, in which case it returns `raw` as-is. The boolean is the
// per-site `raw_distinct_id` setting from the sites table.
func MaybeHashDistinctID(raw, salt string, rawDistinctIDOptIn bool) string {
	if rawDistinctIDOptIn {
		return raw
	}
	return HashDistinctID(raw, salt)
}
