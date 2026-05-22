package tss_db

import (
	"testing"
)

// TestDedupCommitmentsBySemanticKey_CollapsesDuplicateRetries pins the
// migration-safety guarantee added with audit S8: pre-fix rows with the
// same (key_id, block_height, type) but different tx_ids — created by
// retries before the upsert filter was tightened — must collapse to a
// single representative per semantic key at read time.
func TestDedupCommitmentsBySemanticKey_CollapsesDuplicateRetries(t *testing.T) {
	input := []TssCommitment{
		{KeyId: "k1", BlockHeight: 100, Type: "blame", TxId: "ax", Commitment: "BV"},
		{KeyId: "k1", BlockHeight: 100, Type: "blame", TxId: "ay", Commitment: "BV"}, // retry
		{KeyId: "k1", BlockHeight: 100, Type: "blame", TxId: "az", Commitment: "BV"}, // retry
		{KeyId: "k1", BlockHeight: 101, Type: "blame", TxId: "b1", Commitment: "BV2"},
		{KeyId: "k2", BlockHeight: 100, Type: "blame", TxId: "c1", Commitment: "BV3"},
	}
	out := DedupCommitmentsBySemanticKey(input)
	if len(out) != 3 {
		t.Fatalf("expected 3 dedup'd rows, got %d: %+v", len(out), out)
	}
	// Determinism: the winner for the triplicated row must be the
	// lexicographically smallest tx_id ("ax"), regardless of input order.
	for _, c := range out {
		if c.KeyId == "k1" && c.BlockHeight == 100 && c.Type == "blame" {
			if c.TxId != "ax" {
				t.Fatalf("expected tx_id=ax (lex min) as winner, got %q", c.TxId)
			}
		}
	}
	// Sort order must be (block_height asc, key_id asc, type asc, tx_id asc).
	if out[0].BlockHeight != 100 || out[0].KeyId != "k1" {
		t.Errorf("ordering wrong at idx 0: %+v", out[0])
	}
	if out[1].BlockHeight != 100 || out[1].KeyId != "k2" {
		t.Errorf("ordering wrong at idx 1: %+v", out[1])
	}
	if out[2].BlockHeight != 101 {
		t.Errorf("ordering wrong at idx 2: %+v", out[2])
	}
}

// TestDedupCommitmentsBySemanticKey_PreservesNonDuplicates ensures rows
// that differ in (key_id, block_height, type) are all kept.
func TestDedupCommitmentsBySemanticKey_PreservesNonDuplicates(t *testing.T) {
	input := []TssCommitment{
		{KeyId: "k1", BlockHeight: 100, Type: "blame", TxId: "a"},
		{KeyId: "k1", BlockHeight: 100, Type: "reshare", TxId: "b"},
		{KeyId: "k1", BlockHeight: 101, Type: "blame", TxId: "c"},
		{KeyId: "k2", BlockHeight: 100, Type: "blame", TxId: "d"},
	}
	out := DedupCommitmentsBySemanticKey(input)
	if len(out) != 4 {
		t.Fatalf("expected 4 rows (no dups), got %d", len(out))
	}
}

// TestDedupCommitmentsBySemanticKey_EmptyAndSingletonPassthrough confirms
// the fast paths don't allocate / reorder.
func TestDedupCommitmentsBySemanticKey_EmptyAndSingletonPassthrough(t *testing.T) {
	if out := DedupCommitmentsBySemanticKey(nil); len(out) != 0 {
		t.Errorf("nil input should return empty, got %d", len(out))
	}
	single := []TssCommitment{{KeyId: "k", BlockHeight: 1, Type: "blame", TxId: "x"}}
	out := DedupCommitmentsBySemanticKey(single)
	if len(out) != 1 || out[0].TxId != "x" {
		t.Errorf("singleton input should round-trip, got %+v", out)
	}
}
