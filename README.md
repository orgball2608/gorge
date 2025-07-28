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

**Gorge** is a production-grade, type-safe, two-layer (in-memory + distributed) caching library for Go. It's designed to prevent cache stampedes (thundering herds), ensure high performance under heavy load, and provide advanced features like Stale-While-Revalidate and Negative Caching out of the box.

## 🤔 Why Gorge?

In high-traffic systems, uncached database queries can quickly become a bottleneck. While caching is a standard solution, a naive implementation can introduce new problems:
- **Cache Stampede (Thundering Herd)**: When a popular cached item expires, multiple concurrent requests will all try to fetch the same data from the database simultaneously, overwhelming it.
- **Latency Spikes**: Users experience slow responses whenever data needs to be fetched from the source.
- **Boilerplate**: Implementing a robust caching strategy often involves writing complex, repetitive, and error-prone code.

Gorge solves these problems by providing a simple, powerful, and production-ready caching layer that handles these complexities for you.

## ✨ Features

*   **Two-Layer Cache**: Combines the speed of an in-memory cache (Ristretto) with the scalability of a distributed cache (Redis) to optimize read performance.
*   **Type-Safe**: Leverages Go Generics to ensure type safety for cached data, minimizing runtime errors.
*   **Thundering Herd Protection**: Uses a `singleflight` mechanism to ensure that for any given key, only one goroutine fetches data from the source during a cache miss.
*   **Stale-While-Revalidate (SWR)**: Instantly returns stale data while a background goroutine refreshes it, improving user-perceived latency.
*   **Negative Caching**: Caches "not found" results to reduce load on the backend from repeated invalid requests.
*   **Distributed Lock**: Uses a Redis lock to ensure consistency when updating the cache across multiple nodes.
*   **Graceful Degradation**: Can be configured to bypass the cache and fetch directly from the source if Redis is unavailable.
*   **Pub/Sub Invalidation**: Automatically invalidates the L1 (in-memory) cache on other nodes when a key is deleted.
*   **Customizable**: Offers a flexible options pattern for tuning TTLs, logging, metrics, and more.

## 🚀 Installation

```bash
go get github.com/orgball2608/gorge/gorge
```

## ⚡️ Quick Start

Here is a simple example of how to use `gorge` to cache a `User` object.

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/orgball2608/gorge/gorge"
	"github.com/redis/go-redis/v9"
)

// User represents a user record.
type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// A mock database call
func fetchUserFromDB(ctx context.Context, userID int) (User, error) {
	slog.Info("--- ❗️ FETCHING FROM DATABASE ---", "userID", userID)
	time.Sleep(100 * time.Millisecond) // Simulate DB latency
	if userID == 101 {
		return User{ID: 101, Name: "Alice"}, nil
	}
	// Use gorge.ErrNotFound for negative caching
	return User{}, gorge.ErrNotFound
}

func main() {
	// 1. Setup Redis client
	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Sprintf("Could not connect to Redis: %v", err))
	}

	// 2. Create a new Gorge cache instance
	cache, err := gorge.New[User](redisClient,
		gorge.WithNamespace("my-app"),
		gorge.WithNegativeCacheTTL(10*time.Second),
	)
	if err != nil {
		panic(err)
	}
	defer cache.Close()

	ctx := context.Background()

	// 3. Fetch data
	// The first time, data will be fetched from `fetchUserFromDB`
	fmt.Println("--- First fetch (cache miss) ---")
	user, err := cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 101)
	})
	if err != nil {
		slog.Error("Fetch failed", "error", err)
	} else {
		fmt.Printf("Got user: %+v\n\n", user)
	}

	// The second time, data will be retrieved instantly from the cache (in-memory)
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

## 🛠️ Configuration Options

You can customize `gorge` with various `Option` functions passed to `gorge.New()`.

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

## 📊 Metrics

You can monitor the performance of Gorge by implementing the `Metrics` interface and passing it to the `WithMetrics` option.

```go
import (
    "fmt"
    "sync"
    "github.com/orgball2608/gorge/gorge"
)

// myMetrics is a custom implementation of the Metrics interface.
type myMetrics struct {
	mu      sync.Mutex
	l1Hits  int
	l1Miss  int
	l2Hits  int
	l2Miss  int
	dbFetch int
	dbError int
}

func (m *myMetrics) IncL1Hits()    { m.mu.Lock(); m.l1Hits++; m.mu.Unlock() }
func (m *myMetrics) IncL1Misses()  { m.mu.Lock(); m.l1Miss++; m.mu.Unlock() }
func (m *myMetrics) IncL2Hits()    { m.mu.Lock(); m.l2Hits++; m.mu.Unlock() }
func (m *myMetrics) IncL2Misses()  { m.mu.Lock(); m.l2Miss++; m.mu.Unlock() }
func (m *myMetrics) IncDBFetches() { m.mu.Lock(); m.dbFetch++; m.mu.Unlock() }
func (m *myMetrics) IncDBErrors()  { m.mu.Lock(); m.dbError++; m.mu.Unlock() }

func (m *myMetrics) Print() {
    m.mu.Lock()
    defer m.mu.Unlock()
    fmt.Printf("L1 Hits: %d, L1 Misses: %d, L2 Hits: %d, L2 Misses: %d\n", m.l1Hits, m.l1Miss, m.l2Hits, m.l2Miss)
}

// --- In your main function ---
metrics := &myMetrics{}
cache, err := gorge.New[User](redisClient, gorge.WithMetrics(metrics))
// ... use the cache
metrics.Print()
```

## 📈 Benchmarks

Benchmarks were run on a local M1 Mac. To run them yourself, use the command `go test -bench=. ./gorge/...`.

| Benchmark          | Description                               | Result (ns/op) |
| ------------------ | ----------------------------------------- | -------------- |
| `BenchmarkFetch_L1Hit` | Fetches data from the L1 (in-memory) cache | `253.5 ns/op`  |
| `BenchmarkFetch_L2Hit` | Fetches data from the L2 (Redis) cache     | `224.1 ns/op`  |
| `BenchmarkFetch_DBHit` | Fetches data from the source (cache miss)  | `918409 ns/op` |

*Note: Actual results will vary depending on your environment and network latency.*

## ❤️ Contributing

Contributions are always welcome! Please feel free to create a Pull Request to improve the project.

## 📄 License

This project is licensed under the [MIT License](LICENSE).