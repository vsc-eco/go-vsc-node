package state_engine_test

// F4 — unprivileged nil-op permanent chain-halt — FIX VERIFICATION (red→green)
//
// ORIGINAL BUG (devnet-confirmed): an offchain tx whose op.Type is not one of
// the five handled cases in OffchainTransaction.ToTransaction() (call /
// transfer / withdraw / stake_hbd / unstake_hbd) left a nil VSCTransaction in
// the output slice. That nil was dereferenced (v.Type()) in Ingest /
// ExecuteBatch — panicking ProcessBlock on EVERY node that ingests the block,
// halting the chain. (See tests/devnet PoC for the multi-node halt.)
//
// FIX (current behavior asserted below): ToTransaction returns an ERROR the
// moment any op can't be executed exactly as submitted — an unknown op.Type, OR
// an F21 CBOR decode failure on the attacker-controlled op.Payload. The callers
// then fail the ENTIRE transaction (TxPacket.Invalid → ExecuteBatch marks it
// FAILED, charges a fixed RC, executes no op). No op is ever dropped, no nil is
// produced, and an invalid tx is never reported as a CONFIRMED no-op.
// Deterministic: invalidity is a pure function of the content-addressed bytes.

import (
	"testing"

	"vsc-node/modules/common"
	"vsc-node/modules/common/consensusversion"
	transactionpool "vsc-node/modules/transaction-pool"

	stateEngine "vsc-node/modules/state-processing"
)

// validPayload encodes a dag-cbor op payload the way a real client would, so
// DecodeTxCbor succeeds and the op routes to its concrete VSCTransaction type.
func validPayload(t *testing.T, fields map[string]interface{}) []byte {
	t.Helper()
	b, err := common.EncodeDagCbor(fields)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return b
}

// TestF4_UnknownOpFailsWholeTx: an unknown op.Type makes ToTransaction return an
// error (whole tx is invalid) and NO ops — never a nil entry, never a silent drop.
func TestF4_UnknownOpFailsWholeTx(t *testing.T) {
	tx := &stateEngine.OffchainTransaction{}
	tx.Tx = []transactionpool.VSCTransactionOp{
		{Type: "consensus_stake", Payload: []byte{}}, // unknown off-chain type
	}
	tx.Headers.RequiredAuths = []string{"hive:alice"}

	ops, err := tx.ToTransaction(consensusversion.Version{})
	if err == nil {
		t.Fatal("F4 REGRESSION: unknown op did not fail the tx (err == nil) — halt path open")
	}
	if ops != nil {
		t.Fatalf("F4: expected no ops on invalid tx, got %d", len(ops))
	}
}

// TestF4_ValidPlusInvalidFailsWholeTx: the key atomicity property — a VALID
// transfer alongside an unknown op must NOT yield an executable transfer. The
// whole tx errors out; nothing is returned to execute.
func TestF4_ValidPlusInvalidFailsWholeTx(t *testing.T) {
	tx := &stateEngine.OffchainTransaction{}
	tx.Tx = []transactionpool.VSCTransactionOp{
		{Type: "transfer", Payload: validPayload(t, map[string]interface{}{
			"from": "hive:alice", "to": "hive:bob", "amount": "1.000", "asset": "hbd",
		})}, // a perfectly valid op...
		{Type: "consensus_stake", Payload: []byte{}}, // ...next to an unknown one
	}
	tx.Headers.RequiredAuths = []string{"hive:alice"}

	ops, err := tx.ToTransaction(consensusversion.Version{})
	if err == nil {
		t.Fatal("F4 ATOMICITY: tx with one invalid op must fail entirely (err == nil)")
	}
	if ops != nil {
		t.Fatalf("F4 ATOMICITY REGRESSION: %d op(s) returned — the valid transfer must NOT survive", len(ops))
	}
}

