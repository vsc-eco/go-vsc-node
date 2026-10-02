package tss

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"
)

func windowKeys(n int) []tss_db.TssKey {
	keys := make([]tss_db.TssKey, n)
	for i := range keys {
		keys[i] = tss_db.TssKey{Id: fmt.Sprintf("key-%02d", i), Algo: tss_db.EcdsaType}
	}
	return keys
}

func windowIds(keys []tss_db.TssKey) []string {
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.Id
	}
	return ids
}

// Up to the cap, the batch is exactly what it was before: same keys, same order
// (so session ids, which carry the action index, do not change for small networks).
func TestReshareWindow_SmallBatchUnchanged(t *testing.T) {
	for n := 0; n <= MAX_RESHARES_PER_ROTATE; n++ {
		keys := windowKeys(n)
		slices.Reverse(keys) // not sorted: must come back in the given order
		got := reshareWindow(keys, 700, 100)
		if !slices.Equal(windowIds(got), windowIds(keys)) {
			t.Fatalf("n=%d: got %v, want the input unchanged %v", n, windowIds(got), windowIds(keys))
		}
	}
}

// Above the cap, a batch holds at most MAX_RESHARES_PER_ROTATE keys.
func TestReshareWindow_CapsTheBatch(t *testing.T) {
	keys := windowKeys(20)
	for bh := uint64(0); bh < 3000; bh += 100 {
		if got := reshareWindow(keys, bh, 100); len(got) != MAX_RESHARES_PER_ROTATE {
			t.Fatalf("bh %d: batch of %d, want %d", bh, len(got), MAX_RESHARES_PER_ROTATE)
		}
	}
}

// Every node must pick the same keys whatever order its database returned them in
// (FindEpochKeys does not sort).
func TestReshareWindow_IndependentOfInputOrder(t *testing.T) {
	keys := windowKeys(23)
	want := windowIds(reshareWindow(keys, 1200, 100))
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		shuffled := slices.Clone(keys)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := windowIds(reshareWindow(shuffled, 1200, 100)); !slices.Equal(got, want) {
			t.Fatalf("shuffle %d: got %v, want %v", i, got, want)
		}
	}
}

// Over consecutive rotate blocks every key gets a turn, so a key that keeps
// failing (and so stays in the list) cannot hold the others back.
func TestReshareWindow_EveryKeyGetsATurn(t *testing.T) {
	for _, n := range []int{5, 7, 20, 23} {
		keys := windowKeys(n)
		seen := map[string]bool{}
		rotations := (n + MAX_RESHARES_PER_ROTATE - 1) / MAX_RESHARES_PER_ROTATE
		for r := 0; r < rotations; r++ {
			for _, k := range reshareWindow(keys, uint64(r)*100, 100) {
				seen[k.Id] = true
			}
		}
		if len(seen) != n {
			t.Fatalf("n=%d: %d of %d keys reshared in %d rotate blocks", n, len(seen), n, rotations)
		}
	}
}

// The readiness count prepares for the batch that will actually run: with the
// window applied, a node needs at most MAX_RESHARES_PER_ROTATE sets for reshares.
func TestReshareWindow_PreParamsNeedFollowsTheWindow(t *testing.T) {
	keys := windowKeys(20)
	need := countPreParamsNeed(reshareWindow(keys, 1100, 100), nil, func(string) bool { return false })
	if need != MAX_RESHARES_PER_ROTATE {
		t.Fatalf("need %d sets, want %d", need, MAX_RESHARES_PER_ROTATE)
	}
}
