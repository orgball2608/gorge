package gorge

import (
	"context"
	"errors"
	"fmt"
	"github.com/orgball2608/gorge/gorge/internal/payload"
	"math/rand"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// ErrNotFound is a special error indicating that data was not found in the primary source (DB).
var ErrNotFound = errors.New("gocache: resource not found")

// Cache is a two-layer, type-safe cache client ready for production.
type Cache[T any] struct {
	opts               *Options
	l1                 *ristretto.Cache
	l2                 redis.UniversalClient
	sf                 singleflight.Group
	ctx                context.Context
	cancel             context.CancelFunc
	rand               *rand.Rand
	ownerID            string
	lockAndGetScript   *redis.Script
	setAndUnlockScript *redis.Script
}

// New creates a new Cache instance.
func New[T any](redisClient redis.UniversalClient, opts ...Option) (*Cache[T], error) {
	options := NewDefaultOptions()
	for _, opt := range opts {
		opt(options)
	}

	l1Cache, err := ristretto.NewCache(options.L1Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create L1 cache: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &Cache[T]{
		opts:               options,
		l1:                 l1Cache,
		l2:                 redisClient,
		ctx:                ctx,
		cancel:             cancel,
		rand:               rand.New(rand.NewSource(time.Now().UnixNano())),
		ownerID:            fmt.Sprintf("gocache-owner-%d-%d", time.Now().UnixNano(), rand.Intn(1000)),
		lockAndGetScript:   redis.NewScript(tryLockAndGetScript),
		setAndUnlockScript: redis.NewScript(setDataAndUnlockScript),
	}

	go c.listenForInvalidations()
	c.opts.Logger.Info("GoCache instance created and listener started", "namespace", c.opts.Namespace, "ownerID", c.ownerID)
	return c, nil
}

// Fetch is the main function, the "entry point" of the library.
func (c *Cache[T]) Fetch(ctx context.Context, key string, ttl time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T

	if c.opts.DisableCacheRead {
		c.opts.Logger.Warn("Cache read is disabled. Fetching directly from DB.", "key", key)
		c.opts.Metrics.IncDBFetches()
		return fn(ctx)
	}

	prefixedKey := c.prefixedKey(key)

	// Single-flight protects the entire logic to prevent stampede within the same instance.
	res, err, _ := c.sf.Do(prefixedKey, func() (interface{}, error) {
		// 1. Try to get from L1 Cache
		if val, found := c.l1.Get(prefixedKey); found {
			c.opts.Metrics.IncL1Hits()
			c.opts.Logger.Debug("L1 cache hit", "key", prefixedKey)
			pl := val.(payload.CachePayload[T])
			if pl.IsNil {
				return nil, ErrNotFound
			}
			return pl, nil
		}
		c.opts.Metrics.IncL1Misses()

		// Loop to try acquiring lock or getting data from L2
		for i := 0; i < c.opts.LockRetries; i++ {
			lockTTLInSeconds := int(c.opts.LockTTL.Seconds())
			res, err := c.lockAndGetScript.Run(ctx, c.l2, []string{prefixedKey}, c.ownerID, lockTTLInSeconds).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				c.opts.Logger.Error("Failed to run lockAndGet script", "key", prefixedKey, "error", err)
				return nil, err
			}

			results := res.([]interface{})
			value, status := results[0], results[1].(string)

			switch status {
			case "HIT":
				c.opts.Metrics.IncL2Hits()
				c.opts.Logger.Debug("L2 cache hit", "key", prefixedKey)
				return c.handleL2Hit(ctx, prefixedKey, value.(string), ttl, fn)

			case "ACQUIRED_LOCK":
				c.opts.Logger.Debug("Acquired distributed lock", "key", prefixedKey, "owner", c.ownerID)
				return c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn)

			case "LOCKED_BY_OTHER":
				c.opts.Logger.Debug("Key is locked by another instance. Waiting...", "key", prefixedKey)
				time.Sleep(c.opts.LockSleep)
				continue
			}
		}

		return nil, fmt.Errorf("failed to acquire lock for key '%s' after %d retries", key, c.opts.LockRetries)
	})

	if err != nil {
		return zero, err
	}
	pl := res.(payload.CachePayload[T])
	if pl.IsNil {
		return zero, ErrNotFound
	}
	return pl.Data, nil
}

// handleL2Hit processes when L2 cache has data
func (c *Cache[T]) handleL2Hit(ctx context.Context, prefixedKey, l2Val string, ttl time.Duration, fn func(ctx context.Context) (T, error)) (interface{}, error) {
	var pl payload.CachePayload[T]
	if err := c.opts.Serializer.Unmarshal([]byte(l2Val), &pl); err != nil {
		c.opts.Logger.Error("Failed to unmarshal L2 data, re-fetching from DB", "key", prefixedKey, "error", err)
		return c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn)
	}

	c.l1.SetWithTTL(prefixedKey, pl, 1, c.opts.L1TTL)

	// Stale-While-Revalidate logic
	keyTTL, ttlErr := c.l2.TTL(ctx, prefixedKey).Result()
	if ttlErr == nil && c.opts.EnableStaleWhileRevalidate && keyTTL < c.opts.StaleTTL {
		c.opts.Logger.Debug("Returning stale data and refreshing in background", "key", prefixedKey, "remainingTTL", keyTTL)
		go c.refresh(context.Background(), prefixedKey, ttl, fn)
	}

	return pl, nil
}

