package gorge

// Metrics defines the interface for tracking cache performance.
type Metrics interface {
	IncL1Hits()
	IncL1Misses()
	IncL2Hits()
	IncL2Misses()
	IncDBFetches()
	IncDBErrors()
}

// noOpMetrics is a default implementation that does nothing.
type noOpMetrics struct{}

func (m *noOpMetrics) IncL1Hits()    {}
func (m *noOpMetrics) IncL1Misses()  {}
func (m *noOpMetrics) IncL2Hits()    {}
func (m *noOpMetrics) IncL2Misses()  {}
func (m *noOpMetrics) IncDBFetches() {}
func (m *noOpMetrics) IncDBErrors()  {}
