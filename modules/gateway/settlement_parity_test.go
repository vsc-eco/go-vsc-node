package gateway

// GatewaySettlementParityHeight, gateway half (finding C): the vsc.actions
// header must list what was actually PAID, not what was merely selected.
//
// executeActions appended an action id to executedOps BEFORE its own skip
// guards, so a withdrawal it deliberately did not pay still settled to
// "complete" at ingest. The user's L2 debit happened at withdrawal time, so
// that is value gone with nothing pending and nothing to retry -- the opposite
// failure to the double-pay this branch is about, from the same cause: the
// header did not say what happened.
//
// Consensus-affecting twice over, hence the height gate: it changes the
// header's contents, and executedOps POSITIONS are the ClearedOps bitmap's bit
// indexes. 0 = INERT on every network today, so these tests pin BOTH sides --
// the old behaviour below the height and the new one at or above it.

import (
	"encoding/json"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/vsc-eco/hivego"
)

// parityConf is a SystemConfig with one consensus param overridden. The config
// struct has no setter and its fields are private, so embedding the interface
// and shadowing the one method is the least invasive route.
type parityConf struct {
	systemconfig.SystemConfig
	height uint64
}

func (p parityConf) ConsensusParams() params.ConsensusParams {
	cp := p.SystemConfig.ConsensusParams()
	cp.GatewaySettlementParityHeight = p.height
	return cp
}

// countingCreator records the header JSON and counts the L1 transfers built.
type countingCreator struct {
	stubHiveCreator
	headerJSON string
	transfers  int
}

func (c *countingCreator) CustomJson(_ []string, _ []string, id string, jsonStr string) hivego.HiveOperation {
	if id == "vsc.actions" {
		c.headerJSON = jsonStr
	}
	return nil
}

func (c *countingCreator) Transfer(_ string, _ string, _ string, _ string, _ string) hivego.HiveOperation {
	c.transfers++
	return nil
}

// parityBatch runs one executeActions tick and returns the settled ids and the
// number of L1 transfers built.
func parityBatch(t *testing.T, parityHeight uint64, bh uint64, actions map[string]ledgerDb.ActionRecord) ([]string, int) {
	t.Helper()
	creator := &countingCreator{}
	ms := &MultiSig{
		sconf:         parityConf{SystemConfig: systemconfig.MocknetConfig(), height: parityHeight},
		ledgerActions: &test_utils.MockActionsDb{Actions: actions},
		hiveCreator:   creator,
	}
	if _, err := ms.executeActions(bh); err != nil {
		t.Fatalf("executeActions(bh=%d, parity=%d): %v", bh, parityHeight, err)
	}
	var header struct {
		Ops []string `json:"ops"`
	}
	if err := json.Unmarshal([]byte(creator.headerJSON), &header); err != nil {
		t.Fatalf("parsing vsc.actions header %q: %v", creator.headerJSON, err)
	}
	return header.Ops, creator.transfers
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// A withdrawal to a non-"hive:" destination is skipped by executeActions: no
// transfer op is built for it. Below the parity height it is nevertheless
// settled (the live behaviour, preserved); at or above it, it is not.
func TestSettlementParity_SkippedWithdrawIsNotSettled(t *testing.T) {
	const bh = uint64(20) // bh % ACTION_INTERVAL == 0
	mk := func() map[string]ledgerDb.ActionRecord {
		return map[string]ledgerDb.ActionRecord{
			"paid": {
				Id: "paid", Status: "pending", Amount: 1000, Asset: "hbd",
				To: "hive:bob", Type: "withdraw", BlockHeight: 1,
			},
			"skipped": {
				// The only non-hive destination Withdraw will mint, for assets
				// that are not core-ledger assets -- which is why this is
				// latent rather than live.
				Id: "skipped", Status: "pending", Amount: 2000, Asset: "hbd",
				To:   "eth:0x000000000000000000000000000000000000dEaD",
				Type: "withdraw", BlockHeight: 1,
			},
		}
	}

	// INERT (0): today's behaviour on every network. One transfer, two settled.
	ops, transfers := parityBatch(t, 0, bh, mk())
	if transfers != 1 {
		t.Fatalf("inert: built %d transfers, want 1 (only the hive destination is payable)", transfers)
	}
	if !contains(ops, "skipped") {
		t.Fatalf("inert: header must still settle the skipped action (%v) — changing this without the height gate forks the fleet", ops)
	}

	// ACTIVE: the skipped action is not claimed as settled, so it stays
	// pending, is reported by reconcilePendingActions, and can be dealt with.
	ops, transfers = parityBatch(t, bh, bh, mk())
	if transfers != 1 {
		t.Fatalf("active: built %d transfers, want 1 — parity must not change WHO gets paid", transfers)
	}
	if contains(ops, "skipped") {
		t.Fatalf("active: the header settles an action the gateway never paid (%v) — the user is debited and never credited", ops)
	}
	if !contains(ops, "paid") {
		t.Fatalf("active: the header dropped the action that WAS paid (%v) — that is the double-pay bug in the other direction", ops)
	}
	if len(ops) != 1 {
		t.Fatalf("active: header ops %v, want exactly [paid]", ops)
	}
}

// Below its height the gate must be completely inert, and one block below is
// the case a rollout actually passes through.
func TestSettlementParity_InertOneBlockBelowItsHeight(t *testing.T) {
	actions := map[string]ledgerDb.ActionRecord{
		"skipped": {
			Id: "skipped", Status: "pending", Amount: 2000, Asset: "hbd",
			To:   "eth:0x000000000000000000000000000000000000dEaD",
			Type: "withdraw", BlockHeight: 1,
		},
	}
	// bh=20, height=40: not yet in force.
	ops, _ := parityBatch(t, 40, 20, actions)
	if !contains(ops, "skipped") {
		t.Fatalf("a node one tick below the height must behave exactly as before, got %v", ops)
	}
}

// Stake and unstake carry no per-action transfer op (they aggregate into one
// savings op), so the parity rule must still settle them -- otherwise it would
// strand every stake the moment it activates.
func TestSettlementParity_StakeAndUnstakeStillSettle(t *testing.T) {
	const bh = uint64(20)
	actions := map[string]ledgerDb.ActionRecord{
		"s1": {Id: "s1", Status: "pending", Amount: 5000, Asset: "hbd", To: "hive:alice", Type: "stake", BlockHeight: 1},
		"u1": {Id: "u1", Status: "pending", Amount: 3000, Asset: "hbd", To: "hive:bob", Type: "unstake", BlockHeight: 1},
	}
	ops, _ := parityBatch(t, bh, bh, actions)
	if !contains(ops, "s1") || !contains(ops, "u1") {
		t.Fatalf("parity must settle stake and unstake actions, got %v", ops)
	}
}

// A destination with no ":" used to index splitTo[1] unconditionally, panicking
// inside the gateway tick and taking this node's block processing with it.
// Ungated, because a node that panics produces nothing at all, so refusing to
// panic cannot diverge from a node that never panicked.
func TestSettlementParity_DestinationWithoutPrefixDoesNotPanic(t *testing.T) {
	for _, h := range []uint64{0, 20} {
		actions := map[string]ledgerDb.ActionRecord{
			"bare": {
				Id: "bare", Status: "pending", Amount: 1000, Asset: "hbd",
				To: "bob", Type: "withdraw", BlockHeight: 1,
			},
		}
		_, transfers := parityBatch(t, h, 20, actions)
		if transfers != 0 {
			t.Fatalf("parity=%d: a destination with no network prefix must not be paid, built %d transfers", h, transfers)
		}
	}
}
