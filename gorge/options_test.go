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
		assert.False(t, opts.EnableStaleWhileRevalidate)
		assert.Equal(t, 1*time.Minute, opts.StaleTTL)
		assert.Equal(t, 1*time.Minute, opts.NegativeCacheTTL)
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

		assert.Equal(t, "custom-ns", opts.Namespace)
		assert.Equal(t, 10*time.Minute, opts.L1TTL)
		assert.Equal(t, serializer, opts.Serializer)
		assert.Equal(t, logger, opts.Logger)
		assert.Equal(t, metrics, opts.Metrics)
		assert.Equal(t, 2*time.Minute, opts.NegativeCacheTTL)
		assert.True(t, opts.EnableStaleWhileRevalidate)
		assert.Equal(t, 30*time.Second, opts.StaleTTL)
	})
}
