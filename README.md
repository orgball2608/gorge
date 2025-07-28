<!--suppress HtmlDeprecatedAttribute -->
<div align="center">
  <br />
  <p>
    <a href="https://github.com/orgball2608/gorge"><img src="https://i.imgur.com/d4gc3K3.png" width="200" alt="gorge logo" /></a>
  </p>
  <br />
  <p>
    <a href="https://pkg.go.dev/github.com/orgball2608/gorge"><img src="https://pkg.go.dev/badge/github.com/orgball2608/gorge.svg" alt="Go Reference"></a>
    <a href="https://goreportcard.com/report/github.com/orgball2608/gorge"><img src="https://goreportcard.com/badge/github.com/orgball2608/gorge" alt="Go Report Card" /></a>
    <a href="https://github.com/orgball2608/gorge/actions/workflows/ci.yml"><img src="https://github.com/orgball2608/gorge/actions/workflows/ci.yml/badge.svg" alt="Tests"></a>
    <a href='https://coveralls.io/github/orgball2608/gorge?branch=master'><img src='https://coveralls.io/repos/github/orgball2608/gorge/badge.svg?branch=master' alt='Coverage Status' /></a>
    <a href="https://github.com/orgball2608/gorge/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="license" /></a>
  </p>
</div>

---

## 📖 Introduction

**Gorge** is a production-grade, type-safe, two-layer (in-memory + distributed) caching library for Go. It's designed to prevent cache stampedes (thundering herds), ensure high performance under heavy load, and provide advanced features like Stale-While-Revalidate and Cache Tagging out of the box.

## 🤔 Why Gorge?

In high-traffic systems, uncached database queries can quickly become a bottleneck. While caching is a standard solution, a naive implementation can introduce new problems:
- **Cache Stampede (Thundering Herd)**: When a popular cached item expires, multiple concurrent requests will all try to fetch the same data from the database simultaneously, overwhelming it.
- **Complex Invalidation**: Invalidating multiple related keys (e.g., a user's profile, posts, and settings) can be cumbersome and error-prone.
- **Latency Spikes**: Users experience slow responses whenever data needs to be fetched from the source.
- **Boilerplate**: Implementing a robust caching strategy often involves writing complex, repetitive, and error-prone code.

Gorge solves these problems by providing a simple, powerful, and production-ready caching layer that handles these complexities for you.

## ✨ Features

*   **Two-Layer Cache**: Combines the speed of an in-memory cache (Ristretto) with the scalability of a distributed cache (Redis).
*   **Type-Safe**: Leverages Go Generics to ensure type safety for cached data.
*   **Thundering Herd Protection**: Uses `singleflight` to ensure only one request fetches data from the source during a cache miss.
*   **Cache Tagging**: Group related keys with tags and invalidate them all with a single command.
*   **Explicit `Set()` Method**: Manually set values in the cache, useful for pre-warming or handling updates from message queues.
*   **Stale-While-Revalidate (SWR)**: Returns stale data instantly while a background goroutine refreshes it.
*   **Negative Caching**: Caches "not found" results to reduce load from repeated invalid requests.
*   **Distributed Lock**: Uses a Redis lock to ensure consistency when updating the cache across multiple nodes.
*   **Circuit Breaker**: Automatically detects and isolates Redis failures, preventing cascading failures in your system.
*   **Pub/Sub Invalidation**: Automatically invalidates the L1 (in-memory) cache on other nodes when a key is deleted or updated.
*   **Customizable**: Offers a flexible options pattern for tuning TTLs, logging, metrics, and more.

## 🚀 Installation

```bash
go get github.com/orgball2608/gorge
```

## ⚡️ Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/orgball2608/gorge"
	"github.com/redis/go-redis/v9"
)

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func fetchUserFromDB(ctx context.Context, userID int) (User, error) {
	slog.Info("--- ❗️ FETCHING FROM DATABASE ---", "userID", userID)
	time.Sleep(100 * time.Millisecond) // Simulate DB latency
	if userID == 101 {
		return User{ID: 101, Name: "Alice"}, nil
	}
	return User{}, gorge.ErrNotFound
}

func main() {
	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Sprintf("Could not connect to Redis: %v", err))
	}

	cache, err := gorge.New[User](redisClient,
		gorge.WithNamespace("my-app"),
		gorge.WithNegativeCacheTTL(10*time.Second),
	)
	if err != nil {
		panic(err)
	}
	defer cache.Close()

	ctx := context.Background()

	// First fetch will miss and call the function
	fmt.Println("--- First fetch (cache miss) ---")
	user, err := cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 101)
	})
	if err != nil {
		slog.Error("Fetch failed", "error", err)
	} else {
		fmt.Printf("Got user: %+v\n\n", user)
	}

	// Second fetch will hit the in-memory cache
	fmt.Println("--- Second fetch (cache hit) ---")
	user, err = cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 101)
	})
	if err != nil {
		slog.Error("Fetch failed", "error", err)
	} else {
		fmt.Printf("Got user: %+v\n", user)
	}
}
```

## 🛠️ Advanced Usage

### Cache Tagging

You can associate keys with one or more tags. This allows you to invalidate multiple related keys at once.

```go
// Set two keys related to a user, both tagged with "user:101"
_, _ = cache.Fetch(ctx, "user:101:profile", time.Hour, getUserProfile, "user:101")
_, _ = cache.Fetch(ctx, "user:101:posts", time.Hour, getUserPosts, "user:101")

