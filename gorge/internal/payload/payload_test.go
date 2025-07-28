package payload

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIsStale(t *testing.T) {
	staleTTL := 10 * time.Second

	testCases := []struct {
		name      string
		expiresAt time.Time
		expected  bool
	}{
		{
			name:      "Not stale, long TTL remaining",
			expiresAt: time.Now().Add(30 * time.Second),
			expected:  false,
		},
		{
			name:      "Stale, TTL within stale period",
			expiresAt: time.Now().Add(5 * time.Second),
			expected:  true,
		},
		{
			name:      "Not stale, TTL equals stale TTL",
			expiresAt: time.Now().Add(10 * time.Second),
			expected:  false,
		},
		{
			name:      "Expired, should be considered stale",
			expiresAt: time.Now().Add(-5 * time.Second),
			expected:  true,
		},
		{
			name:      "Stale check disabled, staleTTL is zero",
			expiresAt: time.Now().Add(5 * time.Second),
			expected:  false,
		},
		{
			name:      "Stale check disabled, staleTTL is negative",
			expiresAt: time.Now().Add(5 * time.Second),
			expected:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := CachePayload[string]{
				ExpiresAt: tc.expiresAt,
			}
			var StaleTTL time.Duration
			if tc.name == "Stale check disabled, staleTTL is zero" {
				StaleTTL = 0
			} else if tc.name == "Stale check disabled, staleTTL is negative" {
				StaleTTL = -1
			} else {
				StaleTTL = staleTTL
			}
			assert.Equal(t, tc.expected, p.IsStale(StaleTTL))
		})
	}
}
