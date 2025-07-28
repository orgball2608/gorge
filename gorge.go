package gorge

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/google/uuid"
	"github.com/orgball2608/gorge/internal/payload"
	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker"
	"golang.org/x/sync/singleflight"
)

var (
	ErrNotFound         = errors.New("gorge: resource not found")
	ErrLuaScriptFailure = errors.New("gorge: unexpected response from lua script")
)

type Cache[T any] struct {
	opts           *Options
	l1             *ristretto.Cache
	l2             redis.UniversalClient
	sf             singleflight.Group
	ctx            context.Context
	cancel         context.CancelFunc
	rand           *rand.Rand
	ownerID        string
	circuitBreaker *gobreaker.CircuitBreaker

	lockAndGetScript       *redis.Script
	setAndUnlockScript     *redis.Script
	invalidateByTagsScript *redis.Script
}

func New[T any](redisClient redis.UniversalClient, opts ...Option) (*Cache[T], error) {
	options := NewDefaultOptions()
	for _, opt := range opts {
		opt(options)
	}
	if err := options.validate(); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}
	l1Cache, err := ristretto.NewCache(options.L1Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create L1 cache: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	var cb *gobreaker.CircuitBreaker
	if options.EnableCircuitBreaker {
		st := gobreaker.Settings{
			Name:        options.Namespace,
			MaxRequests: 1, // Allows a single request in the half-open state
			Interval:    0, // The clearing of counts is done by the ticker
			Timeout:     options.CircuitBreakerTimeout,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= options.CircuitBreakerMaxFailures
			},
			OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
				options.Logger.Warn("Circuit breaker state changed", "name", name, "from", from.String(), "to", to.String())
			},
		}
		cb = gobreaker.NewCircuitBreaker(st)
	}

	c := &Cache[T]{
		opts:                   options,
		l1:                     l1Cache,
		l2:                     redisClient,
		ctx:                    ctx,
		cancel:                 cancel,
		rand:                   rand.New(rand.NewSource(time.Now().UnixNano())),
		ownerID:                uuid.NewString(),
		circuitBreaker:         cb,
		lockAndGetScript:       redis.NewScript(tryLockAndGetScript),
		setAndUnlockScript:     redis.NewScript(setDataAndUnlockScript),
		invalidateByTagsScript: redis.NewScript(invalidateByTagsScript),
	}

	// Pre-load Lua scripts into Redis.
	for _, script := range []*redis.Script{c.lockAndGetScript, c.setAndUnlockScript, c.invalidateByTagsScript} {
		if err := script.Load(ctx, redisClient).Err(); err != nil {
			return nil, fmt.Errorf("failed to load lua script: %w", err)
		}
	}

	go c.listenForInvalidations()
	c.opts.Logger.Info("Gorge instance created and listener started", "namespace", c.opts.Namespace, "ownerID", c.ownerID)
	return c, nil
}

