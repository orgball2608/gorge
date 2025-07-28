package gorge

import (
	"github.com/dgraph-io/ristretto"
	"io"
	"log/slog"
	"time"
)

const (
	defaultNamespace           = "gocache"
	defaultL1TTL               = 5 * time.Minute
	defaultInvalidationChannel = "gocache:invalidate"
)

// Options contains all configuration for the cache client.
type Options struct {
	Namespace                  string
	L1TTL                      time.Duration
	InvalidationChannel        string
	L1Config                   *ristretto.Config
	Serializer                 Serializer
	Logger                     *slog.Logger
	Metrics                    Metrics
	EnableStaleWhileRevalidate bool
	// If the remaining TTL of a key is less than this value, SWR will be triggered. Default is 1 minute.
	StaleTTL         time.Duration
	NegativeCacheTTL time.Duration
	ExpirationJitter float64

	// TTL for distributed lock. Must be long enough for the slowest fn() execution.
	LockTTL time.Duration
	// Wait time between failed lock acquisition attempts.
	LockSleep time.Duration
	// Number of retries for acquiring a distributed lock
	LockRetries int
	// Skip cache reading entirely, go directly to DB. Useful when Redis is failing.
	DisableCacheRead bool
	// Skip cache deletion entirely.
	DisableCacheDelete bool
	// Timeout for background refresh operations. Default is 10 seconds.
	RefreshTimeout time.Duration

	// Circuit Breaker settings
	EnableCircuitBreaker bool
	// Number of consecutive failures before opening the circuit.
	CircuitBreakerMaxFailures uint32
	// Period of time to wait before transitioning from open to half-open.
	CircuitBreakerTimeout time.Duration
}

// Option is a function to configure Options.
type Option func(*Options)

// NewDefaultOptions creates a default configuration.
func NewDefaultOptions() *Options {
	return &Options{
		Namespace:           defaultNamespace,
		L1TTL:               defaultL1TTL,
		InvalidationChannel: defaultInvalidationChannel,
		L1Config: &ristretto.Config{
			NumCounters: 1e7,
			MaxCost:     1 << 30,
			BufferItems: 64,
		},
		Serializer:                 JSONSerializer{},
		Logger:                     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:                    &noOpMetrics{},
		EnableStaleWhileRevalidate: false,
		StaleTTL:                   1 * time.Minute,
		NegativeCacheTTL:           1 * time.Minute,
		ExpirationJitter:           0.1,

		// Default values for new features
		LockTTL:            5 * time.Second,
		LockSleep:          100 * time.Millisecond,
		LockRetries:        5,
		DisableCacheRead:   false,
		DisableCacheDelete: false,
		RefreshTimeout:     10 * time.Second,

		// Default Circuit Breaker settings
		EnableCircuitBreaker:      true,
		CircuitBreakerMaxFailures: 5,
		CircuitBreakerTimeout:     5 * time.Second,
	}
}

// WithNamespace sets a prefix for all keys in Redis.
func WithNamespace(ns string) Option {
	return func(o *Options) { o.Namespace = ns }
}

// WithL1TTL sets the default TTL for L1 cache.
func WithL1TTL(ttl time.Duration) Option {
	return func(o *Options) { o.L1TTL = ttl }
}

// WithSerializer allows using a custom serializer (e.g., MsgPack).
func WithSerializer(s Serializer) Option {
	return func(o *Options) { o.Serializer = s }
}

// WithLogger allows integrating the application's logger.
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) { o.Logger = l }
}

// WithMetrics WithMetrics allows integrating a metrics system.
func WithMetrics(m Metrics) Option {
	return func(o *Options) { o.Metrics = m }
}

// WithNegativeCacheTTL Set TTL for negative cache ("not found" entries).
func WithNegativeCacheTTL(ttl time.Duration) Option {
	return func(o *Options) { o.NegativeCacheTTL = ttl }
}

// WithExpirationJitter Set expiration jitter ratio (0.0 to 1.0).
func WithExpirationJitter(jitter float64) Option {
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	return func(o *Options) { o.ExpirationJitter = jitter }
}

// WithStaleWhileRevalidate enables or disables SWR mode.
func WithStaleWhileRevalidate(enable bool) Option {
	return func(o *Options) { o.EnableStaleWhileRevalidate = enable }
}

// WithStaleTTL sets the TTL threshold to trigger SWR.
func WithStaleTTL(ttl time.Duration) Option {
	return func(o *Options) { o.StaleTTL = ttl }
}

// WithLockTTL sets the TTL for distributed lock.
func WithLockTTL(ttl time.Duration) Option {
	return func(o *Options) { o.LockTTL = ttl }
}

// WithLockSleep sets the wait time between lock acquisition attempts.
func WithLockSleep(sleep time.Duration) Option {
	return func(o *Options) { o.LockSleep = sleep }
}

// WithLockRetries sets the number of retries for lock acquisition
func WithLockRetries(retries int) Option {
	return func(o *Options) { o.LockRetries = retries }
}

// WithCacheReadDisabled enables or disables cache reading.
func WithCacheReadDisabled(disabled bool) Option {
	return func(o *Options) { o.DisableCacheRead = disabled }
}

// WithCacheDeleteDisabled enables or disables cache deletion.
func WithCacheDeleteDisabled(disabled bool) Option {
	return func(o *Options) { o.DisableCacheDelete = disabled }
}

// WithRefreshTimeout sets the timeout for background refresh operations.
func WithRefreshTimeout(timeout time.Duration) Option {
	return func(o *Options) { o.RefreshTimeout = timeout }
}

// WithCircuitBreaker enables the circuit breaker.
func WithCircuitBreaker(enable bool) Option {
	return func(o *Options) { o.EnableCircuitBreaker = enable }
}

// WithCircuitBreakerMaxFailures sets the number of consecutive failures before opening the circuit.
func WithCircuitBreakerMaxFailures(failures uint32) Option {
	return func(o *Options) { o.CircuitBreakerMaxFailures = failures }
}

// WithCircuitBreakerTimeout sets the period of time to wait before transitioning from open to half-open.
func WithCircuitBreakerTimeout(timeout time.Duration) Option {
	return func(o *Options) { o.CircuitBreakerTimeout = timeout }
}
