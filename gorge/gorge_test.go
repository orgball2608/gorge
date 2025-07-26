package gorge

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	rdb *redis.Client
)

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

	// **SỬA LỖI: Logic test được làm rõ ràng và chính xác**
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
	// Stale period starts at T+2s (5s - 3s). We wait 3s to be safely inside it.
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

	// Create a cache instance with cache reads disabled
	g, err := New[string](rdb,
		WithNamespace("test-degradation"),
		WithCacheReadDisabled(true),
	)
	assert.NoError(t, err)
	defer g.Close()

	// 1. First call, should go to DB
	_, err = g.Fetch(ctx, key, time.Hour, fn)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fnCalls))

	// 2. Second call, should also go to DB because cache is disabled
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

	// Create two gorge instances to simulate two different nodes
	g1, err := New[string](rdb, WithNamespace(prefix))
	assert.NoError(t, err)
	defer g1.Close()

	g2, err := New[string](rdb, WithNamespace(prefix))
	assert.NoError(t, err)
	defer g2.Close()

	// 1. Populate cache on both nodes
	_, err = g1.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)
	_, err = g2.Fetch(ctx, key, 1*time.Hour, fn)
	assert.NoError(t, err)

	// Verify L1 cache exists on both, accounting for eventual consistency
	assert.Eventually(t, func() bool {
		_, ok := g1.l1.Get(g1.prefixedKey(key))
		return ok
	}, 100*time.Millisecond, 10*time.Millisecond, "g1 L1 cache should have the key")
	assert.Eventually(t, func() bool {
		_, ok := g2.l1.Get(g2.prefixedKey(key))
		return ok
	}, 100*time.Millisecond, 10*time.Millisecond, "g2 L1 cache should have the key")

	// 2. g1 deletes the key
	err = g1.Delete(ctx, key)
	assert.NoError(t, err)

	// 3. Verify L1 is deleted on g1 immediately (should be fast due to local call)
	assert.Eventually(t, func() bool {
		_, ok := g1.l1.Get(g1.prefixedKey(key))
		return !ok
	}, 100*time.Millisecond, 10*time.Millisecond, "g1 L1 cache should be deleted immediately")

	// 4. Wait for pub/sub message to be processed by g2
	assert.Eventually(t, func() bool {
		_, ok := g2.l1.Get(g2.prefixedKey(key))
		return !ok
	}, 200*time.Millisecond, 20*time.Millisecond, "g2 L1 cache should be deleted after pub/sub propagation")
}

func BenchmarkFetch_L1Hit(b *testing.B) {
	ctx := context.Background()
	g, _ := New[string](rdb, WithNamespace("bench-l1-hit"))
	defer g.Close()

	key := "my-key"
	value := "my-value"
	fn := func(ctx context.Context) (string, error) { return value, nil }

	// Prime the cache
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

	// Prime the L2 cache and clear L1
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
		// Use a different key for each iteration to ensure a DB hit
		_, _ = g.Fetch(ctx, fmt.Sprintf("%s-%d", key, i), 1*time.Hour, fn)
	}
}
