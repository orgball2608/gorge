package gorge

import "time"

// Metrics defines the interface for tracking cache performance.
type Metrics interface {
	IncL1Hits()
	IncL1Misses()
	IncL2Hits()
	IncL2Misses()
	IncDBFetches()
	IncDBErrors()
	ObserveDBFetchLatency(d time.Duration)
	ObserveL2HitLatency(d time.Duration)
}

// noOpMetrics is a default implementation that does nothing.
type noOpMetrics struct{}

func (m *noOpMetrics) IncL1Hits()                            {}
func (m *noOpMetrics) IncL1Misses()                          {}
func (m *noOpMetrics) IncL2Hits()                            {}
func (m *noOpMetrics) IncL2Misses()                          {}
func (m *noOpMetrics) IncDBFetches()                         {}
func (m *noOpMetrics) IncDBErrors()                          {}
func (m *noOpMetrics) ObserveDBFetchLatency(d time.Duration) {}
func (m *noOpMetrics) ObserveL2HitLatency(d time.Duration)   {}
