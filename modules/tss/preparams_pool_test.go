package tss

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	tss_db "vsc-node/modules/db/vsc/tss"

	ecKeyGen "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
)

// slowGen stands in for ecKeyGen.GeneratePreParams: each set takes `delay`.
func slowGen(delay time.Duration, calls *atomic.Int32) func(time.Duration) (*ecKeyGen.LocalPreParams, error) {
	return func(time.Duration) (*ecKeyGen.LocalPreParams, error) {
		calls.Add(1)
		time.Sleep(delay)
		return &ecKeyGen.LocalPreParams{}, nil
	}
}

// TSS-BATCH-1. A batch of N ECDSA ceremonies takes N sets, one per dispatcher
// Start(), and RunActions starts them one after another. With the pool warm, all
// N starts must take their set at once. Before the fix the pool held one set and
// was refilled only when empty, so the batch paid N-1 generations back to back.
func TestPreParamsPool_WarmPoolStartsABatchWithoutGenerating(t *testing.T) {
	const batch = 6
	const delay = 200 * time.Millisecond
	var calls atomic.Int32
	mgr := &TssManager{
		preParams:    make(chan ecKeyGen.LocalPreParams, preParamsPoolCap),
		genPreParams: slowGen(delay, &calls),
	}
	mgr.raisePreParamsTarget(batch)
	mgr.GeneratePreParams()
	if got := len(mgr.preParams); got != batch {
		t.Fatalf("pool after fill = %d, want %d", got, batch)
	}

	calls.Store(0)
	start := time.Now()
	for i := 0; i < batch; i++ {
		// What each dispatcher Start() does: kick a top-up, then take one set.
		go mgr.GeneratePreParams()
		if _, err := mgr.awaitPreParams(nil, "batch"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > delay {
		t.Fatalf("starting %d sessions took %s; with a warm pool no start may wait on a generation (%s each)", batch, elapsed, delay)
	}
}

// The fill loop stops at the target and never blocks on a full pool.
func TestPreParamsPool_FillStopsAtTargetAndCap(t *testing.T) {
	var calls atomic.Int32
	mgr := &TssManager{
		preParams:    make(chan ecKeyGen.LocalPreParams, 3),
		genPreParams: slowGen(0, &calls),
	}
	mgr.GeneratePreParams()
	if got := len(mgr.preParams); got != DEFAULT_PREPARAMS_POOL {
		t.Fatalf("default fill = %d, want %d", got, DEFAULT_PREPARAMS_POOL)
	}
	mgr.raisePreParamsTarget(10) // above the channel's capacity
	mgr.GeneratePreParams()
	if got := len(mgr.preParams); got != 3 {
		t.Fatalf("fill with target above capacity = %d, want the capacity 3", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("generated %d sets, want exactly 3 (no set generated for a full pool)", n)
	}
}

// The target only rises: a node that needed N sets for one batch needs them again
// at the next epoch's reshares.
func TestPreParamsPool_TargetNeverLowers(t *testing.T) {
	mgr := &TssManager{preParams: make(chan ecKeyGen.LocalPreParams, preParamsPoolCap)}
	mgr.raisePreParamsTarget(5)
	mgr.raisePreParamsTarget(3)
	if got := mgr.preParamsFillTarget(); got != 5 {
		t.Fatalf("target = %d, want 5", got)
	}
}

// A failed generation ends the fill attempt cleanly (the next tick retries).
func TestPreParamsPool_GenerationFailureStopsCleanly(t *testing.T) {
	mgr := &TssManager{
		preParams: make(chan ecKeyGen.LocalPreParams, preParamsPoolCap),
		genPreParams: func(time.Duration) (*ecKeyGen.LocalPreParams, error) {
			return nil, errors.New("safe prime timeout")
		},
	}
	mgr.raisePreParamsTarget(4)
	done := make(chan struct{})
	go func() { mgr.GeneratePreParams(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GeneratePreParams did not return after a failed generation")
	}
	if len(mgr.preParams) != 0 {
		t.Fatalf("pool = %d after failures, want 0", len(mgr.preParams))
	}
}

// countPreParamsNeed counts what the node's own dispatchers take at a rotate
// block: ECDSA reshares that are not skipped, plus ECDSA keygens. EdDSA takes none.
func TestCountPreParamsNeed(t *testing.T) {
	reshare := []tss_db.TssKey{
		{Id: "a", Algo: tss_db.EcdsaType},
		{Id: "b", Algo: tss_db.EcdsaType}, // skipped: a superseded vault generation
		{Id: "c", Algo: tss_db.EddsaType},
		{Id: "d", Algo: tss_db.EcdsaType},
	}
	newKeys := []tss_db.TssKey{
		{Id: "e", Algo: tss_db.EcdsaType},
		{Id: "f", Algo: tss_db.EddsaType},
	}
	skip := func(keyId string) bool { return keyId == "b" }
	if got := countPreParamsNeed(reshare, newKeys, skip); got != 3 {
		t.Fatalf("need = %d, want 3 (a, d reshares + e keygen)", got)
	}
	if got := countPreParamsNeed(nil, nil, skip); got != 0 {
		t.Fatalf("need with no ceremonies = %d, want 0", got)
	}
}

// A member holds its readiness back only while it lacks sets for the batch; a
// non-member (a retiring-generation signer joins only as an old party) never does.
func TestWithholdReadinessForPreParams(t *testing.T) {
	cases := []struct {
		name            string
		member          bool
		need, have      int
		wantWithholding bool
	}{
		{"member short", true, 5, 2, true},
		{"member exactly enough", true, 5, 5, false},
		{"member more than enough", true, 5, 9, false},
		{"member, no ceremony", true, 0, 0, false},
		{"retiring signer, not a member", false, 5, 0, false},
	}
	for _, c := range cases {
		if got := withholdReadinessForPreParams(c.member, c.need, c.have); got != c.wantWithholding {
			t.Errorf("%s: withhold = %v, want %v", c.name, got, c.wantWithholding)
		}
	}
}