func (c *Cache[T]) Fetch(ctx context.Context, key string, ttl time.Duration, fn func(ctx context.Context) (T, error), tags ...string) (T, error) {
	var zero T
	if c.opts.DisableCacheRead {
		c.opts.Logger.Warn("Cache read is disabled. Fetching directly from DB.", "key", key)
		c.opts.Metrics.IncDBFetches()
		return fn(ctx)
	}

	prefixedKey := c.prefixedKey(key)

	// L1 Check
	if val, found := c.l1.Get(prefixedKey); found {
		c.opts.Metrics.IncL1Hits()
		pl := val.(payload.CachePayload[T])
		if pl.IsNil {
			c.opts.Logger.Debug("L1 cache hit (negative)", "key", prefixedKey)
			return zero, ErrNotFound
		}
		if c.opts.EnableStaleWhileRevalidate && pl.IsStale(c.opts.StaleTTL) {
			c.opts.Logger.Debug("L1 cache hit (stale), refreshing in background", "key", prefixedKey)
			go c.refresh(prefixedKey, ttl, fn, tags...)
		}
		c.opts.Logger.Debug("L1 cache hit", "key", prefixedKey)
		return pl.Data, nil
	}
	c.opts.Metrics.IncL1Misses()

	// Use singleflight for L2/DB access.
	// The closure will always return a (payload.CachePayload[T], error) for consistency.
	res, err, _ := c.sf.Do(prefixedKey, func() (interface{}, error) {
		// Re-check L1 inside singleflight.
		if val, found := c.l1.Get(prefixedKey); found {
			c.opts.Metrics.IncL1Hits()
			return val.(payload.CachePayload[T]), nil
		}

		// L2 Cache & Distributed Lock Loop
		for i := 0; i < c.opts.LockRetries; i++ {
			lockTTLInSeconds := int(c.opts.LockTTL.Seconds())

			// Execute Redis command via circuit breaker
			res, err := c.executeL2(func() (interface{}, error) {
				return c.lockAndGetScript.Run(ctx, c.l2, []string{prefixedKey}, c.ownerID, lockTTLInSeconds).Result()
			})

			if err != nil {
				// If circuit breaker is open, go directly to DB
				if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
					c.opts.Logger.Warn("Circuit breaker is open. Fetching directly from DB.", "key", prefixedKey, "error", err)
					return c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn)
				}
				// For other Redis errors, log and return
				c.opts.Logger.Error("Failed to run lockAndGet script", "key", prefixedKey, "error", err)
				return nil, err
			}

			results, ok := res.([]interface{})
			if !ok || len(results) < 2 {
				c.opts.Logger.Error("Unexpected response from lockAndGet script", "key", prefixedKey, "response", res)
				return nil, ErrLuaScriptFailure
			}

			value, status := results[0], results[1].(string)
			switch status {
			case "HIT":
				c.opts.Metrics.IncL2Hits()
				c.opts.Logger.Debug("L2 cache hit", "key", prefixedKey)
				return c.handleL2Hit(ctx, prefixedKey, value.(string), ttl, fn, tags...)
			case "ACQUIRED_LOCK":
				c.opts.Metrics.IncL2Misses()
				c.opts.Logger.Debug("Acquired distributed lock", "key", prefixedKey, "owner", c.ownerID)
				return c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn, tags...)
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

	// Ensure the result from the singleflight is stored in the local L1 cache.
	// This handles the case where this goroutine received a cached result from another
	// goroutine via singleflight, and therefore didn't execute the L1 set logic inside the closure.
	if _, found := c.l1.Get(prefixedKey); !found {
		var l1TTL time.Duration
		if pl.IsNil {
			l1TTL = c.opts.NegativeCacheTTL
		} else {
			remainingTTL := time.Until(pl.ExpiresAt)
			l1TTL = min(c.opts.L1TTL, remainingTTL)
		}

		if l1TTL > 0 {
			c.l1.SetWithTTL(prefixedKey, pl, 1, l1TTL)
			c.l1.Wait()
		}
	}

	if pl.IsNil {
		return zero, ErrNotFound
	}
	return pl.Data, nil
}

func (c *Cache[T]) handleL2Hit(ctx context.Context, prefixedKey, l2Val string, ttl time.Duration, fn func(ctx context.Context) (T, error), tags ...string) (interface{}, error) {
	var pl payload.CachePayload[T]
	start := time.Now()
	if err := c.opts.Serializer.Unmarshal([]byte(l2Val), &pl); err != nil {
		c.opts.Logger.Error("Failed to unmarshal L2 data, re-fetching from DB", "key", prefixedKey, "error", err)
		return c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn, tags...)
	}
	c.opts.Metrics.ObserveL2HitLatency(time.Since(start))

	remainingTTL := time.Until(pl.ExpiresAt)
	if remainingTTL > 0 {
		c.l1.SetWithTTL(prefixedKey, pl, 1, min(c.opts.L1TTL, remainingTTL))
		c.l1.Wait()
	}

	if c.opts.EnableStaleWhileRevalidate && pl.IsStale(c.opts.StaleTTL) {
		go c.refresh(prefixedKey, ttl, fn, tags...)
	}
	return pl, nil
}

func (c *Cache[T]) fetchFromDBAndSet(ctx context.Context, prefixedKey string, ttl time.Duration, fn func(ctx context.Context) (T, error), tags ...string) (interface{}, error) {
	c.opts.Metrics.IncDBFetches()
	start := time.Now()
	dbVal, err := fn(ctx)
	c.opts.Metrics.ObserveDBFetchLatency(time.Since(start))
	if err != nil {
		c.opts.Metrics.IncDBErrors()
		if errors.Is(err, ErrNotFound) {
			pl := payload.CachePayload[T]{IsNil: true}
			if err := c.setCacheAndUnlock(ctx, prefixedKey, pl, c.opts.NegativeCacheTTL, tags...); err != nil {
				return nil, fmt.Errorf("failed to set negative cache: %w", err)
			}
			return pl, nil
		}
		// Use c.ctx to ensure the lock is released even if the request context is cancelled.
		_, _ = c.executeL2(func() (interface{}, error) {
			return c.l2.HDel(c.ctx, prefixedKey, "lockOwner").Result()
		})
		return nil, err
	}

	pl := payload.CachePayload[T]{Data: dbVal, IsNil: false}
	if err := c.setCacheAndUnlock(ctx, prefixedKey, pl, ttl, tags...); err != nil {
		return nil, fmt.Errorf("failed to set cache after DB fetch: %w", err)
	}
	return pl, nil
}

