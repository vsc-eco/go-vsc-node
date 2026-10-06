package gqlgen

import (
	"context"
	"testing"

	"vsc-node/modules/common/params"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/gql/model"

	"go.mongodb.org/mongo-driver/mongo"
)

// fakeBalances / fakeRcs implement only what GetAccountRc reads; the embedded
// interfaces make any other call panic, so the test cannot pass by accident.
type fakeBalances struct {
	ledgerDb.Balances
	recs map[string]ledgerDb.BalanceRecord
}

func (f fakeBalances) GetBalanceRecord(account string, _ uint64) (*ledgerDb.BalanceRecord, error) {
	r, ok := f.recs[account]
	if !ok {
		return nil, mongo.ErrNoDocuments
	}
	return &r, nil
}

type fakeRcs struct {
	rcDb.RcDb
	recs map[string]rcDb.RcRecord
}

func (f fakeRcs) GetRecord(account string, _ uint64) (rcDb.RcRecord, error) {
	r, ok := f.recs[account]
	if !ok {
		return rcDb.RcRecord{}, mongo.ErrNoDocuments
	}
	return r, nil
}

// RC-GQL-1 (testnet 2026-10-06): a mapping-bot DID funded with 50 HBD but with no
// RC record yet read "0 RC" here while the RC system would have admitted its tx
// (available = HBD balance - frozen, frozen 0 without a record). The bot's
// pre-flight trusts this query, so it could never send its first transaction.
func TestGetAccountRc_FundedAccountWithoutRcRecord(t *testing.T) {
	const did = "did:pkh:eip155:1:0x014D5bb85E8829d751D97bC3E6Cc1A5074F00DFA"
	const hive = "hive:fresh"
	r := &queryResolver{&Resolver{
		Balances: fakeBalances{recs: map[string]ledgerDb.BalanceRecord{
			did:  {Account: did, HBD: 50000},
			hive: {Account: hive, HBD: 1000},
		}},
		Rc: fakeRcs{recs: map[string]rcDb.RcRecord{}},
	}}
	h := model.Uint64(7142840)

	got, err := r.GetAccountRc(context.Background(), did, &h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != 50000 || got.MaxRcs != 50000 {
		t.Errorf("funded DID without an RC record: amount %d max %d, want 50000/50000", got.Amount, got.MaxRcs)
	}

	got, err = r.GetAccountRc(context.Background(), hive, &h)
	if err != nil {
		t.Fatal(err)
	}
	want := params.RC_HIVE_FREE_AMOUNT + 1000
	if got.Amount != want || got.MaxRcs != want {
		t.Errorf("hive account without an RC record: amount %d max %d, want %d", got.Amount, got.MaxRcs, want)
	}
}

// Unchanged paths: no balance at all reports only the free allowance, and an
// account with an RC record still subtracts what is frozen.
func TestGetAccountRc_UnchangedPaths(t *testing.T) {
	const did = "did:pkh:eip155:1:0x0000000000000000000000000000000000000001"
	const spent = "did:pkh:eip155:1:0x0000000000000000000000000000000000000002"
	h := model.Uint64(1000)
	r := &queryResolver{&Resolver{
		Balances: fakeBalances{recs: map[string]ledgerDb.BalanceRecord{spent: {Account: spent, HBD: 50000}}},
		Rc:       fakeRcs{recs: map[string]rcDb.RcRecord{spent: {Account: spent, Amount: 100, BlockHeight: 1000}}},
	}}

	got, err := r.GetAccountRc(context.Background(), did, &h)
	if err != nil || got.Amount != 0 || got.MaxRcs != 0 {
		t.Errorf("no balance: %+v %v", got, err)
	}
	got, err = r.GetAccountRc(context.Background(), spent, &h)
	if err != nil || got.Amount != 49900 || got.MaxRcs != 50000 {
		t.Errorf("with an RC record spent at this height: %+v %v (want 49900/50000)", got, err)
	}
}
