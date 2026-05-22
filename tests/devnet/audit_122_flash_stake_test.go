package devnet

import (
	"context"
	"testing"
	"time"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/incentive-pendulum/settlement"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestAuditFix_122_FlashStakeFilteredByTWAB is the end-to-end validation of
// audit #122 against the real production Mongo schema. Pre-fix
// ReadCommitteeBonds did a single point-in-time read at slotHeight, so an
// account flash-staking 1,000,000 HIVE_CONSENSUS one block before the
// settlement slot got the full pro-rata distribution weight. Post-fix the
// reader samples bondSampleCount points across (epochStartBh, slotHeight]
// and returns min(HIVE_CONSENSUS) per account, so flash-stakers are
// omitted entirely (min=0).
//
// This test injects two balance rows into the real devnet Mongo using
// the production BSON layout (the same fields ledger.balances writes),
// then drives the production ReadCommitteeBonds through a thin
// mongo-backed reader. If the BSON shape changes upstream the test
// fails loudly; if the TWAB logic regresses, the assertion at the end
// flips.
//
// The devnet itself is only used here for its Mongo (and for the
// guarantee that the schema matches what production writes). No
// settlement is exercised — the bond_reader semantics are isolated.
func TestAuditFix_122_FlashStakeFilteredByTWAB(t *testing.T) {
	requireDocker(t)

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SkipFunding = true
	cfg.LogLevel = "error"
	d, ctx := startDevnetNoKey(t, cfg, 15*time.Minute)
	_ = ctx

	// Choose a deterministic far-future block window so we don't collide
	// with any rows the running devnet writes itself.
	const (
		flashAccount = "hive:audit-122-flash-staker"
		epochStartBh = uint64(1_000_000)
		slotHeight   = uint64(1_000_100)
		flashBlock   = slotHeight - 1 // 1 block of dwell time
		flashAmt     = int64(1_000_000)
	)

	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(d.MongoURI()))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("magi-1").Collection("ledger_balances")

	// Cleanup any rows from a prior failed run.
	_, _ = coll.DeleteMany(context.Background(), bson.M{"account": flashAccount})
	t.Cleanup(func() {
		_, _ = coll.DeleteMany(context.Background(), bson.M{"account": flashAccount})
	})

	// Pre-flash row: zero bond at the very start of the window.
	prefl := ledgerDb.BalanceRecord{
		Account:        flashAccount,
		BlockHeight:    epochStartBh,
		HIVE_CONSENSUS: 0,
	}
	if _, err := coll.InsertOne(context.Background(), prefl); err != nil {
		t.Fatalf("insert pre-flash row: %v", err)
	}
	// Flash row: huge bond one block before slot end.
	flash := ledgerDb.BalanceRecord{
		Account:        flashAccount,
		BlockHeight:    flashBlock,
		HIVE_CONSENSUS: flashAmt,
	}
	if _, err := coll.InsertOne(context.Background(), flash); err != nil {
		t.Fatalf("insert flash row: %v", err)
	}

	reader := &mongoBalanceReader{coll: coll}

	// Sanity: the point-in-time read at slot end DOES see the flashed bond.
	// This confirms the BSON layout matches production and the data is
	// reachable — i.e., a pre-fix read would have credited the full amount.
	atSlot, err := reader.GetBalanceRecord(flashAccount, slotHeight)
	if err != nil {
		t.Fatalf("GetBalanceRecord at slot: %v", err)
	}
	if atSlot == nil || atSlot.HIVE_CONSENSUS != flashAmt {
		t.Fatalf("point-in-time read at slotHeight should see flashAmt=%d, got %+v", flashAmt, atSlot)
	}

	// Production path: TWAB-style min across (epochStartBh, slotHeight]
	// samples the pre-flash row at most window blocks and the flash row
	// only at slotHeight. min = 0 → account omitted from the bonds map.
	bonds := settlement.ReadCommitteeBonds(reader, []string{flashAccount}, epochStartBh, slotHeight)
	if got, present := bonds[flashAccount]; present {
		t.Fatalf("audit #122: flash-staker must be omitted (min=0), got bond=%d", got)
	}

	// Counter-case: with bond present at the start of the window, the
	// min-form TWAB credits the full amount. Confirms the test is
	// actually exercising the time dimension, not just always returning 0.
	flash2 := ledgerDb.BalanceRecord{
		Account:        flashAccount,
		BlockHeight:    epochStartBh, // overwrite pre-flash row with non-zero
		HIVE_CONSENSUS: flashAmt,
	}
	if _, err := coll.UpdateOne(
		context.Background(),
		bson.M{"account": flashAccount, "block_height": epochStartBh},
		bson.M{"$set": flash2},
	); err != nil {
		t.Fatalf("update pre-flash row: %v", err)
	}
	bonds = settlement.ReadCommitteeBonds(reader, []string{flashAccount}, epochStartBh, slotHeight)
	if got, present := bonds[flashAccount]; !present || got != flashAmt {
		t.Fatalf("audit #122: bonded-since-start should credit full amount, got bond=%d present=%v", got, present)
	}
}

// mongoBalanceReader implements settlement.BalanceRecordReader against a
// live mongo ledger_balances collection. The query mirrors the
// production impl in modules/db/vsc/ledger/ledger.go:GetBalanceRecord
// so this test fails loudly if the schema or read semantics drift.
type mongoBalanceReader struct {
	coll *mongo.Collection
}

func (r *mongoBalanceReader) GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error) {
	opts := options.FindOne().SetSort(bson.D{{Key: "block_height", Value: -1}})
	res := r.coll.FindOne(context.Background(), bson.M{
		"account":      account,
		"block_height": bson.M{"$lte": blockHeight},
	}, opts)
	if err := res.Err(); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	var rec ledgerDb.BalanceRecord
	if err := res.Decode(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}
