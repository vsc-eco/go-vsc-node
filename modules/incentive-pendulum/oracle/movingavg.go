package oracle

import "math"

// MovingAverageRing keeps the last N trusted oracle tick prices (HIVE in HBD).
// Use at height % tick == 0 to align consensus; Push each tick’s aggregated trusted mean.
type MovingAverageRing struct {
	N   int
	buf []float64
	i   int
	len int
}

// NewMovingAverageRing creates a ring of capacity n >= 1.
func NewMovingAverageRing(n int) *MovingAverageRing {
	if n < 1 {
		n = 1
	}
	return &MovingAverageRing{N: n, buf: make([]float64, n)}
}

// Push adds a tick value; oldest drops when full.
func (m *MovingAverageRing) Push(v float64) {
	if m == nil || m.N < 1 {
		return
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return
	}
	if len(m.buf) != m.N {
		m.buf = make([]float64, m.N)
	}
	m.buf[m.i] = v
	m.i = (m.i + 1) % m.N
	if m.len < m.N {
		m.len++
	}
}

// Mean returns the simple MA over stored ticks (only filled slots if not yet full).
func (m *MovingAverageRing) Mean() (float64, bool) {
	if m == nil || m.len == 0 {
		return 0, false
	}
	var s float64
	for k := 0; k < m.len; k++ {
		idx := (m.i - m.len + k + m.N) % m.N
		s += m.buf[idx]
	}
	return s / float64(m.len), true
}

// Reset clears the buffer.
func (m *MovingAverageRing) Reset() {
	if m == nil {
		return
	}
	m.i, m.len = 0, 0
	for i := range m.buf {
		m.buf[i] = 0
	}
}
