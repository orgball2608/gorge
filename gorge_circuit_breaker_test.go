package gorge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGorge_CircuitBreaker_OpensAndBypassesCache(t *testing.T) {
	ctx := context.Background()
	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&fnCalls, 1)
		return "from-db", nil
	}

	g, err := New[string](rdb,
		WithNamespace("cb-opens-test"),
		WithCircuitBreaker(true),
		WithCircuitBreakerMaxFailures(2),
		WithCircuitBreakerTimeout(10*time.Second), // Long timeout to prevent half-open state during test
	)
	assert.NoError(t, err)
	defer g.Close()

	// 1. Manually cause two failures to trip the breaker
	failingOp := func() (interface{}, error) {
		return nil, errors.New("redis is down")
	}
	_, err = g.executeL2(failingOp)
	assert.Error(t, err)
	_, err = g.executeL2(failingOp)
	assert.Error(t, err)

	// 2. Breaker should now be open.
	// The next Fetch call should bypass Redis entirely and call the function directly.
	val, err := g.Fetch(ctx, "key1", time.Hour, fn)
	assert.NoError(t, err, "Fetch should succeed by calling fn directly when breaker is open")
	assert.Equal(t, "from-db", val)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "DB func should be called when breaker is open")

	// 3. A second fetch should also go to the DB, as the result is not cached
	val, err = g.Fetch(ctx, "key2", time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, "from-db", val)
	assert.Equal(t, int32(2), atomic.LoadInt32(&fnCalls), "DB func should be called again")
}

func TestGorge_CircuitBreakerDisabled(t *testing.T) {
	g, err := New[string](rdb,
		WithNamespace("cb-disabled"),
		WithCircuitBreaker(false),
	)
	assert.NoError(t, err)
	defer g.Close()

	// This just tests that the executeL2 function doesn't panic when the breaker is nil
	_, err = g.executeL2(func() (interface{}, error) {
		return nil, errors.New("some error")
	})
	assert.Error(t, err, "some error")
}
