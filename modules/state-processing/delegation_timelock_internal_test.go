package state_engine

import (
	"fmt"
	"testing"

	"vsc-node/modules/common/delegationmode"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"

	"go.mongodb.org/mongo-driver/mongo"
)

// Internal (white-box) tests for the delegation-mode downgrade timelock:
// computeDelegationTimelock (ingest) + resolveDelegationMode / NodeDelegationMode
// (read). They exercise the pure resolution logic against minimal mock
// witness/election DBs — no ledger or block pipeline needed. Local mocks embed
// the DB interfaces and override only the two methods under test, so this file
// can live in-package (an import of lib/test_utils would cycle).

const tlLock = 6 // params.DELEGATION_DOWNGRADE_LOCK_EPOCHS (unbond 5 + 1 margin)

// tlWitnessDB overrides only GetWitnessAtHeight; the mock is height-agnostic
// (returns the single stored row per account), which matches how the tests
// simulate the immediately-preceding row.
type tlWitnessDB struct {
	witnesses.Witnesses
	rows map[string]*witnesses.Witness
}

func (m *tlWitnessDB) GetWitnessAtHeight(account string, _ *uint64) (*witnesses.Witness, error) {
	if m.rows == nil {
		return nil, nil
	}
	return m.rows[account], nil
}

// tlElectionDB overrides only GetElectionByHeight. Unknown heights return the
// mongo.ErrNoDocuments the fail-stop reader treats as deterministic absence
// (found=false), never an infra error it would block on.
type tlElectionDB struct {
	elections.Elections
	byHeight map[uint64]elections.ElectionResult
}

func (m *tlElectionDB) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	if r, ok := m.byHeight[height]; ok {
		return r, nil
	}
	return elections.ElectionResult{}, fmt.Errorf("no election at %d: %w", height, mongo.ErrNoDocuments)
}

// electionAtEpoch builds an election pinned to a given epoch and consensus
// version (major 0). consensus=5 → the 0.5.0 delegation gate is active; 4 → off.
func electionAtEpoch(epoch, consensus uint64) elections.ElectionResult {
	er := elections.ElectionResult{}
	er.Epoch = epoch
	er.VersionMajor = 0
	er.ProtocolVersion = consensus
	return er
}

// tlEngine wires a StateEngine with the local mock DBs. witRows is keyed by BARE
// account; elecs is keyed by exact query height.
func tlEngine(witRows map[string]*witnesses.Witness, elecs map[uint64]elections.ElectionResult) *StateEngine {
	return &StateEngine{
		witnessDb:  &tlWitnessDB{rows: witRows},
		electionDb: &tlElectionDB{byHeight: elecs},
	}
}

func TestComputeDelegationTimelock_LockAndConstant(t *testing.T) {
	// The lock constant must equal the unbond period plus one epoch of margin, so
	// the guard provably tracks the consensus_unstake window.
	if params.DELEGATION_DOWNGRADE_LOCK_EPOCHS != params.CONSENSUS_UNSTAKE_LOCK_EPOCHS+1 {
		t.Fatalf("DELEGATION_DOWNGRADE_LOCK_EPOCHS=%d, want CONSENSUS_UNSTAKE_LOCK_EPOCHS(%d)+1",
			params.DELEGATION_DOWNGRADE_LOCK_EPOCHS, params.CONSENSUS_UNSTAKE_LOCK_EPOCHS)
	}
	if tlLock != int(params.DELEGATION_DOWNGRADE_LOCK_EPOCHS) {
		t.Fatalf("test tlLock=%d out of sync with params=%d", tlLock, params.DELEGATION_DOWNGRADE_LOCK_EPOCHS)
	}
}

func TestComputeDelegationTimelock_AdverseVsImmediate(t *testing.T) {
	const node = "node1"
	// Prior effective mode = share (a matured / immediate share row).
	prior := func() map[string]*witnesses.Witness {
		return map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Share}}
	}
	elecs := map[uint64]elections.ElectionResult{200: electionAtEpoch(10, 5)}

	cases := []struct {
		name       string
		announce   string
		wantEff    string
		wantMature uint64
	}{
		// Leaving Share → adverse → protected + timer from epoch 10.
		{"share->custom locks", delegationmode.Custom, delegationmode.Share, 10 + tlLock},
		{"share->deactivated locks", delegationmode.Deactivated, delegationmode.Share, 10 + tlLock},
		// Non-adverse → zero-values (immediate, byte-clean).
		{"share->share immediate", delegationmode.Share, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			se := tlEngine(prior(), elecs)
			eff, mat := se.computeDelegationTimelock(node, 200, c.announce)
			if eff != c.wantEff || mat != c.wantMature {
				t.Errorf("computeDelegationTimelock(share->%s) = (%q,%d), want (%q,%d)",
					c.announce, eff, mat, c.wantEff, c.wantMature)
			}
		})
	}
}

