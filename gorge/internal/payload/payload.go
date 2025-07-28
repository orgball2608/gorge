package payload

import (
	"math"
	"time"
)

// CachePayload is the data structure stored in the cache.
type CachePayload[T any] struct {
	Data      T
	IsNil     bool      // Used for negative caching
	ExpiresAt time.Time // Actual expiration time of the L2 entry
}

// IsStale checks if the payload is within its stale period, defined by the StaleTTL option.
func (p *CachePayload[T]) IsStale(staleTTL time.Duration) bool {
	if staleTTL <= 0 {
		return false
	}
	// An item is stale if its remaining TTL is less than the configured staleTTL.
	// This also correctly handles already-expired items (where remaining TTL is negative).
	return math.Ceil(time.Until(p.ExpiresAt).Seconds()) < staleTTL.Seconds()
}
