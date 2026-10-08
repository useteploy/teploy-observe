package aiquery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const MaxQuestionBytes = 4000

var (
	ErrQuestion  = errors.New("question required and must be at most 4000 bytes")
	ErrPrincipal = errors.New("AI query requires an authenticated principal")
	ErrRateLimit = errors.New("AI query rate limit exceeded")
	ErrCapacity  = errors.New("AI query capacity is busy")
)

type principalKey struct{}

// WithPrincipal is set only by a transport after verifying its credential.
// Namespace principals (user: or mcp:) so independent credentials cannot collide.
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}
func ValidateQuestion(question string) error {
	if strings.TrimSpace(question) == "" || len(question) > MaxQuestionBytes {
		return ErrQuestion
	}
	return nil
}

// One process-wide budget across all Service instances and both transports.
var generationBudget = struct {
	limiter *principalLimiter
	slots   chan struct{}
}{&principalLimiter{buckets: make(map[string]principalBucket)}, make(chan struct{}, 4)}

func admit(ctx context.Context, question string) (func(), error) {
	if err := ValidateQuestion(question); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	principal, _ := ctx.Value(principalKey{}).(string)
	if strings.TrimSpace(principal) == "" {
		return nil, ErrPrincipal
	}
	if !generationBudget.limiter.allow(principal, time.Now()) {
		return nil, ErrRateLimit
	}
	select {
	case generationBudget.slots <- struct{}{}:
		return func() { <-generationBudget.slots }, nil
	default:
		return nil, ErrCapacity
	}
}

// Match the REST limiter's burst ten, whole-minute refill semantics without
// importing the ingestion service. Bound credential cardinality and expire idle
// buckets; no caller can allocate an unbounded rate map.
type principalBucket struct {
	tokens int
	filled time.Time
}
type principalLimiter struct {
	mu      sync.Mutex
	buckets map[string]principalBucket
}

func (l *principalLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= 4096 {
			for k, v := range l.buckets {
				if now.Sub(v.filled) > 5*time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= 4096 {
				return false
			}
		}
		b = principalBucket{10, now}
	} else if now.Sub(b.filled) >= time.Minute {
		b = principalBucket{10, now}
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	l.buckets[key] = b
	return true
}
