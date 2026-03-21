package session

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// ID generates a deterministic, cookie-free session ID.
// Uses SHA-256 of (siteID + IP + userAgent + monthlySalt) truncated to UUID format.
// This mirrors Umami's approach: same visitor within the same month always gets
// the same session ID without needing cookies.
func ID(siteID, ip, userAgent, salt string) string {
	monthKey := time.Now().UTC().Format("2006-01")
	input := siteID + ip + userAgent + salt + monthKey
	hash := sha256.Sum256([]byte(input))
	// Format as UUID v5-style string
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

// VisitID generates a visit identifier that rotates after 30 minutes of
// inactivity or at hourly boundaries. A session can contain multiple visits.
func VisitID(sessionID string, ts time.Time) string {
	hourBucket := ts.Truncate(time.Hour).Unix()
	input := fmt.Sprintf("%s:%d", sessionID, hourBucket)
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}
