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
	"github.com/orgball2608/gorge/internal/payload"
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

func TestGorge_CacheTagging(t *testing.T) {
	ctx := context.Background()
	ns := "test-tagging"
	g, err := New[string](rdb, WithNamespace(ns))
	assert.NoError(t, err)
	defer g.Close()

	fn := func(ctx context.Context) (string, error) {
		return "some-value", nil
	}

	// 1. Set keys with tags
	_, err = g.Fetch(ctx, "key1", time.Hour, fn, "tag1", "tag2")
	assert.NoError(t, err)
	_, err = g.Fetch(ctx, "key2", time.Hour, fn, "tag2")
	assert.NoError(t, err)
	_, err = g.Fetch(ctx, "key3", time.Hour, fn, "tag3")
	assert.NoError(t, err)

	// 2. Verify tags are set in Redis
	tag1Members, err := rdb.SMembers(ctx, g.tagKey("tag1")).Result()
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{g.prefixedKey("key1")}, tag1Members)

	tag2Members, err := rdb.SMembers(ctx, g.tagKey("tag2")).Result()
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{g.prefixedKey("key1"), g.prefixedKey("key2")}, tag2Members)

	// 3. Invalidate "tag2"
	err = g.InvalidateTags(ctx, "tag2")
	assert.NoError(t, err)

	// 4. Verify keys with "tag2" are deleted
	assert.Eventually(t, func() bool {
		exists, _ := rdb.Exists(ctx, g.prefixedKey("key1")).Result()
		return exists == 0
	}, time.Second, 50*time.Millisecond)

	assert.Eventually(t, func() bool {
		exists, _ := rdb.Exists(ctx, g.prefixedKey("key2")).Result()
		return exists == 0
	}, time.Second, 50*time.Millisecond)

	// 5. Verify key without "tag2" still exists
	exists, err := rdb.Exists(ctx, g.prefixedKey("key3")).Result()
	assert.NoError(t, err)
	assert.Equal(t, int64(1), exists, "key3 should still exist")

	// 6. Verify tag sets are deleted
	exists, err = rdb.Exists(ctx, g.tagKey("tag1")).Result()
	assert.NoError(t, err)
	assert.Equal(t, int64(1), exists, "tag1 should still exist") // key1 was part of it

	exists, err = rdb.Exists(ctx, g.tagKey("tag2")).Result()
	assert.NoError(t, err)
	assert.Equal(t, int64(0), exists, "tag2 should be deleted")
}

func TestGorge_Set(t *testing.T) {
	ctx := context.Background()
	ns := "test-set"
	key := "set-key"
	value1 := "value1"
	value2 := "value2"

	g1, err := New[string](rdb, WithNamespace(ns))
	assert.NoError(t, err)
	defer g1.Close()

	g2, err := New[string](rdb, WithNamespace(ns))
	assert.NoError(t, err)
	defer g2.Close()

	// Allow time for pubsub listeners to connect
	time.Sleep(100 * time.Millisecond)

	// 1. g1 sets a value
	err = g1.Set(ctx, key, value1, time.Hour)
	assert.NoError(t, err)

	// 2. Verify g1 has it in L1 eventually
	assert.Eventually(t, func() bool {
		v1, ok := g1.l1.Get(g1.prefixedKey(key))
		if !ok {
			return false
		}
		pl, ok_cast := v1.(payload.CachePayload[string])
		if !ok_cast {
			return false
		}
		return assert.ObjectsAreEqual(value1, pl.Data)
	}, time.Second, 10*time.Millisecond, "g1 should have the value in L1 after Set")

	// 3. Verify g2 can fetch it from L2
	val, err := g2.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		t.Fail() // Should not be called
		return "", nil
	})
	assert.NoError(t, err)
	assert.Equal(t, value1, val)

	// 4. Verify g2 now has it in L1
	_, ok := g2.l1.Get(g2.prefixedKey(key))
	assert.True(t, ok)

	// 5. g1 sets a new value for the same key
	err = g1.Set(ctx, key, value2, time.Hour)
	assert.NoError(t, err)

	// 6. Verify g2's L1 cache for that key is invalidated
	assert.Eventually(t, func() bool {
		_, ok := g2.l1.Get(g2.prefixedKey(key))
		return !ok
	}, time.Second, 50*time.Millisecond, "g2's L1 cache should be invalidated after Set")

	// 7. Verify fetching from g2 gets the new value
	val, err = g2.Fetch(ctx, key, time.Hour, func(ctx context.Context) (string, error) {
		t.Fail() // Should not be called
		return "", nil
	})
	assert.NoError(t, err)
	assert.Equal(t, value2, val)
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

	// Prime the L2 cache (L1 will also be primed)
	_, err := g.Fetch(ctx, key, 1*time.Hour, fn)
	if err != nil {
		b.Fatalf("failed to prime cache: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Stop the timer, perform setup for each iteration
		b.StopTimer()
		g.l1.Clear() // Clear L1 to ensure L2 is hit
		b.StartTimer()

		// Measure exactly one L2 hit
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
