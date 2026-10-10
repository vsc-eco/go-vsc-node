package state_engine

import (
	"testing"

	"vsc-node/modules/db/vsc/transactions"
)

// Re-processing the block that included a transaction (a restart before the
// next block's oplog confirmed it) must run it again; a different block listing
// it, or a transaction already confirmed or failed, must not.
func TestIncludedByThisBlock(t *testing.T) {
	block := "l1-produce-block-tx"
	other := "l1-other-tx"
	rec := func(status transactions.TransactionStatus, anchor *string) *transactions.TransactionRecord {
		return &transactions.TransactionRecord{Status: status, AnchoredId: anchor}
	}
	cases := []struct {
		name string
		rec  *transactions.TransactionRecord
		want bool
	}{
		{"included by this block (restart)", rec(transactions.TransactionStatusIncluded, &block), true},
		{"included by another block (re-list or re-post)", rec(transactions.TransactionStatusIncluded, &other), false},
		{"included, no anchor", rec(transactions.TransactionStatusIncluded, nil), false},
		{"confirmed", rec(transactions.TransactionStatusConfirmed, &block), false},
		{"failed", rec(transactions.TransactionStatusFailed, &block), false},
	}
	for _, c := range cases {
		if got := includedByThisBlock(c.rec, block); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