func TestComputeDelegationTimelock_CustomToDeactivatedImmediate(t *testing.T) {
	// custom->deactivated is acceptance-only (custom never shared on-chain), so it
	// is NOT adverse and must apply immediately (no fields stored).
	const node = "node1"
	se := tlEngine(
		map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Custom}},
		map[uint64]elections.ElectionResult{200: electionAtEpoch(10, 5)},
	)
	eff, mat := se.computeDelegationTimelock(node, 200, delegationmode.Deactivated)
	if eff != "" || mat != 0 {
		t.Errorf("custom->deactivated = (%q,%d), want immediate (\"\",0)", eff, mat)
	}
}

func TestComputeDelegationTimelock_GateOff(t *testing.T) {
	// Below the 0.5.0 floor (consensus 4), even an adverse announce stores nothing
	// → byte-identical to pre-feature history.
	const node = "node1"
	se := tlEngine(
		map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Share}},
		map[uint64]elections.ElectionResult{200: electionAtEpoch(10, 4)},
	)
	eff, mat := se.computeDelegationTimelock(node, 200, delegationmode.Custom)
	if eff != "" || mat != 0 {
		t.Errorf("gate-off share->custom = (%q,%d), want (\"\",0)", eff, mat)
	}
}

func TestComputeDelegationTimelock_NewNodeNoPrior(t *testing.T) {
	// A node's first announcement (no prior row) is never adverse — there is no
	// existing reward share to strip.
	se := tlEngine(
		map[string]*witnesses.Witness{}, // no prior
		map[uint64]elections.ElectionResult{200: electionAtEpoch(10, 5)},
	)
	for _, m := range []string{delegationmode.Share, delegationmode.Custom, delegationmode.Deactivated} {
		eff, mat := se.computeDelegationTimelock("fresh", 200, m)
		if eff != "" || mat != 0 {
			t.Errorf("first announce %q = (%q,%d), want immediate (\"\",0)", m, eff, mat)
		}
	}
}

// applyStep simulates the ingest write: compute the timelock, then store the row
// exactly as SetWitnessUpdate would (raw announced mode + eff/mat only when
// mat!=0), so the NEXT step reads it as the immediately-preceding row.
func applyStep(t *testing.T, se *StateEngine, wit map[string]*witnesses.Witness, node string, height uint64, announce string) (string, uint64) {
	t.Helper()
	eff, mat := se.computeDelegationTimelock(node, height, announce)
	row := &witnesses.Witness{Height: height, DelegationMode: delegationmode.Normalize(announce)}
	if mat != 0 {
		row.DelegationModeEffective = eff
		row.DelegationModeMaturityEpoch = mat
	}
	wit[node] = row
	return eff, mat
}

func TestComputeDelegationTimelock_ReannounceNoReset(t *testing.T) {
	// share pending->custom at epoch 10 (row height 200); re-announcing custom at a
	// later height while still within the window must CARRY the original maturity,
	// never restart it (a periodic re-announce would otherwise push it out forever).
	const node = "node1"
	wit := map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Share}}
	elecs := map[uint64]elections.ElectionResult{
		200: electionAtEpoch(10, 5),
		210: electionAtEpoch(12, 5), // still < 10+6=16
		220: electionAtEpoch(14, 5),
	}
	se := tlEngine(wit, elecs)

	_, mat0 := applyStep(t, se, wit, node, 200, delegationmode.Custom)
	if mat0 != 10+tlLock {
		t.Fatalf("initial share->custom maturity = %d, want %d", mat0, 10+tlLock)
	}
	for _, h := range []uint64{210, 220} {
		eff, mat := applyStep(t, se, wit, node, h, delegationmode.Custom)
		if mat != mat0 || eff != delegationmode.Share {
			t.Errorf("re-announce custom @%d = (%q,%d), want (share,%d) carried unchanged", h, eff, mat, mat0)
		}
	}
}

func TestComputeDelegationTimelock_UpgradeCancelsPending(t *testing.T) {
	// A pending share->custom is cancelled the moment the operator re-announces
	// share (returns to the protected mode) → immediate, no fields.
	const node = "node1"
	wit := map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Share}}
	se := tlEngine(wit, map[uint64]elections.ElectionResult{
		200: electionAtEpoch(10, 5),
		205: electionAtEpoch(11, 5), // within window
	})
	applyStep(t, se, wit, node, 200, delegationmode.Custom) // pending
	eff, mat := applyStep(t, se, wit, node, 205, delegationmode.Share)
	if eff != "" || mat != 0 {
		t.Errorf("share re-announce (cancel) = (%q,%d), want immediate (\"\",0)", eff, mat)
	}
}

