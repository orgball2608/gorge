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
		{"NormalJitter", 0.1, 0.1},
		{"ZeroJitter", 0.0, 0.0},
		{"MaxJitter", 1.0, 1.0},
		{"NegativeJitter", -0.5, 0.0},
		{"LargeJitter", 1.5, 1.0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			opts := NewDefaultOptions()
			WithExpirationJitter(tc.input)(opts)
			assert.Equal(t, tc.expected, opts.ExpirationJitter)
		})
	}
}

func TestOptions_validate(t *testing.T) {
	t.Run("valid options", func(t *testing.T) {
		opts := NewDefaultOptions()
		assert.NoError(t, opts.validate())
	})

	t.Run("invalid options", func(t *testing.T) {
		testCases := []struct {
			name    string
			optFunc func(o *Options)
			errMsg  string
		}{
			{
				name: "StaleTTL >= LockTTL",
				optFunc: func(o *Options) {
					o.EnableStaleWhileRevalidate = true
					o.StaleTTL = 10 * time.Second
					o.LockTTL = 5 * time.Second
				},
				errMsg: "StaleTTL must be smaller than LockTTL",
			},
			{
				name:    "Negative RefreshTimeout",
				optFunc: func(o *Options) { o.RefreshTimeout = -1 },
				errMsg:  "RefreshTimeout must be positive",
			},
			{
				name:    "Negative L1TTL",
				optFunc: func(o *Options) { o.L1TTL = 0 },
				errMsg:  "L1TTL must be positive",
			},
			{
				name:    "Negative LockTTL",
				optFunc: func(o *Options) { o.LockTTL = 0 },
				errMsg:  "LockTTL must be positive",
			},
			{
				name:    "Negative Jitter",
				optFunc: func(o *Options) { o.ExpirationJitter = -0.1 },
				errMsg:  "ExpirationJitter must be between 0.0 and 1.0",
			},
			{
				name:    "Invalid CircuitBreakerMaxFailures",
				optFunc: func(o *Options) { o.EnableCircuitBreaker = true; o.CircuitBreakerMaxFailures = 0 },
				errMsg:  "CircuitBreakerMaxFailures must be positive",
			},
			{
				name:    "Invalid CircuitBreakerTimeout",
				optFunc: func(o *Options) { o.EnableCircuitBreaker = true; o.CircuitBreakerTimeout = 0 },
				errMsg:  "CircuitBreakerTimeout must be positive",
			},
			{
				name:    "Invalid NegativeCacheTTL",
				optFunc: func(o *Options) { o.NegativeCacheTTL = 0 },
				errMsg:  "NegativeCacheTTL must be positive",
			},
			{
				name:    "Invalid StaleTTL",
				optFunc: func(o *Options) { o.StaleTTL = 0 },
				errMsg:  "StaleTTL must be positive",
			},
			{
				name:    "Invalid LockSleep",
				optFunc: func(o *Options) { o.LockSleep = 0 },
				errMsg:  "LockSleep must be positive",
			},
			{
				name:    "Invalid LockRetries",
				optFunc: func(o *Options) { o.LockRetries = -1 },
				errMsg:  "LockRetries must be non-negative",
			},
			{
				name:    "Jitter > 1",
				optFunc: func(o *Options) { o.ExpirationJitter = 1.1 },
				errMsg:  "ExpirationJitter must be between 0.0 and 1.0",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				opts := NewDefaultOptions()
				tc.optFunc(opts)
				err := opts.validate()
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMsg)
			})
		}
	})
}
