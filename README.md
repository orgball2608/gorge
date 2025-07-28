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
    <a href="https://coveralls.io/github/orgball2608/gorge"><img src="https://coveralls.io/repos/github/orgball2608/gorge/badge.svg" alt="Coverage Status" /></a>
    <a href="https://github.com/orgball2608/gorge/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="license" /></a>
  </p>
</div>

---

## 📖 Introduction

**Gorge** is a production-grade, type-safe, two-layer (in-memory + distributed) caching library for Go. It's designed to prevent cache stampedes (thundering herds), ensure high performance under heavy load, and provide advanced features like Stale-While-Revalidate and Negative Caching out of the box.

## ✨ Features

*   **Two-Layer Cache**: Combines the speed of an in-memory cache (Ristretto) with the scalability of a distributed cache (Redis) to optimize read performance.
*   **Type-Safe**: Leverages Go Generics to ensure type safety for cached data, minimizing runtime errors.
*   **Thundering Herd Protection (Single-flight)**: Ensures that for the same key, only one goroutine fetches data from the source (e.g., database) during a cache miss.
*   **Stale-While-Revalidate (SWR)**: Instantly returns stale data while a background goroutine refreshes it, improving user-perceived latency.
*   **Negative Caching**: Caches "not found" results to reduce load on the backend from repeated invalid requests.
*   **Distributed Lock**: Uses a Redis lock to ensure consistency when updating the cache across multiple nodes.
*   **Graceful Degradation**: Can automatically or manually disable cache reads/writes if Redis fails, ensuring the system remains operational.
*   **Pub/Sub Invalidation**: Automatically invalidates the L1 (in-memory) cache on other nodes when a key is deleted or updated.

## 🚀 Installation

```bash
go get github.com/orgball2608/gorge
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

// fetchUserFromDB simulates fetching a user from a database.
func fetchUserFromDB(ctx context.Context, userID int) (User, error) {
	slog.Info("--- ❗️ FETCHING FROM DATABASE ---", "userID", userID)
	time.Sleep(100 * time.Millisecond) // Simulate DB latency
	if userID == 101 {
		return User{ID: 101, Name: "Alice"}, nil
	}
	return User{}, gorge.ErrNotFound // Use for negative caching
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
	user, _ := cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 101)
	})
	fmt.Printf("Got user: %+v\n\n", user)

	// The second time, data will be retrieved instantly from the cache (in-memory)
	fmt.Println("--- Second fetch (cache hit) ---")
	user, _ = cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 101)
	})
	fmt.Printf("Got user: %+v\n", user)
}
```

## 🛠️ Advanced Configuration

You can customize `gorge` with various `Option` functions.

| Option                       | Description                                                                                       | Example                                         |
| ---------------------------- | ------------------------------------------------------------------------------------------------- | ----------------------------------------------- |
| `WithNamespace(string)`      | Sets a prefix for all Redis keys to prevent collisions.                                           | `gorge.WithNamespace("production")`             |
| `WithL1TTL(time.Duration)`   | Default Time-To-Live (TTL) for the L1 (in-memory) cache.                                          | `gorge.WithL1TTL(10 * time.Minute)`             |
| `WithNegativeCacheTTL(t)`    | TTL for negative cache entries (when the fetch function returns `gorge.ErrNotFound`).             | `gorge.WithNegativeCacheTTL(5 * time.Second)`   |
| `WithStaleWhileRevalidate(b)`| Enables or disables SWR mode.                                                                     | `gorge.WithStaleWhileRevalidate(true)`          |
| `WithStaleTTL(t)`            | When a key's remaining TTL is less than this value, SWR will be triggered.                        | `gorge.WithStaleTTL(30 * time.Second)`          |
| `WithLockTTL(t)`             | The TTL for the distributed lock. Must be longer than the slowest `fn()` execution time.          | `gorge.WithLockTTL(10 * time.Second)`           |
| `WithLogger(*slog.Logger)`   | Integrates your application's logger to monitor cache activity.                                   | `gorge.WithLogger(myLogger)`                    |
| `WithCacheReadDisabled(b)`   | Disables reading from the cache, always calling `fn()` directly. Useful when Redis is down.       | `gorge.WithCacheReadDisabled(true)`             |

### A Note on TTL

It's important to understand how `gorge` handles the `ttl` parameter in the `Fetch` function:

- **TTL is set on cache miss**: The `ttl` is only applied to the L2 cache (Redis) when there is a cache miss and the data is fetched from your source function (`fn`).
- **TTL is NOT updated on cache hit**: When data is retrieved from the cache (L1 or L2), its existing expiration time is not modified. If you call `Fetch` with a new `ttl` for an existing key, it will not update the key's TTL in Redis.

This design is intentional to minimize Redis commands on cache hits. If you need to update the TTL on every access, you would typically handle that logic in your application layer by explicitly deleting and re-fetching the key.

## 📊 Benchmarks

Benchmarks were run on a local machine. To run them yourself, use the command `go test -bench=.` in the `gorge` directory.

| Benchmark          | Description                               | Result (ns/op)                 |
| ------------------ | ----------------------------------------- | ------------------------------ |
| `BenchmarkFetch_L1Hit` | Fetches data from the L1 (in-memory) cache | *(run 'go test -bench=.')*     |
| `BenchmarkFetch_L2Hit` | Fetches data from the L2 (Redis) cache     | *(run 'go test -bench=.')*     |
| `BenchmarkFetch_DBHit` | Fetches data from the source (cache miss)  | *(run 'go test -bench=.')*     |

*Note: Actual results will vary depending on your environment.*

## ❤️ Contributing

Contributions are always welcome! Please feel free to create a Pull Request to improve the project.

## 📄 License

This project is licensed under the [MIT License](LICENSE).