// fetchFromDBAndSet is called when a lock is acquired, responsible for calling DB and setting cache.
func (c *Cache[T]) fetchFromDBAndSet(ctx context.Context, prefixedKey string, ttl time.Duration, fn func(ctx context.Context) (T, error)) (interface{}, error) {
	c.opts.Metrics.IncDBFetches()
	dbVal, err := fn(ctx)
	if err != nil {
		c.opts.Metrics.IncDBErrors()
		if errors.Is(err, ErrNotFound) {
			// Negative Caching
			pl := payload.CachePayload[T]{IsNil: true}
			c.setCacheAndUnlock(ctx, prefixedKey, pl, c.opts.NegativeCacheTTL)
		} else {
			c.l2.HDel(ctx, prefixedKey, "lockOwner")
		}
		return nil, err
	}

	pl := payload.CachePayload[T]{Data: dbVal, IsNil: false}
	c.setCacheAndUnlock(ctx, prefixedKey, pl, ttl)
	return pl, nil
}

// setCacheAndUnlock uses Lua to atomically write data and release the lock.
func (c *Cache[T]) setCacheAndUnlock(ctx context.Context, key string, payload payload.CachePayload[T], ttl time.Duration) {
	// Jitter
	if c.opts.ExpirationJitter > 0 {
		jitter := time.Duration(c.opts.ExpirationJitter * float64(ttl))
		ttl -= time.Duration(c.rand.Int63n(int64(jitter)))
	}

	bytes, err := c.opts.Serializer.Marshal(payload)
	if err != nil {
		c.opts.Logger.Error("Failed to marshal data", "key", key, "error", err)
		c.l2.HDel(ctx, key, "lockOwner") // Release lock
		return
	}

	err = c.setAndUnlockScript.Run(ctx, c.l2, []string{key}, c.ownerID, bytes, int(ttl.Seconds())).Err()
	if err != nil {
		c.opts.Logger.Error("Failed to run setAndUnlock script", "key", key, "error", err)
	}

	// Update L1
	c.l1.SetWithTTL(key, payload, 1, c.opts.L1TTL)
}

// IMPROVEMENT: Refresh logic has been simplified and made safer.
func (c *Cache[T]) refresh(ctx context.Context, prefixedKey string, ttl time.Duration, fn func(ctx context.Context) (T, error)) {
	_, _, _ = c.sf.Do(prefixedKey+":refresh", func() (interface{}, error) {
		c.opts.Logger.Debug("Background refresh started", "key", prefixedKey)
		// The inner logic already has distributed locks, no need to repeat here.
		_, err := c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn)
		if err != nil && !errors.Is(err, ErrNotFound) {
			c.opts.Logger.Error("Background refresh failed", "key", prefixedKey, "error", err)
		}
		return nil, nil
	})
}

// Delete removes a key from both cache layers and notifies other nodes.
func (c *Cache[T]) Delete(ctx context.Context, key string) error {
	if c.opts.DisableCacheDelete {
		c.opts.Logger.Warn("Cache delete is disabled.", "key", key)
		return nil
	}

	prefixedKey := c.prefixedKey(key)
	c.opts.Logger.Info("Deleting key from cache", "key", prefixedKey)

	c.l1.Del(prefixedKey)

	delErr := c.l2.Del(ctx, prefixedKey).Err()
	pubErr := c.l2.Publish(ctx, c.opts.InvalidationChannel, prefixedKey).Err()

	if delErr != nil {
		c.opts.Logger.Error("Failed to delete L2 cache", "key", prefixedKey, "error", delErr)
	}
	if pubErr != nil {
		c.opts.Logger.Error("Failed to publish invalidation message", "key", prefixedKey, "error", pubErr)
	}
	return errors.Join(delErr, pubErr)
}

// Close releases resources and stops background goroutines.
func (c *Cache[T]) Close() {
	c.opts.Logger.Info("Closing GoCache instance")
	c.cancel()
	c.l1.Close()
}

// listenForInvalidations runs in the background to process cache deletion messages.
func (c *Cache[T]) listenForInvalidations() {
	pubsub := c.l2.Subscribe(c.ctx, c.opts.InvalidationChannel)
	defer func() {
		if err := pubsub.Close(); err != nil {
			c.opts.Logger.Error("Failed to close PubSub connection", "error", err)
		}
	}()

	ch := pubsub.Channel()

	for {
		select {
		case <-c.ctx.Done():
			c.opts.Logger.Info("Invalidation listener stopped")
			return
		case msg := <-ch:
			c.opts.Logger.Debug("Received invalidation message", "key", msg.Payload)
			c.l1.Del(msg.Payload)
		}
	}
}

// prefixedKey creates a fully qualified key with namespace.
func (c *Cache[T]) prefixedKey(key string) string {
	return fmt.Sprintf("%s:%s", c.opts.Namespace, key)
}
