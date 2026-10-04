package tss

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/modules/common/consensusversion"
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
	mgr.preParamsBatch.Store(true)
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
	mgr.preParamsBatch.Store(true)
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
	mgr.preParamsBatch.Store(true)
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
	mgr.preParamsBatch.Store(true)
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
		if got := withholdReadinessForPreParams(true, c.member, c.need, c.have); got != c.wantWithholding {
			t.Errorf("%s: withhold = %v, want %v", c.name, got, c.wantWithholding)
		}
		// Below the 0.7.0 line no member ever holds its readiness back.
		if withholdReadinessForPreParams(false, c.member, c.need, c.have) {
			t.Errorf("%s, below the line: withhold = true, want false", c.name)
		}
	}
}

// Below the 0.7.0 line the pool keeps the 0.3.0 build's single set, whatever
// batch was announced; at the line it keeps the announced batch.
func TestPreParamsPool_BelowTheLineKeepsOneSet(t *testing.T) {
	var calls atomic.Int32
	mgr := &TssManager{
		preParams:    make(chan ecKeyGen.LocalPreParams, preParamsPoolCap),
		genPreParams: slowGen(0, &calls),
	}
	mgr.raisePreParamsTarget(6)
	mgr.GeneratePreParams()
	if got := len(mgr.preParams); got != 1 {
		t.Fatalf("below the line: pool = %d, want 1", got)
	}
	mgr.preParamsBatch.Store(true)
	mgr.GeneratePreParams()
	if got := len(mgr.preParams); got != 6 {
		t.Fatalf("at the line: pool = %d, want the announced 6", got)
	}
	if got := preParamsFillLevel(false, 40, preParamsPoolCap); got != 1 {
		t.Fatalf("fill level below the line = %d, want 1", got)
	}
}

// The batch pool, B1's pooled reshare sets and VR2-08's keygen gate follow the
// chain-active version: off at mainnet's 0.3.0, on from 0.7.0, off without a
// scheduler.
func TestPreParamsPool_GatesFollowTheActiveVersion(t *testing.T) {
	for _, tc := range []struct {
		ver  consensusversion.Version
		want bool
	}{
		{consensusversion.Version{}, false},
		{consensusversion.V0_3_0, false},
		{consensusversion.Version{Major: 0, Consensus: 6}, false},
		{consensusversion.V0_7_0, true},
		{consensusversion.V0_9_0, true},
	} {
		mgr := &TssManager{scheduler: &fakeSolvencyScheduler{minVer: tc.ver}}
		if got := mgr.batchPreParamsActive(100); got != tc.want {
			t.Errorf("active %v: batchPreParamsActive = %v, want %v", tc.ver, got, tc.want)
		}
		if got := mgr.reshareTakesPooledPreParams(100); got != tc.want {
			t.Errorf("active %v: reshareTakesPooledPreParams = %v, want %v", tc.ver, got, tc.want)
		}
		if got := mgr.keygenReadinessGateActive(100); got != tc.want {
			t.Errorf("active %v: keygenReadinessGateActive = %v, want %v", tc.ver, got, tc.want)
		}
	}
	mgr := &TssManager{}
	if mgr.batchPreParamsActive(100) || mgr.reshareTakesPooledPreParams(100) || mgr.keygenReadinessGateActive(100) {
		t.Error("no scheduler: want the 0.3.0 behaviour")
	}
}