func TestComputeDelegationTimelock_DistinctTargetRestartsFromProtected(t *testing.T) {
	// Pending share->custom, then the operator announces deactivated (a DIFFERENT
	// adverse target) mid-window → a fresh timer, still protecting share.
	const node = "node1"
	wit := map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Share}}
	se := tlEngine(wit, map[uint64]elections.ElectionResult{
		200: electionAtEpoch(10, 5),
		205: electionAtEpoch(11, 5),
	})
	applyStep(t, se, wit, node, 200, delegationmode.Custom)
	eff, mat := applyStep(t, se, wit, node, 205, delegationmode.Deactivated)
	if eff != delegationmode.Share || mat != 11+tlLock {
		t.Errorf("distinct adverse target = (%q,%d), want (share,%d)", eff, mat, 11+tlLock)
	}
}

func TestComputeDelegationTimelock_PreActivationShareProtected(t *testing.T) {
	// A pre-0.5.0 share row (zero timelock fields) must still protect a
	// post-activation share->custom flip.
	const node = "node1"
	wit := map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Share}} // maturity 0
	se := tlEngine(wit, map[uint64]elections.ElectionResult{300: electionAtEpoch(20, 5)})
	eff, mat := se.computeDelegationTimelock(node, 300, delegationmode.Custom)
	if eff != delegationmode.Share || mat != 20+tlLock {
		t.Errorf("pre-0.5.0 share->custom = (%q,%d), want (share,%d)", eff, mat, 20+tlLock)
	}
}

func TestNodeDelegationMode_ResolvesAcrossMaturity(t *testing.T) {
	// Read path: a pending share->custom row resolves to share before maturity and
	// custom at/after it, driven purely by the chain epoch at the read height.
	const node = "node1"
	row := &witnesses.Witness{
		Height:                      200,
		DelegationMode:              delegationmode.Custom, // announced target
		DelegationModeEffective:     delegationmode.Share,  // protected
		DelegationModeMaturityEpoch: 16,                    // 10 + 6
	}
	se := tlEngine(
		map[string]*witnesses.Witness{node: row},
		map[uint64]elections.ElectionResult{
			210: electionAtEpoch(15, 5), // before maturity
			260: electionAtEpoch(16, 5), // exactly at maturity
			300: electionAtEpoch(20, 5), // after
		},
	)
	if got := se.NodeDelegationMode("hive:"+node, 210); got != delegationmode.Share {
		t.Errorf("before maturity: got %q, want share (still protected)", got)
	}
	if got := se.NodeDelegationMode("hive:"+node, 260); got != delegationmode.Custom {
		t.Errorf("at maturity: got %q, want custom (matured)", got)
	}
	if got := se.NodeDelegationMode("hive:"+node, 300); got != delegationmode.Custom {
		t.Errorf("after maturity: got %q, want custom", got)
	}
}

func TestNodeDelegationMode_ZeroFieldsUnchanged(t *testing.T) {
	// A row with no timelock fields (pre-0.5.0 / non-adverse) resolves to its raw
	// announced mode with no epoch read — byte-identical to the old behavior.
	const node = "node1"
	se := tlEngine(
		map[string]*witnesses.Witness{node: {Height: 100, DelegationMode: delegationmode.Custom}},
		nil, // election DB never consulted when maturity==0
	)
	if got := se.NodeDelegationMode("hive:"+node, 999); got != delegationmode.Custom {
		t.Errorf("zero-field row: got %q, want custom", got)
	}
}

func TestNodeDelegationMode_GateOffIgnoresTimelock(t *testing.T) {
	// If a row carries timelock fields but the read height is below the 0.5.0 gate,
	// the raw target is returned (replay below the floor is byte-identical).
	const node = "node1"
	row := &witnesses.Witness{
		Height:                      200,
		DelegationMode:              delegationmode.Custom,
		DelegationModeEffective:     delegationmode.Share,
		DelegationModeMaturityEpoch: 16,
	}
	se := tlEngine(
		map[string]*witnesses.Witness{node: row},
		map[uint64]elections.ElectionResult{210: electionAtEpoch(15, 4)}, // consensus 4 → gate off
	)
	if got := se.NodeDelegationMode("hive:"+node, 210); got != delegationmode.Custom {
		t.Errorf("gate-off read: got %q, want custom (timelock ignored)", got)
	}
}
