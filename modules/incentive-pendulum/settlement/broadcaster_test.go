package settlement

import (
	"testing"

	"vsc-node/lib/hive"

	"github.com/vsc-eco/hivego"
)

// fakeCreator captures the operations passed through the broadcast lifecycle
// so tests can assert on the wire-form payload without standing up a Hive
// RPC. It implements just enough of hive.HiveTransactionCreator for the
// HiveBroadcaster.Broadcast call path.
type fakeCreator struct {
	hive.TransactionCrafter // free CustomJson + MakeTransaction + (Transfer|Update*) impls
	signed                  bool
	broadcastedID           string
	signCalled              int
	broadcastCalled         int
	captured                hivego.HiveTransaction
}

func (f *fakeCreator) PopulateSigningProps(tx *hivego.HiveTransaction, bh []int) error {
	tx.Expiration = "2030-01-01T00:00:01"
	tx.RefBlockNum = 1
	return nil
}

func (f *fakeCreator) Sign(tx hivego.HiveTransaction) (string, error) {
	f.signCalled++
	f.signed = true
	return "deadbeef", nil
}

func (f *fakeCreator) Broadcast(tx hivego.HiveTransaction) (string, error) {
	f.broadcastCalled++
	f.captured = tx
	f.broadcastedID = "tx-id-faked"
	return f.broadcastedID, nil
}

func TestHiveBroadcaster_EmitsCustomJsonOp(t *testing.T) {
	creator := &fakeCreator{}
	b := NewHiveBroadcaster(creator, "magi-leader")
	payload := BuildSettlementPayload(
		7, 6,
		[]SlashEntry{{Account: "hive:bad", Bps: 50}},
		[]DistributionEntry{{Account: "hive:good", HBDAmt: 1000}},
	)
	id, err := b.Broadcast(payload)
	if err != nil {
		t.Fatalf("broadcast failed: %v", err)
	}
	if id != "tx-id-faked" {
		t.Errorf("tx id mismatch: got %s", id)
	}
	if creator.signCalled != 1 || creator.broadcastCalled != 1 {
		t.Errorf("expected exactly one sign+broadcast, got sign=%d broadcast=%d", creator.signCalled, creator.broadcastCalled)
	}
	if len(creator.captured.Operations) != 1 {
		t.Fatalf("expected one op, got %d", len(creator.captured.Operations))
	}
	op := creator.captured.Operations[0]
	cj, ok := op.(hivego.CustomJsonOperation)
	if !ok {
		t.Fatalf("op type: got %T want CustomJsonOperation", op)
	}
	if cj.Id != "vsc.pendulum_settlement" {
		t.Errorf("op id: got %s want vsc.pendulum_settlement", cj.Id)
	}
	if len(cj.RequiredAuths) != 1 || cj.RequiredAuths[0] != "magi-leader" {
		t.Errorf("required_auths: got %v want [magi-leader]", cj.RequiredAuths)
	}
	if cj.Json == "" {
		t.Error("custom_json body should not be empty")
	}
}

func TestHiveBroadcaster_NilCreatorFailsCleanly(t *testing.T) {
	b := NewHiveBroadcaster(nil, "magi-leader")
	if _, err := b.Broadcast(SettlementPayload{Epoch: 1, PrevEpoch: 0}); err == nil {
		t.Fatal("expected error for nil creator")
	}
}

func TestHiveBroadcaster_EmptyUsernameFails(t *testing.T) {
	b := NewHiveBroadcaster(&fakeCreator{}, "")
	if _, err := b.Broadcast(SettlementPayload{Epoch: 1, PrevEpoch: 0}); err == nil {
		t.Fatal("expected error for empty hive username")
	}
}
