package gorge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mockMetrics is an implementation of the Metrics interface that records calls.
type mockMetrics struct {
	mu      sync.Mutex
	l1Hits  int
	l1Miss  int
	l2Hits  int
	l2Miss  int
	dbFetch int
	dbError int
}

func (m *mockMetrics) IncL1Hits()    { m.mu.Lock(); m.l1Hits++; m.mu.Unlock() }
func (m *mockMetrics) IncL1Misses()  { m.mu.Lock(); m.l1Miss++; m.mu.Unlock() }
func (m *mockMetrics) IncL2Hits()    { m.mu.Lock(); m.l2Hits++; m.mu.Unlock() }
func (m *mockMetrics) IncL2Misses()  { m.mu.Lock(); m.l2Miss++; m.mu.Unlock() }
func (m *mockMetrics) IncDBFetches() { m.mu.Lock(); m.dbFetch++; m.mu.Unlock() }
func (m *mockMetrics) IncDBErrors()  { m.mu.Lock(); m.dbError++; m.mu.Unlock() }

func (m *mockMetrics) get() (int, int, int, int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.l1Hits, m.l1Miss, m.l2Hits, m.l2Miss, m.dbFetch, m.dbError
}

func TestGorge_Metrics(t *testing.T) {
	ctx := context.Background()
	key := "metrics-key"
	value := "metrics-value"
	ns := "metrics-test"

	fn := func(ctx context.Context) (string, error) {
		return value, nil
	}

	m := &mockMetrics{}
	g, err := New[string](rdb, WithNamespace(ns), WithMetrics(m))
	assert.NoError(t, err)
	defer g.Close()

	// 1. L1 miss, L2 miss, DB hit
	_, err = g.Fetch(ctx, key, time.Hour, fn)
	assert.NoError(t, err)
	l1h, l1m, l2h, l2m, dbf, dbe := m.get()
	assert.Equal(t, 0, l1h, "1: L1 Hits")
	assert.Equal(t, 1, l1m, "1: L1 Misses")
	assert.Equal(t, 0, l2h, "1: L2 Hits")
	assert.Equal(t, 1, l2m, "1: L2 Misses")
	assert.Equal(t, 1, dbf, "1: DB Fetches")
	assert.Equal(t, 0, dbe, "1: DB Errors")

	// 2. L1 hit
	_, err = g.Fetch(ctx, key, time.Hour, fn)
	assert.NoError(t, err)
	l1h, l1m, l2h, l2m, dbf, dbe = m.get()
	assert.Equal(t, 1, l1h, "2: L1 Hits")
	assert.Equal(t, 1, l1m, "2: L1 Misses")
	assert.Equal(t, 0, l2h, "2: L2 Hits")
	assert.Equal(t, 1, l2m, "2: L2 Misses")
	assert.Equal(t, 1, dbf, "2: DB Fetches")
	assert.Equal(t, 0, dbe, "2: DB Errors")

	// 3. L1 miss, L2 hit
	g.l1.Clear()
	_, err = g.Fetch(ctx, key, time.Hour, fn)
	assert.NoError(t, err)
	l1h, l1m, l2h, l2m, dbf, dbe = m.get()
	assert.Equal(t, 1, l1h, "3: L1 Hits")
	assert.Equal(t, 2, l1m, "3: L1 Misses")
	assert.Equal(t, 1, l2h, "3: L2 Hits")
	assert.Equal(t, 1, l2m, "3: L2 Misses")
	assert.Equal(t, 1, dbf, "3: DB Fetches")
	assert.Equal(t, 0, dbe, "3: DB Errors")

	// 4. DB Error
	err = g.Delete(ctx, "db-error-key") // Clear previous attempts
	assert.NoError(t, err)
	_, err = g.Fetch(ctx, "db-error-key", time.Hour, func(ctx context.Context) (string, error) {
		return "", assert.AnError
	})
	assert.Error(t, err)
	l1h, l1m, l2h, l2m, dbf, dbe = m.get()
	assert.Equal(t, 1, l1h, "4: L1 Hits")
	assert.Equal(t, 3, l1m, "4: L1 Misses")
	assert.Equal(t, 1, l2h, "4: L2 Hits")
	assert.Equal(t, 2, l2m, "4: L2 Misses")
	assert.Equal(t, 2, dbf, "4: DB Fetches")
	assert.Equal(t, 1, dbe, "4: DB Errors")
}