func (c *Cache[T]) setCacheAndUnlock(ctx context.Context, key string, pl payload.CachePayload[T], ttl time.Duration, tags ...string) error {
	if c.opts.ExpirationJitter > 0 {
		jitter := time.Duration(c.opts.ExpirationJitter * float64(ttl))
		ttl -= time.Duration(c.rand.Int63n(int64(jitter)))
	}

	pl.ExpiresAt = time.Now().Add(ttl)

	bytes, err := c.opts.Serializer.Marshal(pl)
	if err != nil {
		c.opts.Logger.Error("Failed to marshal data, releasing lock", "key", key, "error", err)
		_, _ = c.executeL2(func() (interface{}, error) {
			return c.l2.HDel(c.ctx, key, "lockOwner").Result()
		})
		return err
	}

	_, err = c.executeL2(func() (interface{}, error) {
		// Use a pipeline to set the key and add it to tag sets atomically.
		pipe := c.l2.Pipeline()
		pipe.EvalSha(ctx, c.setAndUnlockScript.Hash(), []string{key}, c.ownerID, bytes, int(ttl.Seconds()))

		if len(tags) > 0 && !pl.IsNil {
			for _, tag := range tags {
				tagKey := c.tagKey(tag)
				pipe.SAdd(ctx, tagKey, key)
				pipe.Expire(ctx, tagKey, ttl) // Expire the tag set along with the key
			}
		}
		_, err := pipe.Exec(ctx)
		return nil, err
	})

	if err != nil {
		c.opts.Logger.Error("Failed to run setCacheAndUnlock pipeline", "key", key, "error", err)
		return err
	}

	c.l1.SetWithTTL(key, pl, 1, min(c.opts.L1TTL, ttl))
	c.l1.Wait()
	return nil
}

func (c *Cache[T]) refresh(prefixedKey string, ttl time.Duration, fn func(ctx context.Context) (T, error), tags ...string) {
	_, _, _ = c.sf.Do(prefixedKey+":refresh", func() (interface{}, error) {
		refreshCtx, cancel := context.WithTimeout(c.ctx, c.opts.RefreshTimeout)
		defer cancel()

		c.opts.Logger.Debug("Background refresh started", "key", prefixedKey)
		_, err := c.fetchFromDBAndSet(refreshCtx, prefixedKey, ttl, fn, tags...)
		if err != nil && !errors.Is(err, ErrNotFound) {
			c.opts.Logger.Error("Background refresh failed", "key", prefixedKey, "error", err)
		}
		return nil, nil // Return nil to not cache the singleflight result
	})
}

func (c *Cache[T]) Delete(ctx context.Context, key string) error {
	if c.opts.DisableCacheDelete {
		c.opts.Logger.Warn("Cache delete is disabled.", "key", key)
		return nil
	}
	prefixedKey := c.prefixedKey(key)
	c.opts.Logger.Info("Deleting key from cache", "key", prefixedKey)
	c.l1.Del(prefixedKey)

	var delErr, pubErr error

	_, delErr = c.executeL2(func() (interface{}, error) {
		return c.l2.Del(ctx, prefixedKey).Result()
	})

	_, pubErr = c.executeL2(func() (interface{}, error) {
		return c.l2.Publish(ctx, c.opts.InvalidationChannel, prefixedKey).Result()
	})

	if delErr != nil {
		c.opts.Logger.Error("Failed to delete L2 cache", "key", prefixedKey, "error", delErr)
	}
	if pubErr != nil {
		c.opts.Logger.Error("Failed to publish invalidation message", "key", prefixedKey, "error", pubErr)
	}
	return errors.Join(delErr, pubErr)
}

