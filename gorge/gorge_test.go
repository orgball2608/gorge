package gorge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	rdb *redis.Client
)

// mockSerializer is used to test serialization errors.
type mockSerializer struct{}

func (s mockSerializer) Marshal(v interface{}) ([]byte, error) {
	return nil, errors.New("mock marshal error")
}

func (s mockSerializer) Unmarshal(data []byte, v interface{}) error {
	return errors.New("mock unmarshal error")
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	redisContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	if err != nil {
		log.Fatalf("could not start redis container: %s", err)
	}

	defer func() {
		if err := redisContainer.Terminate(ctx); err != nil {
			log.Fatalf("could not stop redis container: %s", err)
		}
	}()

	endpoint, err := redisContainer.Endpoint(ctx, "")
	if err != nil {
		log.Fatalf("could not get redis endpoint: %s", err)
	}

	rdb = redis.NewClient(&redis.Options{
		Addr: endpoint,
	})

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("could not connect to redis: %s", err)
	}

	os.Exit(m.Run())
}

func TestGorge_Fetch_CacheBehavior(t *testing.T) {
	ctx := context.Background()
	key := "my-key"
	value := "my-value"

	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&fnCalls, 1)
		return value, nil
	}

	g, err := New[string](rdb, WithNamespace("test-prefix"))
	assert.NoError(t, err)
	defer g.Close()

	// 1. First call: L1 miss, L2 miss, DB hit
	v, err := g.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, value, v)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "fn should be called once for DB hit")

	// 2. Second call: L1 hit
	v, err = g.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, value, v)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "fn should not be called for L1 hit")

	// 3. Third call after clearing L1: L2 hit
	g.l1.Clear()
	v, err = g.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, value, v)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "fn should not be called for L2 hit")
}

func TestGorge_Fetch_NegativeCaching(t *testing.T) {
	ctx := context.Background()
	key := "not-found-key"

	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&fnCalls, 1)
		return "", ErrNotFound
	}

	g, err := New[string](rdb,
		WithNamespace("test-prefix"),
		WithNegativeCacheTTL(1*time.Hour),
	)
	assert.NoError(t, err)
	defer g.Close()

	// 1. First call: DB returns error
	_, err = g.Fetch(ctx, key, 1*time.Hour, fn)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "fn should be called once")

	// 2. Second call: Negative cache hit
	_, err = g.Fetch(ctx, key, 1*time.Hour, fn)
	assert.ErrorIs(t, err, ErrNotFound, "error should be ErrNotFound for negative cache hit")
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "fn should not be called for negative cache hit")
}

func TestGorge_Fetch_ThunderingHerd(t *testing.T) {
	ctx := context.Background()
	key := "herd-key"
	value := "herd-value"

	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		// Simulate work
		time.Sleep(100 * time.Millisecond)
		atomic.AddInt32(&fnCalls, 1)
		return value, nil
	}

	g, err := New[string](rdb, WithNamespace("test-prefix"))
	assert.NoError(t, err)
	defer g.Close()

	// Clear key to ensure a miss
	err = g.Delete(ctx, key)
	assert.NoError(t, err)
	_, err = rdb.Del(ctx, g.prefixedKey(key)).Result()
	assert.NoError(t, err)

	var wg sync.WaitGroup
	numGoroutines := 20

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := g.Fetch(ctx, key, 1*time.Hour, fn)
			assert.NoError(t, err)
			assert.Equal(t, value, v)
		}()
	}

	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "fn should only be called once during a thundering herd")
}

func TestGorge_Fetch_StaleWhileRevalidate(t *testing.T) {
	ctx := context.Background()
	key := "stale-key"
	firstValue := "first-value"
	secondValue := "second-value"

	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		callNum := atomic.AddInt32(&fnCalls, 1)
		if callNum == 1 {
			return firstValue, nil
		}
		return secondValue, nil
	}

	mainTTL := 5 * time.Second
	staleTTL := 3 * time.Second // Key will be stale in its last 3 seconds of life

	g, err := New[string](rdb,
		WithNamespace("test-swr"),
		WithStaleWhileRevalidate(true),
		WithStaleTTL(staleTTL),
	)
	assert.NoError(t, err)
	defer g.Close()

	// 1. Prime the cache
	v, err := g.Fetch(ctx, key, mainTTL, fn)
	assert.NoError(t, err)
	assert.Equal(t, firstValue, v)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls))

	// 2. Wait until data is stale but not expired.
	time.Sleep(3 * time.Second)

	// 3. Second call: should return stale value and trigger background refresh
	start := time.Now()
	v, err = g.Fetch(ctx, key, mainTTL, fn)
	assert.NoError(t, err)
	assert.Less(t, time.Since(start), 20*time.Millisecond, "should return instantly from cache")
	assert.Equal(t, firstValue, v, "should return stale value")

	// Give the background refresh time to complete
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, int32(2), atomic.LoadInt32(&fnCalls), "fn should have been called again in the background")

	// 4. Third call: Should now get the fresh value from L1
	v, err = g.Fetch(ctx, key, mainTTL, fn)
	assert.NoError(t, err)
	assert.Equal(t, secondValue, v, "should return the new value from L1")
}

