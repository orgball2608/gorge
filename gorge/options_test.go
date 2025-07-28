package gorge

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestOptions(t *testing.T) {
	t.Run("default options", func(t *testing.T) {
		opts := NewDefaultOptions()

		assert.Equal(t, "gocache", opts.Namespace)
		assert.Equal(t, 5*time.Minute, opts.L1TTL)
		assert.NotNil(t, opts.L1Config)
		assert.IsType(t, JSONSerializer{}, opts.Serializer)
		assert.NotNil(t, opts.Logger)
		assert.IsType(t, &noOpMetrics{}, opts.Metrics)

		// Call methods to improve coverage
		opts.Metrics.IncL1Hits()
		opts.Metrics.IncL1Misses()
		opts.Metrics.IncL2Hits()
		opts.Metrics.IncL2Misses()
		opts.Metrics.IncDBFetches()
		opts.Metrics.IncDBErrors()

		assert.False(t, opts.EnableStaleWhileRevalidate)
		assert.Equal(t, 1*time.Minute, opts.StaleTTL)
		assert.Equal(t, 1*time.Minute, opts.NegativeCacheTTL)
		assert.Equal(t, 10*time.Second, opts.RefreshTimeout)
	})

	t.Run("with custom options", func(t *testing.T) {
		logger := slog.Default()
		metrics := &noOpMetrics{}
		serializer := JSONSerializer{}

		opts := NewDefaultOptions()
		WithNamespace("custom-ns")(opts)
		WithL1TTL(10 * time.Minute)(opts)
		WithSerializer(serializer)(opts)
		WithLogger(logger)(opts)
		WithMetrics(metrics)(opts)
		WithNegativeCacheTTL(2 * time.Minute)(opts)
		WithStaleWhileRevalidate(true)(opts)
		WithStaleTTL(30 * time.Second)(opts)
		WithRefreshTimeout(5 * time.Second)(opts)

		assert.Equal(t, "custom-ns", opts.Namespace)
		assert.Equal(t, 10*time.Minute, opts.L1TTL)
		assert.Equal(t, serializer, opts.Serializer)
		assert.Equal(t, logger, opts.Logger)
		assert.Equal(t, metrics, opts.Metrics)
		assert.Equal(t, 2*time.Minute, opts.NegativeCacheTTL)
		assert.True(t, opts.EnableStaleWhileRevalidate)
		assert.Equal(t, 30*time.Second, opts.StaleTTL)
		assert.Equal(t, 5*time.Second, opts.RefreshTimeout)
	})
}

func TestWithExpirationJitter(t *testing.T) {
	testCases := []struct {
		name     string
		input    float64
		expected float64
	}{
		{
			name:     "Jitter within range",
			input:    0.5,
			expected: 0.5,
		},
		{
			name:     "Jitter less than 0",
			input:    -0.1,
			expected: 0,
		},
		{
			name:     "Jitter greater than 1",
			input:    1.5,
			expected: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			opts := NewDefaultOptions()
			WithExpirationJitter(tc.input)(opts)
			assert.Equal(t, tc.expected, opts.ExpirationJitter)
		})
	}
}
