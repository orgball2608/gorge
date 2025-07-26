package payload

import "time"

// CachePayload is the data structure stored in the cache.
type CachePayload[T any] struct {
	Data      T
	IsNil     bool      // Used for negative caching
	ExpiresAt time.Time // Actual expiration time of the L2 entry
}

// IsStale checks if the payload is within its stale period, defined by the StaleTTL option.
func (p *CachePayload[T]) IsStale(staleTTL time.Duration) bool {
	// If SWR is disabled (staleTTL <= 0) or data is already expired, it's not considered stale.
	if staleTTL <= 0 || time.Now().After(p.ExpiresAt) {
		return false
	}
	// It's stale if we are past the point in time: (ExpiresAt - StaleTTL)
	return time.Now().After(p.ExpiresAt.Add(-staleTTL))
}