func TestGorge_GracefulDegradation(t *testing.T) {
	ctx := context.Background()
	key := "degradation-key"
	value := "degradation-value"

	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&fnCalls, 1)
		return value, nil
	}

	g, err := New[string](rdb,
		WithNamespace("test-degradation"),
		WithCacheReadDisabled(true),
	)
	assert.NoError(t, err)
	defer g.Close()

	_, err = g.Fetch(ctx, key, time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls))

	_, err = g.Fetch(ctx, key, time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&fnCalls), "fn should be called again as cache is disabled")
}

func TestGorge_Delete_PubSub(t *testing.T) {
	ctx := context.Background()
	key := "pubsub-key"
	value := "pubsub-value"
	prefix := "pubsub-test"

	fn := func(ctx context.Context) (string, error) {
		return value, nil
	}

	g1, err := New[string](rdb, WithNamespace(prefix))
	assert.NoError(t, err)
	defer g1.Close()

	g2, err := New[string](rdb, WithNamespace(prefix))
	assert.NoError(t, err)
	defer g2.Close()

	_, err = g1.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)
	_, err = g2.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)

	assert.Eventually(t, func() bool {
		_, ok := g1.l1.Get(g1.prefixedKey(key))
		return ok
	}, 100*time.Millisecond, 10*time.Millisecond, "g1 L1 cache should have the key")
	assert.Eventually(t, func() bool {
		_, ok := g2.l1.Get(g2.prefixedKey(key))
		return ok
	}, 100*time.Millisecond, 10*time.Millisecond, "g2 L1 cache should have the key")

	err = g1.Delete(ctx, key)
	assert.NoError(t, err)

	assert.Eventually(t, func() bool {
		_, ok := g1.l1.Get(g1.prefixedKey(key))
		return !ok
	}, 100*time.Millisecond, 10*time.Millisecond, "g1 L1 cache should be deleted immediately")

	assert.Eventually(t, func() bool {
		_, ok := g2.l1.Get(g2.prefixedKey(key))
		return !ok
	}, 200*time.Millisecond, 20*time.Millisecond, "g2 L1 cache should be deleted after pub/sub propagation")
}

func TestNew_RistrettoError(t *testing.T) {
	badL1Config := &ristretto.Config{
		NumCounters: 0,
		MaxCost:     1 << 30,
		BufferItems: 64,
	}
	opts := NewDefaultOptions()
	opts.L1Config = badL1Config

	_, err := New[string](rdb, func(o *Options) {
		o.L1Config = badL1Config
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create L1 cache")
}

func TestFetch_LockRetriesExhausted(t *testing.T) {
	ctx := context.Background()
	key := "locked-key"

	g, err := New[string](rdb,
		WithNamespace("test-lock-exhausted"),
		WithLockRetries(2),
		WithLockSleep(10*time.Millisecond),
		WithLockTTL(1*time.Second), // Use WithLockTTL
	)
	assert.NoError(t, err)
	defer g.Close()

	prefixedKey := g.prefixedKey(key)
	rdb.HSet(ctx, prefixedKey, "lockOwner", "another-owner")
	rdb.Expire(ctx, prefixedKey, 10*time.Second)

	_, err = g.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		t.FailNow()
		return "", nil
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to acquire lock")

	rdb.Del(ctx, prefixedKey)
}

func TestHandleL2Hit_UnmarshalError(t *testing.T) {
	ctx := context.Background()
	key := "corrupted-key"
	var fnCalls int32

	g, err := New[string](rdb, WithNamespace("test-unmarshal-error"))
	assert.NoError(t, err)
	defer g.Close()

	// Manually set a value in L2 that is a valid payload, but whose data
	// cannot be unmarshalled into the target type (string).
	prefixedKey := g.prefixedKey(key)
	badPayload, _ := JSONSerializer{}.Marshal(map[string]interface{}{"data": 12345, "expiresAt": time.Now().Add(time.Hour)})
	rdb.HSet(ctx, prefixedKey, "value", badPayload)
	rdb.Expire(ctx, prefixedKey, time.Hour)

	// Fetch should fail to unmarshal, and then call the DB function
	val, err := g.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		atomic.AddInt32(&fnCalls, 1)
		return "good-value", nil
	})

	assert.NoError(t, err)
	assert.Equal(t, "good-value", val)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls), "DB function should be called after unmarshal error")
}

func TestDelete_Disabled(t *testing.T) {
	ctx := context.Background()
	key := "no-delete-key"

	g, err := New[string](rdb,
		WithNamespace("test-delete-disabled"),
		WithCacheDeleteDisabled(true),
	)
	assert.NoError(t, err)
	defer g.Close()

	_, err = g.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		return "some-value", nil
	})
	assert.NoError(t, err)

	err = g.Delete(ctx, key)
	assert.NoError(t, err)

	prefixedKey := g.prefixedKey(key)
	res := rdb.Exists(ctx, prefixedKey).Val()
	assert.Equal(t, int64(1), res, "Key should still exist in Redis after disabled delete call")
}