// TestF4_ValidTxResolvesAllOpsNoNil: a fully valid multi-op tx resolves cleanly
// (no error) and contains no nil entry, so the ExecuteBatch/Ingest Type() loops
// that previously panicked complete normally.
func TestF4_ValidTxResolvesAllOpsNoNil(t *testing.T) {
	tx := &stateEngine.OffchainTransaction{}
	tx.Tx = []transactionpool.VSCTransactionOp{
		{Type: "transfer", Payload: validPayload(t, map[string]interface{}{
			"from": "hive:alice", "to": "hive:bob", "amount": "1.000", "asset": "hbd",
		})},
		{Type: "transfer", Payload: validPayload(t, map[string]interface{}{
			"from": "hive:alice", "to": "hive:carol", "amount": "2.000", "asset": "hbd",
		})},
	}
	tx.Headers.RequiredAuths = []string{"hive:alice"}

	ops, err := tx.ToTransaction(consensusversion.Version{})
	if err != nil {
		t.Fatalf("F4: valid tx unexpectedly failed to resolve: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("F4: expected 2 resolved ops, got %d", len(ops))
	}
	for i, v := range ops {
		if v == nil {
			t.Fatalf("F4 REGRESSION: nil VSCTransaction at idx %d", i)
		}
		_ = v.Type() // mirrors Ingest / ExecuteBatch
	}
}

// TestF4_ReachabilityOpTypeNotValidatedInTxPool documents that the tx-pool
// itself still has no op-type allowlist (StaticMaxRcCost costs unknown ops via
// the default branch). That is fine post-fix: the poison op now fails the whole
// tx at ToTransaction instead of nil→panic. This test stays to flag that the
// pool layer is NOT the defense — the deterministic state-processing layer is.
func TestF4_ReachabilityOpTypeNotValidatedInTxPool(t *testing.T) {
	ops := []transactionpool.VSCTransactionOp{
		{Type: "consensus_stake", Payload: []byte{}},
	}
	cost := transactionpool.StaticMaxRcCost(ops)
	if cost == 0 {
		t.Fatal("F4 reachability: StaticMaxRcCost returned 0 for unknown op — unexpected")
	}
	t.Logf("F4 NOTE: tx-pool still has no op-type allowlist (cost=%d); the failure is enforced downstream at ToTransaction, not here", cost)
}

// TestF14_UnstakeHbdDirectionVersionGated: an offchain "unstake_hbd" op (valid
// payload) must build a *TxUnstakeHbd (whose ExecuteTx releases stake) ONLY once
// the chain-active consensus version reaches 0.4.0. Below the line it keeps the
// legacy 0.3.0 behavior — *TxStakeHbd (the wrong direction, but byte-identical on
// replay). Proves the F14 fix is gated, not unconditional. Cherry-picked from
// a1c171d7 and converted from an unconditional fix to the 0.4.0 version gate.
func TestF14_UnstakeHbdDirectionVersionGated(t *testing.T) {
	build := func(active consensusversion.Version) stateEngine.VSCTransaction {
		tx := &stateEngine.OffchainTransaction{}
		tx.Tx = []transactionpool.VSCTransactionOp{
			{Type: "unstake_hbd", Payload: validPayload(t, map[string]interface{}{
				"from": "hive:alice", "to": "hive:alice", "amount": "1.000", "asset": "hbd",
			})},
		}
		tx.Headers.RequiredAuths = []string{"hive:alice"}
		ops, err := tx.ToTransaction(active)
		if err != nil {
			t.Fatalf("F14: valid unstake_hbd unexpectedly failed: %v", err)
		}
		if len(ops) != 1 {
			t.Fatalf("F14: expected 1 element for unstake_hbd, got %d", len(ops))
		}
		return ops[0]
	}

	// Below 0.4.0: legacy behavior — builds TxStakeHbd (staked, the wrong direction).
	if op := build(consensusversion.Version{Major: 0, Consensus: 3}); op == nil {
		t.Fatal("F14: nil op below the gate")
	} else if _, ok := op.(*stateEngine.TxStakeHbd); !ok {
		t.Fatalf("F14: below 0.4.0 unstake_hbd built %T, expected legacy *TxStakeHbd", op)
	}

	// At/above 0.4.0: fixed — builds TxUnstakeHbd (releases stake).
	if op := build(consensusversion.V0_4_0); op == nil {
		t.Fatal("F14: nil op at the gate")
	} else if _, ok := op.(*stateEngine.TxUnstakeHbd); !ok {
		t.Fatalf("F14 REGRESSION: at 0.4.0 unstake_hbd built %T, expected *TxUnstakeHbd (releases stake)", op)
	} else if got := op.Type(); got != "unstake_hbd" {
		t.Fatalf("F14: Type() = %q, want unstake_hbd", got)
	}
}

// TestF21_BadPayloadFailsWholeTx: a KNOWN op type carrying an empty/malformed
// CBOR payload must NOT panic DecodeTxCbor (which previously dropped the decode
// error and dereferenced a nil node in MarshalJSON → chain halt). Post-fix it
// must fail the entire tx (error, no ops) — never a silently zero-valued op or a
// CONFIRMED no-op. op.Payload is attacker-controlled.
func TestF21_BadPayloadFailsWholeTx(t *testing.T) {
	for _, opType := range []string{"transfer", "withdraw", "stake_hbd", "unstake_hbd", "call"} {
		tx := &stateEngine.OffchainTransaction{}
		tx.Tx = []transactionpool.VSCTransactionOp{
			{Type: opType, Payload: []byte{}}, // empty/malformed payload
		}
		tx.Headers.RequiredAuths = []string{"hive:alice"}

		var (
			panicVal interface{}
			ops      []stateEngine.VSCTransaction
			err      error
		)
		func() {
			defer func() { panicVal = recover() }()
			ops, err = tx.ToTransaction(consensusversion.Version{})
		}()
		if panicVal != nil {
			t.Fatalf("F21 REGRESSION: ToTransaction panicked on empty-payload %q op: %v", opType, panicVal)
		}
		if err == nil {
			t.Fatalf("F21 (%s): bad payload must fail the whole tx (err == nil)", opType)
		}
		if ops != nil {
			t.Fatalf("F21 (%s): expected no ops on invalid tx, got %d", opType, len(ops))
		}
	}
}
