package tss

import (
	"sync"
	"time"
)

// Metrics tracks TSS operation metrics for observability
type Metrics struct {
	mu sync.RWMutex

	// Reshare metrics
	ReshareDuration      []time.Duration // Histogram of reshare completion times
	ReshareSuccessCount  int64           // Counter of successful reshares
	ReshareFailureCount  int64           // Counter of failed reshares
	ReshareTimeoutCount  int64           // Counter of timeout occurrences

	// Message metrics
	MessageSendFailures  int64           // Counter of failed message sends
	MessageDrops         int64           // Counter of dropped messages (missing actionMap)
	MessageRetryCount    int64           // Counter of message retries
	MessageSendLatency   []time.Duration // Histogram of message send latencies

	// Blame metrics
	BlameCount           map[string]int64 // Counter of blame events per node
	BanCount             int64            // Counter of node bans

	// Participant metrics
	ParticipantReadinessFailures int64 // Counter of readiness check failures
}

var globalMetrics = &Metrics{
	BlameCount: make(map[string]int64),
}

// GetMetrics returns the global metrics instance
func GetMetrics() *Metrics {
	return globalMetrics
}

// RecordReshareDuration records a reshare completion duration
func (m *Metrics) RecordReshareDuration(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReshareDuration = append(m.ReshareDuration, duration)
	// Keep only last 100 entries
	if len(m.ReshareDuration) > 100 {
		m.ReshareDuration = m.ReshareDuration[len(m.ReshareDuration)-100:]
	}
}

// IncrementReshareSuccess increments successful reshare counter
func (m *Metrics) IncrementReshareSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReshareSuccessCount++
}

// IncrementReshareFailure increments failed reshare counter
func (m *Metrics) IncrementReshareFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReshareFailureCount++
}

// IncrementReshareTimeout increments timeout counter
func (m *Metrics) IncrementReshareTimeout() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReshareTimeoutCount++
}

// IncrementMessageSendFailure increments failed message send counter
func (m *Metrics) IncrementMessageSendFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MessageSendFailures++
}

// IncrementMessageDrop increments dropped message counter
func (m *Metrics) IncrementMessageDrop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MessageDrops++
}

// IncrementMessageRetry increments message retry counter
func (m *Metrics) IncrementMessageRetry() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MessageRetryCount++
}

// RecordMessageSendLatency records message send latency
func (m *Metrics) RecordMessageSendLatency(latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MessageSendLatency = append(m.MessageSendLatency, latency)
	// Keep only last 100 entries
	if len(m.MessageSendLatency) > 100 {
		m.MessageSendLatency = m.MessageSendLatency[len(m.MessageSendLatency)-100:]
	}
}

// IncrementBlameCount increments blame count for a specific node
func (m *Metrics) IncrementBlameCount(nodeAccount string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BlameCount[nodeAccount]++
}

// IncrementBanCount increments ban counter
func (m *Metrics) IncrementBanCount() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BanCount++
}

// IncrementParticipantReadinessFailure increments readiness check failure counter
func (m *Metrics) IncrementParticipantReadinessFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ParticipantReadinessFailures++
}

// GetStats returns current metric statistics
func (m *Metrics) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make(map[string]interface{})
	stats["reshare_success_count"] = m.ReshareSuccessCount
	stats["reshare_failure_count"] = m.ReshareFailureCount
	stats["reshare_timeout_count"] = m.ReshareTimeoutCount
	stats["message_send_failures"] = m.MessageSendFailures
	stats["message_drops"] = m.MessageDrops
	stats["message_retry_count"] = m.MessageRetryCount
	stats["ban_count"] = m.BanCount
	stats["participant_readiness_failures"] = m.ParticipantReadinessFailures

	// Calculate average reshare duration
	if len(m.ReshareDuration) > 0 {
		var total time.Duration
		for _, d := range m.ReshareDuration {
			total += d
		}
		stats["reshare_avg_duration_ms"] = total.Milliseconds() / int64(len(m.ReshareDuration))
	}

	// Calculate average message send latency
	if len(m.MessageSendLatency) > 0 {
		var total time.Duration
		for _, d := range m.MessageSendLatency {
			total += d
		}
		stats["message_avg_latency_ms"] = total.Milliseconds() / int64(len(m.MessageSendLatency))
	}

	// Blame counts per node
	blameCounts := make(map[string]int64)
	for node, count := range m.BlameCount {
		blameCounts[node] = count
	}
	stats["blame_counts"] = blameCounts

	return stats
}
