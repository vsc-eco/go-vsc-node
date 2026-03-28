package blockproducer

import (
	"sort"
	"testing"
)

// TestSortedMapKeys verifies that our sorting approach produces deterministic
// key order regardless of Go's non-deterministic map iteration.
func TestSortedMapKeys(t *testing.T) {
	testMap := map[string]int{
		"contract-z": 1,
		"contract-a": 2,
		"contract-m": 3,
		"contract-b": 4,
	}

	var lastOrder []string
	for i := 0; i < 100; i++ {
		keys := make([]string, 0, len(testMap))
		for k := range testMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		if lastOrder != nil {
			for j, k := range keys {
				if k != lastOrder[j] {
					t.Fatalf("iteration %d: key order changed at index %d: got %s, want %s", i, j, k, lastOrder[j])
				}
			}
		}
		lastOrder = keys
	}

	expected := []string{"contract-a", "contract-b", "contract-m", "contract-z"}
	for i, k := range lastOrder {
		if k != expected[i] {
			t.Errorf("index %d: got %s, want %s", i, k, expected[i])
		}
	}
}

// TestSortedDeletionsAndCache simulates the MakeOutputs pattern where
// deletions and cache updates must be applied in deterministic order.
func TestSortedDeletionsAndCache(t *testing.T) {
	deletions := map[string]bool{
		"key-z": true,
		"key-a": true,
		"key-m": true,
	}
	cache := map[string][]byte{
		"key-z": []byte("val-z"),
		"key-b": []byte("val-b"),
		"key-a": []byte("val-a"),
		"key-c": []byte("val-c"),
	}

	delKeys := make([]string, 0, len(deletions))
	for key := range deletions {
		delKeys = append(delKeys, key)
	}
	sort.Strings(delKeys)

	expectedDel := []string{"key-a", "key-m", "key-z"}
	for i, k := range delKeys {
		if k != expectedDel[i] {
			t.Errorf("deletion index %d: got %s, want %s", i, k, expectedDel[i])
		}
	}

	cacheKeys := make([]string, 0, len(cache))
	for key := range cache {
		cacheKeys = append(cacheKeys, key)
	}
	sort.Strings(cacheKeys)

	applied := make([]string, 0)
	for _, key := range cacheKeys {
		if deletions[key] {
			continue
		}
		applied = append(applied, key)
	}

	expectedApplied := []string{"key-b", "key-c"}
	if len(applied) != len(expectedApplied) {
		t.Fatalf("applied count: got %d, want %d", len(applied), len(expectedApplied))
	}
	for i, k := range applied {
		if k != expectedApplied[i] {
			t.Errorf("applied index %d: got %s, want %s", i, k, expectedApplied[i])
		}
	}
}

// TestSortedLogIds simulates the state_engine PopLogs sort where
// cross-contract call logs must be ordered deterministically.
func TestSortedLogIds(t *testing.T) {
	logs := map[string]string{
		"vsc1Pool...":    "swap log",
		"vsc1Router...":  "execute log",
		"vsc1Mapping...": "map log",
	}

	var lastOrder []string
	for i := 0; i < 100; i++ {
		logIds := make([]string, 0, len(logs))
		for id := range logs {
			logIds = append(logIds, id)
		}
		sort.Strings(logIds)

		if lastOrder != nil {
			for j, id := range logIds {
				if id != lastOrder[j] {
					t.Fatalf("iteration %d: log order changed at index %d: got %s, want %s", i, j, id, lastOrder[j])
				}
			}
		}
		lastOrder = logIds
	}

	expected := []string{"vsc1Mapping...", "vsc1Pool...", "vsc1Router..."}
	for i, id := range lastOrder {
		if id != expected[i] {
			t.Errorf("index %d: got %s, want %s", i, id, expected[i])
		}
	}
}