func TestFetch_DBError(t *testing.T) {
	ctx := context.Background()
	key := "db-error-key"
	dbErr := errors.New("database connection failed")

	g, err := New[string](rdb, WithNamespace("test-db-error"))
	assert.NoError(t, err)
	defer g.Close()

	_, err = g.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		return "", dbErr
	})

	assert.ErrorIs(t, err, dbErr)

	// Check that the lock was released
	prefixedKey := g.prefixedKey(key)
	lockOwner := rdb.HGet(ctx, prefixedKey, "lockOwner").Val()
	assert.Empty(t, lockOwner, "Lock should be released on DB error")
}

func TestSetCache_MarshalError(t *testing.T) {
	ctx := context.Background()
	key := "marshal-error-key"

	g, err := New[string](rdb,
		WithNamespace("test-marshal-error"),
		WithSerializer(mockSerializer{}),
	)
	assert.NoError(t, err)
	defer g.Close()

	_, err = g.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		return "some-data", nil
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mock marshal error")

	// Check that the lock was released
	prefixedKey := g.prefixedKey(key)
	lockOwner := rdb.HGet(ctx, prefixedKey, "lockOwner").Val()
	assert.Empty(t, lockOwner, "Lock should be released on marshal error")
}

func TestGorge_DisabledCaches(t *testing.T) {
	ctx := context.Background()
	key := "disabled-key"
	value := "disabled-value"
	ns := "test-disabled"

	var fnCalls int32
	fn := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&fnCalls, 1)
		return value, nil
	}

	t.Run("CacheReadDisabled", func(t *testing.T) {
		atomic.StoreInt32(&fnCalls, 0)
		g, err := New[string](rdb, WithNamespace(ns), WithCacheReadDisabled(true))
		assert.NoError(t, err)
		defer g.Close()

		// Fetch twice, DB func should be called twice
		_, err = g.Fetch(ctx, key, time.Hour, fn)
		assert.NoError(t, err)
		_, err = g.Fetch(ctx, key, time.Hour, fn)
		assert.NoError(t, err)
		assert.Equal(t, int32(2), atomic.LoadInt32(&fnCalls))

		// Verify the key does not exist in L2
		exists, err := rdb.Exists(ctx, g.prefixedKey(key)).Result()
		assert.NoError(t, err)
		assert.Equal(t, int64(0), exists, "Key should not be in L2 when cache read is disabled")
	})

	t.Run("CacheDeleteDisabled", func(t *testing.T) {
		atomic.StoreInt32(&fnCalls, 0)
		g, err := New[string](rdb, WithNamespace(ns), WithCacheDeleteDisabled(true))
		assert.NoError(t, err)
		defer g.Close()

		// Prime the cache
		_, err = g.Fetch(ctx, key, time.Hour, fn)
		assert.NoError(t, err)

		// Delete should be a no-op
		err = g.Delete(ctx, key)
		assert.NoError(t, err)

		// Verify the key still exists in L2
		exists, err := rdb.Exists(ctx, g.prefixedKey(key)).Result()
		assert.NoError(t, err)
		assert.Equal(t, int64(1), exists, "Key should not be deleted from L2 when delete is disabled")
	})
}

func BenchmarkFetch_L1Hit(b *testing.B) {
	ctx := context.Background()
	g, _ := New[string](rdb, WithNamespace("bench-l1-hit"))
	defer g.Close()

	key := "my-key"
	value := "my-value"
	fn := func(ctx context.Context) (string, error) { return value, nil }

	_, err := g.Fetch(ctx, key, 1*time.Hour, fn)
	if err != nil {
		b.Fatalf("failed to prime cache: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _ = g.Fetch(ctx, key, 1*time.Hour, fn)
	}
}

func BenchmarkFetch_L2Hit(b *testing.B) {
	ctx := context.Background()
	g, _ := New[string](rdb, WithNamespace("bench-l2-hit"))
	defer g.Close()

	key := "my-key"
	value := "my-value"
	fn := func(ctx context.Context) (string, error) { return value, nil }

	_, err := g.Fetch(ctx, key, 1*time.Hour, fn)
	if err != nil {
		b.Fatalf("failed to prime cache: %v", err)
	}
	g.l1.Clear()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _ = g.Fetch(ctx, key, 1*time.Hour, fn)
	}
}

func BenchmarkFetch_DBHit(b *testing.B) {
	ctx := context.Background()
	g, _ := New[string](rdb, WithNamespace("bench-db-hit"))
	defer g.Close()

	key := "my-key"
	value := "my-value"
	fn := func(ctx context.Context) (string, error) { return value, nil }

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _ = g.Fetch(ctx, fmt.Sprintf("%s-%d", key, i), 1*time.Hour, fn)
	}
}
