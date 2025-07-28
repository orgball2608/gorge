package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/orgball2608/gorge"

	"github.com/redis/go-redis/v9"
)

// User represents a user record.
type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// A simple in-memory "database" for demonstration purposes.
var db = map[int]User{
	101: {ID: 101, Name: "Alice"},
	102: {ID: 102, Name: "Bob"},
}

// fetchUserFromDB simulates fetching a user from a database.
// It can return gorge.ErrNotFound if the user does not exist.
func fetchUserFromDB(ctx context.Context, userID int) (User, error) {
	slog.Info("--- ❗️ FETCHING FROM DATABASE ---", "userID", userID)
	time.Sleep(100 * time.Millisecond) // Simulate DB latency
	if user, ok := db[userID]; ok {
		return user, nil
	}
	// IMPORTANT: Return this specific error to trigger negative caching.
	return User{}, gorge.ErrNotFound
}

func main() {
	// Setup a structured logger.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Setup Redis client.
	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Sprintf("Could not connect to Redis: %v", err))
	}

	// --- Demo 1: Strong Consistency mode (default) and Negative Caching ---
	demoStrongConsistency(logger, redisClient)

	// --- Demo 2: Stale-While-Revalidate mode ---
	demoStaleWhileRevalidate(logger, redisClient)
}

func demoStrongConsistency(logger *slog.Logger, redisClient *redis.Client) {
	fmt.Println("\n\n--- DEMO 1: STRONG CONSISTENCY & NEGATIVE CACHING ---")

	cache, err := gorge.New[User](redisClient,
		gorge.WithNamespace("demo1"),
		gorge.WithLogger(logger),
		gorge.WithNegativeCacheTTL(5*time.Second), // Negative cache entries live for 5 seconds.
	)
	if err != nil {
		panic(err)
	}
	defer cache.Close()

	ctx := context.Background()

	// Case 1: Fetch an existing user twice.
	fmt.Println("\n--- Fetching existing user (101) twice ---")
	user, _ := cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 101)
	})
	fmt.Printf("Got user: %+v\n", user)
	cache.Fetch(ctx, "user:101", time.Hour, func(ctx context.Context) (User, error) { // This call should hit the cache.
		return fetchUserFromDB(ctx, 101)
	})

	// Case 2: Fetch a non-existent user twice to demonstrate negative caching.
	fmt.Println("\n--- Fetching non-existent user (999) twice ---")
	_, err = cache.Fetch(ctx, "user:999", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 999)
	})
	if errors.Is(err, gorge.ErrNotFound) {
		fmt.Println("Correctly received ErrNotFound.")
	}

	// The second fetch for the non-existent user should hit the negative cache, not the DB.
	fmt.Println("Fetching non-existent user again (should hit negative cache)...")
	_, err = cache.Fetch(ctx, "user:999", time.Hour, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 999)
	})
	if errors.Is(err, gorge.ErrNotFound) {
		fmt.Println("Correctly received ErrNotFound from negative cache.")
	}

	time.Sleep(6 * time.Second) // Wait for the negative cache entry to expire.
	fmt.Println("\n--- Fetching non-existent user after negative cache expired ---")
	cache.Fetch(ctx, "user:999", time.Hour, func(ctx context.Context) (User, error) { // This call should hit the DB again.
		return fetchUserFromDB(ctx, 999)
	})
}

func demoStaleWhileRevalidate(logger *slog.Logger, redisClient *redis.Client) {
	fmt.Println("\n\n--- DEMO 2: STALE-WHILE-REVALIDATE ---")

	cache, err := gorge.New[User](redisClient,
		gorge.WithNamespace("demo2"),
		gorge.WithLogger(logger),
		gorge.WithStaleWhileRevalidate(true), // Enable SWR mode.
	)
	if err != nil {
		panic(err)
	}
	defer cache.Close()

	ctx := context.Background()

	// First fetch to prime the cache.
	fmt.Println("\n--- Priming the cache with user 102 ---")
	ttl := 10 * time.Second
	cache.Fetch(ctx, "user:102", ttl, func(ctx context.Context) (User, error) {
		return fetchUserFromDB(ctx, 102)
	})

	// Wait for more than half of the TTL to make the data "stale".
	staleWaitTime := ttl/2 + 1*time.Second
	fmt.Printf("\nWaiting for %d seconds to make the cache data stale...\n", int(staleWaitTime.Seconds()))
	time.Sleep(staleWaitTime)

	// Fetch again. It should return the stale data instantly and trigger a background refresh.
	fmt.Println("--- Fetching stale data ---")
	start := time.Now()
	user, _ := cache.Fetch(ctx, "user:102", ttl, func(ctx context.Context) (User, error) {
		// This function should not be called in the foreground.
		slog.Error("DB function was called in the foreground during SWR!")
		return User{}, errors.New("this should not happen")
	})
	duration := time.Since(start)

	fmt.Printf("Got stale user INSTANTLY: %+v (took %v)\n", user, duration)
	fmt.Println("A background refresh should have been triggered. Check logs for details.")

	// Wait for the background goroutine to complete.
	time.Sleep(200 * time.Millisecond)
}