// ... later, when the user updates their profile ...

// Invalidate all keys tagged with "user:101"
err := cache.InvalidateTags(ctx, "user:101")
if err != nil {
    slog.Error("Failed to invalidate tags", "error", err)
}
```

### Explicit `Set()`

Sometimes you have data from a source other than your primary fetch function (e.g., a message queue) and want to update the cache directly.

```go
// Pre-warm the cache or update it from an external source
newUser := User{ID: 202, Name: "Bob"}
err := cache.Set(ctx, "user:202", newUser, time.Hour)
if err != nil {
    slog.Error("Failed to set cache", "error", err)
}

// The Set operation also publishes an invalidation, so other nodes
// will clear this key from their local L1 cache.
```

## ⚙️ Configuration Options

| Option                       | Description                                                                                       | Default                               |
| ---------------------------- | ------------------------------------------------------------------------------------------------- | ------------------------------------- |
| `WithNamespace(string)`      | Sets a prefix for all Redis keys to prevent collisions.                                           | `"gocache"`                           |
| `WithL1TTL(time.Duration)`   | Default Time-To-Live (TTL) for the L1 (in-memory) cache.                                          | `5 * time.Minute`                     |
| `WithNegativeCacheTTL(t)`    | TTL for negative cache entries (when `fn` returns `gorge.ErrNotFound`).                           | `1 * time.Minute`                     |
| `WithStaleWhileRevalidate(b)`| Enables or disables SWR mode. When enabled, returns stale data while refreshing in the background.| `false`                               |
| `WithStaleTTL(t)`            | If SWR is enabled, a key is considered stale when its remaining TTL is less than this value.      | `1 * time.Minute`                     |
| `WithLockTTL(t)`             | The TTL for the distributed lock in Redis. Must be longer than the slowest `fn()` execution.      | `10 * time.Second`                    |
| `WithLockSleep(t)`           | The duration to wait before retrying to acquire a lock.                                           | `50 * time.Millisecond`               |
| `WithLockRetries(int)`       | The number of times to retry acquiring a lock.                                                    | `10`                                  |
| `WithExpirationJitter(f)`    | A factor (0.0 to 1.0) to add jitter to Redis TTLs, preventing mass expirations.                   | `0.0` (disabled)                      |
| `WithLogger(*slog.Logger)`   | Integrates your application's logger to monitor cache activity.                                   | `slog.Default()` (discard handler)    |
| `WithMetrics(Metrics)`       | Provides an interface to track cache hits, misses, and errors.                                    | `noOpMetrics`                         |
| `WithCacheReadDisabled(b)`   | Disables reading from the cache, always calling `fn()` directly. Useful for graceful degradation. | `false`                               |
| `WithCacheDeleteDisabled(b)` | Disables deleting from the cache.                                                                 | `false`                               |
| `WithEnableCircuitBreaker(b)`| Enables the circuit breaker for Redis operations.                                                 | `false`                               |

## 📊 Metrics

You can monitor the performance of Gorge by implementing the `Metrics` interface. This now includes observing latencies.

```go
import (
    "fmt"
    "sync"
    "time"
    "github.com/orgball2608/gorge"
)

type myMetrics {
	// ... counters
}

// Implement counter methods (IncL1Hits, etc.)
func (m *myMetrics) IncL1Hits()    { /* ... */ }
func (m *myMetrics) IncL1Misses()  { /* ... */ }
func (m *myMetrics) IncL2Hits()    { /* ... */ }
func (m *myMetrics) IncL2Misses()  { /* ... */ }
func (m *myMetrics) IncDBFetches() { /* ... */ }
func (m *myMetrics) IncDBErrors()  { /* ... */ }

// Implement latency observers
func (m *myMetrics) ObserveDBFetchLatency(d time.Duration) {
    fmt.Printf("DB Fetch took: %s\n", d)
}
func (m *myMetrics) ObserveL2HitLatency(d time.Duration) {
    fmt.Printf("L2 Hit (unmarshal) took: %s\n", d)
}

// --- In your main function ---
metrics := &myMetrics{}
cache, err := gorge.New[User](redisClient, gorge.WithMetrics(metrics))
// ... use the cache
```

## 📈 Benchmarks

Benchmarks were run on an Apple M1 (goos: darwin, goarch: arm64).

| Benchmark              | Description                               | Result (ns/op) | Allocations (B/op) | Allocations (allocs/op) |
| ---------------------- | ----------------------------------------- | -------------- | ------------------ | ----------------------- |
| `BenchmarkFetch_L1Hit` | Fetches data from the L1 (in-memory) cache| `~280 ns/op`   | `95 B/op`          | `5 allocs/op`           |
| `BenchmarkFetch_L2Hit` | Fetches data from the L2 (Redis) cache    | `~824 µs/op`   | `2281 B/op`        | `43 allocs/op`          |
| `BenchmarkFetch_DBHit` | Fetches data from the source (cache miss) | `~801 µs/op`   | `2011 B/op`        | `55 allocs/op`          |

*Note: Actual results will vary depending on your environment and network latency.*

## ❤️ Contributing

Contributions are always welcome! Please feel free to create a Pull Request to improve the project.

## 📄 License

This project is licensed under the [MIT License](LICENSE).
