package oracle

// MovingAverageRing keeps the last N trusted oracle tick prices (HBD per HIVE,
// basis points). Push each tick's aggregated trusted-mean bps; Mean returns
// the simple integer mean of the filled slots in the ring.
//
// Integer-only: identical inputs across nodes produce identical Mean output.
type MovingAverageRing struct {
	N   int
	buf []int64
	i   int
	len int
}

// NewMovingAverageRing creates a ring of capacity n >= 1.
func NewMovingAverageRing(n int) *MovingAverageRing {
	if n < 1 {
		n = 1
	}
	return &MovingAverageRing{N: n, buf: make([]int64, n)}
}

// Push adds a tick value (in bps); oldest drops when full. Non-positive values
// are ignored — same intent as the float ring's NaN/Inf/<=0 guard.
func (m *MovingAverageRing) Push(v int64) {
	if m == nil || m.N < 1 {
		return
	}
	if v <= 0 {
		return
	}
	if len(m.buf) != m.N {
		m.buf = make([]int64, m.N)
	}
	m.buf[m.i] = v
	m.i = (m.i + 1) % m.N
	if m.len < m.N {
		m.len++
	}
}

// Mean returns the integer mean (in bps) over stored ticks.
func (m *MovingAverageRing) Mean() (int64, bool) {
	if m == nil || m.len == 0 {
		return 0, false
	}
	var s int64
	for k := 0; k < m.len; k++ {
		idx := (m.i - m.len + k + m.N) % m.N
		s += m.buf[idx]
	}
	return s / int64(m.len), true
}

// IsFull reports whether the ring has reached its capacity. Used by the
// FeedTracker's warmup gate to decide when in-memory state matches a long-
// running peer (a partial ring would expose a divergent moving-average
// value to consumers).
func (m *MovingAverageRing) IsFull() bool {
	if m == nil {
		return false
	}
	return m.len >= m.N
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