// Set allows manually setting a key-value pair in the cache.
// This is useful for pre-warming the cache or when data is updated from a source other than the fetch function.
// It writes to both L1 and L2 caches and publishes an invalidation message to other nodes.
func (c *Cache[T]) Set(ctx context.Context, key string, value T, ttl time.Duration) error {
	prefixedKey := c.prefixedKey(key)
	pl := payload.CachePayload[T]{Data: value, IsNil: false}

	// This is a simplified version of setCacheAndUnlock, without the locking mechanism.
	if c.opts.ExpirationJitter > 0 {
		jitter := time.Duration(c.opts.ExpirationJitter * float64(ttl))
		ttl -= time.Duration(c.rand.Int63n(int64(jitter)))
	}
	pl.ExpiresAt = time.Now().Add(ttl)

	bytes, err := c.opts.Serializer.Marshal(pl)
	if err != nil {
		c.opts.Logger.Error("Failed to marshal data for Set", "key", key, "error", err)
		return err
	}

	_, err = c.executeL2(func() (interface{}, error) {
		pipe := c.l2.Pipeline()
		pipe.HSet(ctx, prefixedKey, "value", bytes)
		pipe.Expire(ctx, prefixedKey, ttl)
		pipe.Publish(ctx, c.opts.InvalidationChannel, prefixedKey)
		_, err := pipe.Exec(ctx)
		return nil, err
	})

	if err != nil {
		c.opts.Logger.Error("Failed to execute Set pipeline", "key", key, "error", err)
		return err
	}

	c.l1.SetWithTTL(prefixedKey, pl, 1, min(c.opts.L1TTL, ttl))
	c.l1.Wait()
	c.opts.Logger.Debug("Successfully set key via manual Set", "key", prefixedKey)
	return nil
}

func (c *Cache[T]) InvalidateTags(ctx context.Context, tags ...string) error {
	if c.opts.DisableCacheDelete {
		c.opts.Logger.Warn("Cache delete is disabled.", "tags", tags)
		return nil
	}

	if len(tags) == 0 {
		return nil
	}

	c.opts.Logger.Info("Invalidating tags", "tags", tags)

	tagKeys := make([]string, len(tags))
	for i, tag := range tags {
		tagKeys[i] = c.tagKey(tag)
	}

	// We still need to get the members for L1 invalidation on other nodes.
	// This is a trade-off for correctness. The Lua script handles the atomic deletion on L2.
	keysToInvalidateLocally := make(map[string]struct{})
	for _, tagKey := range tagKeys {
		members, err := c.l2.SMembers(ctx, tagKey).Result()
		if err != nil {
			c.opts.Logger.Error("Failed to get members for tag for local invalidation", "tagKey", tagKey, "error", err)
			// Continue anyway, the Lua script will still clear L2.
		}
		for _, member := range members {
			keysToInvalidateLocally[member] = struct{}{}
		}
	}

	// Execute the Lua script to delete all keys on Redis side.
	_, err := c.executeL2(func() (interface{}, error) {
		return c.invalidateByTagsScript.Run(ctx, c.l2, tagKeys).Result()
	})

	if err != nil {
		c.opts.Logger.Error("Failed to execute invalidateByTagsScript", "error", err)
		// Don't return yet, still attempt to publish invalidations.
	}

	// Publish invalidation messages for all affected keys.
	// This is crucial for other nodes to invalidate their L1 caches.
	if len(keysToInvalidateLocally) > 0 {
		pipe := c.l2.Pipeline()
		for key := range keysToInvalidateLocally {
			c.l1.Del(key) // Invalidate local L1 cache immediately.
			pipe.Publish(ctx, c.opts.InvalidationChannel, key)
		}
		if _, pubErr := pipe.Exec(ctx); pubErr != nil {
			c.opts.Logger.Error("Failed to publish invalidation messages for tags", "error", pubErr)
			return errors.Join(err, pubErr) // Join script error and pubsub error.
		}
	}

	return err
}

func (c *Cache[T]) Close() {
	c.opts.Logger.Info("Closing Gorge instance")
	c.cancel()
	c.l1.Close()
}

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

func (c *Cache[T]) prefixedKey(key string) string {
	return fmt.Sprintf("%s:%s", c.opts.Namespace, key)
}

func (c *Cache[T]) tagKey(tag string) string {
	return fmt.Sprintf("%s:tag:%s", c.opts.Namespace, tag)
}

// executeL2 wraps a Redis operation within the circuit breaker.
func (c *Cache[T]) executeL2(op func() (interface{}, error)) (interface{}, error) {
	if !c.opts.EnableCircuitBreaker {
		return op()
	}
	res, err := c.circuitBreaker.Execute(op)
	// Ignore redis.Nil as it's a valid "not found" response, not a system error.
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	return res, nil
}
