package gorge

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/google/uuid"
	"github.com/orgball2608/gorge/gorge/internal/payload"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

var (
	ErrNotFound         = errors.New("gorge: resource not found")
	ErrLuaScriptFailure = errors.New("gorge: unexpected response from lua script")
)

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
		ownerID:            uuid.NewString(),
		lockAndGetScript:   redis.NewScript(tryLockAndGetScript),
		setAndUnlockScript: redis.NewScript(setDataAndUnlockScript),
	}
	go c.listenForInvalidations()
	c.opts.Logger.Info("Gorge instance created and listener started", "namespace", c.opts.Namespace, "ownerID", c.ownerID)
	return c, nil
}

func (c *Cache[T]) Fetch(ctx context.Context, key string, ttl time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
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
			go c.refresh(prefixedKey, ttl, fn)
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
			res, err := c.lockAndGetScript.Run(ctx, c.l2, []string{prefixedKey}, c.ownerID, lockTTLInSeconds).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
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
		}
	}

	if pl.IsNil {
		return zero, ErrNotFound
	}
	return pl.Data, nil
}

func (c *Cache[T]) handleL2Hit(ctx context.Context, prefixedKey, l2Val string, ttl time.Duration, fn func(ctx context.Context) (T, error)) (interface{}, error) {
	var pl payload.CachePayload[T]
	if err := c.opts.Serializer.Unmarshal([]byte(l2Val), &pl); err != nil {
		c.opts.Logger.Error("Failed to unmarshal L2 data, re-fetching from DB", "key", prefixedKey, "error", err)
		return c.fetchFromDBAndSet(ctx, prefixedKey, ttl, fn)
	}

	remainingTTL := time.Until(pl.ExpiresAt)
	if remainingTTL > 0 {
		c.l1.SetWithTTL(prefixedKey, pl, 1, min(c.opts.L1TTL, remainingTTL))
	}

	if c.opts.EnableStaleWhileRevalidate && pl.IsStale(c.opts.StaleTTL) {
		go c.refresh(prefixedKey, ttl, fn)
	}
	return pl, nil
}

func (c *Cache[T]) fetchFromDBAndSet(ctx context.Context, prefixedKey string, ttl time.Duration, fn func(ctx context.Context) (T, error)) (interface{}, error) {
	c.opts.Metrics.IncDBFetches()
	dbVal, err := fn(ctx)
	if err != nil {
		c.opts.Metrics.IncDBErrors()
		if errors.Is(err, ErrNotFound) {
			pl := payload.CachePayload[T]{IsNil: true}
			c.setCacheAndUnlock(ctx, prefixedKey, pl, c.opts.NegativeCacheTTL)
			return pl, nil
		}
		c.l2.HDel(ctx, prefixedKey, "lockOwner")
		return nil, err
	}

	pl := payload.CachePayload[T]{Data: dbVal, IsNil: false}
	c.setCacheAndUnlock(ctx, prefixedKey, pl, ttl)
	return pl, nil
}

func (c *Cache[T]) setCacheAndUnlock(ctx context.Context, key string, pl payload.CachePayload[T], ttl time.Duration) {
	if c.opts.ExpirationJitter > 0 {
		jitter := time.Duration(c.opts.ExpirationJitter * float64(ttl))
		ttl -= time.Duration(c.rand.Int63n(int64(jitter)))
	}

	pl.ExpiresAt = time.Now().Add(ttl)

	bytes, err := c.opts.Serializer.Marshal(pl)
	if err != nil {
		c.opts.Logger.Error("Failed to marshal data", "key", key, "error", err)
		c.l2.HDel(ctx, key, "lockOwner")
		return
	}
	err = c.setAndUnlockScript.Run(ctx, c.l2, []string{key}, c.ownerID, bytes, int(ttl.Seconds())).Err()
	if err != nil {
		c.opts.Logger.Error("Failed to run setAndUnlock script", "key", key, "error", err)
	}
	c.l1.SetWithTTL(key, pl, 1, min(c.opts.L1TTL, ttl))
}

func (c *Cache[T]) refresh(prefixedKey string, ttl time.Duration, fn func(ctx context.Context) (T, error)) {
	_, _, _ = c.sf.Do(prefixedKey+":refresh", func() (interface{}, error) {
		refreshCtx, cancel := context.WithTimeout(c.ctx, c.opts.RefreshTimeout)
		defer cancel()

		c.opts.Logger.Debug("Background refresh started", "key", prefixedKey)
		_, err := c.fetchFromDBAndSet(refreshCtx, prefixedKey, ttl, fn)
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
