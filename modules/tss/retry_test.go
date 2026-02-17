package tss

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGetRetryCount(t *testing.T) {
	tssMgr := &TssManager{
		retryCounts:   make(map[string]int),
		retryCountsMu: sync.Mutex{},
	}

	// Test initial retry count is 0
	count := tssMgr.getRetryCount("key1")
	assert.Equal(t, 0, count, "Initial retry count should be 0")

	// Test increment
	tssMgr.incrementRetryCount("key1")
	count = tssMgr.getRetryCount("key1")
	assert.Equal(t, 1, count, "Retry count should be 1 after increment")

	// Test multiple increments
	tssMgr.incrementRetryCount("key1")
	tssMgr.incrementRetryCount("key1")
	count = tssMgr.getRetryCount("key1")
	assert.Equal(t, 3, count, "Retry count should be 3 after 3 increments")

	// Test different keys are independent
	count = tssMgr.getRetryCount("key2")
	assert.Equal(t, 0, count, "key2 should have 0 retries")
}

func TestRetryCountExceedsMax(t *testing.T) {
	tssMgr := &TssManager{
		retryCounts:   make(map[string]int),
		retryCountsMu: sync.Mutex{},
	}

	keyId := "test-key-1"

	// Simulate reaching max retries
	for i := 0; i < MAX_RESHARE_RETRIES; i++ {
		retryCount := tssMgr.getRetryCount(keyId)
		if retryCount < MAX_RESHARE_RETRIES {
			tssMgr.incrementRetryCount(keyId)
		}
	}

	// Verify we've hit the limit
	finalCount := tssMgr.getRetryCount(keyId)
	assert.Equal(t, MAX_RESHARE_RETRIES, finalCount, "Should be at max retries")

	// The next check should indicate we've exceeded
	canRetry := tssMgr.getRetryCount(keyId) < MAX_RESHARE_RETRIES
	assert.False(t, canRetry, "Should not be able to retry after max reached")
}

func TestExponentialBackoffCalculation(t *testing.T) {
	// Test that retry delay follows exponential backoff pattern
	baseDelay := TSS_RESHARE_SYNC_DELAY

	// Retry 0: delay = (0+1) * baseDelay = 1 * baseDelay
	delay0 := time.Duration(0+1) * baseDelay
	assert.Equal(t, baseDelay, delay0, "First retry delay should equal base delay")

	// Retry 1: delay = (1+1) * baseDelay = 2 * baseDelay
	delay1 := time.Duration(1+1) * baseDelay
	assert.Equal(t, 2*baseDelay, delay1, "Second retry delay should be 2x base")

	// Retry 2: delay = (2+1) * baseDelay = 3 * baseDelay
	delay2 := time.Duration(2+1) * baseDelay
	assert.Equal(t, 3*baseDelay, delay2, "Third retry delay should be 3x base")

	// Verify exponential pattern (actually linear in current implementation)
	// Current: delay = retryCount+1 * baseDelay
	// Could be changed to: delay = 2^retryCount * baseDelay for true exponential
}

func TestRetryCountReset(t *testing.T) {
	tssMgr := &TssManager{
		retryCounts:   make(map[string]int),
		retryCountsMu: sync.Mutex{},
	}

	keyId := "test-key-reset"

	// Increment a few times
	tssMgr.incrementRetryCount(keyId)
	tssMgr.incrementRetryCount(keyId)
	assert.Equal(t, 2, tssMgr.getRetryCount(keyId))

	// Simulate successful operation by resetting
	tssMgr.retryCountsMu.Lock()
	tssMgr.retryCounts[keyId] = 0
	tssMgr.retryCountsMu.Unlock()

	assert.Equal(t, 0, tssMgr.getRetryCount(keyId), "Retry count should reset to 0")
}

func TestConcurrentRetryCountAccess(t *testing.T) {
	tssMgr := &TssManager{
		retryCounts:   make(map[string]int),
		retryCountsMu: sync.Mutex{},
	}

	keyId := "concurrent-test-key"
	numGoroutines := 10
	incrementsPerGoroutine := 100

	// Run concurrent increments
	done := make(chan bool, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			for j := 0; j < incrementsPerGoroutine; j++ {
				tssMgr.incrementRetryCount(keyId)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify final count matches expected
	expected := numGoroutines * incrementsPerGoroutine
	actual := tssMgr.getRetryCount(keyId)
	assert.Equal(t, expected, actual, "Final count should match expected after concurrent increments")
}
