package oracle

import (
	"encoding/json"
	"errors"
	"testing"

	"vsc-node/lib/hive"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/chain"

	"github.com/vsc-eco/hivego"
)

// N4: a witness votes only when what it saw differs from the current halt,
// so a healthy chain sends nothing; the payload is what the tally reads.
func TestN4_VoteOnlyOnDisagreement(t *testing.T) {
	bad := chain.SolvencyObservation{Insolvent: true, MissingSats: 250_000_000, Missing: 1, Tip: 900_002, Digest: "ab"}
	good := chain.SolvencyObservation{Tip: 900_002}
	if _, send := btcSolvencyVoteFor(200, good, false); send {
		t.Fatal("solvent and not halted: a vote was sent")
	}
	if _, send := btcSolvencyVoteFor(200, bad, true); send {
		t.Fatal("insolvent and already halted: a vote was sent")
	}
	v, send := btcSolvencyVoteFor(200, bad, false)
	var got struct {
		C         uint64 `json:"c"`
		Insolvent bool   `json:"insolvent"`
		Missing   int64  `json:"missing_sats"`
	}
	if !send || json.Unmarshal([]byte(v), &got) != nil || got.C != 200 || !got.Insolvent || got.Missing != 250_000_000 {
		t.Fatalf("insolvent vote %q send=%v", v, send)
	}
	v, send = btcSolvencyVoteFor(400, good, true)
	if !send || json.Unmarshal([]byte(v), &got) != nil || got.C != 400 || got.Insolvent {
		t.Fatalf("solvent vote while halted %q send=%v", v, send)
	}
}

type fakeHiveCreator struct {
	hive.HiveTransactionCreator
	sent    []hivego.HiveTransaction
	signErr error
}

func (f *fakeHiveCreator) MakeTransaction(ops []hivego.HiveOperation) hivego.HiveTransaction {
	return hivego.HiveTransaction{Operations: ops}
}
func (f *fakeHiveCreator) PopulateSigningProps(*hivego.HiveTransaction, []int) error { return nil }
func (f *fakeHiveCreator) Sign(hivego.HiveTransaction) (string, error)               { return "sig", f.signErr }
func (f *fakeHiveCreator) Broadcast(tx hivego.HiveTransaction) (string, error) {
	f.sent = append(f.sent, tx)
	return "txid", nil
}

// The vote is one custom_json "vsc.btc_solvency" from the witness's own
// account with active authority, signed; a signing failure sends nothing.
func TestN4_VoteBroadcast(t *testing.T) {
	fh := &fakeHiveCreator{}
	o := &Oracle{conf: common.NewIdentityConfig(), logger: vsclog.Module("oracle-test"), hiveCreator: fh}
	o.broadcastBtcSolvencyVote(200, `{"c":200,"insolvent":true}`)
	if len(fh.sent) != 1 || len(fh.sent[0].Operations) != 1 {
		t.Fatalf("sent %d transactions", len(fh.sent))
	}
	op, ok := fh.sent[0].Operations[0].(hivego.CustomJsonOperation)
	if !ok || op.Id != "vsc.btc_solvency" || len(op.RequiredAuths) != 1 || len(op.RequiredPostingAuths) != 0 || op.Json != `{"c":200,"insolvent":true}` {
		t.Fatalf("operation %+v", fh.sent[0].Operations[0])
	}
	if len(fh.sent[0].Signatures) != 1 {
		t.Fatalf("the vote was not signed: %+v", fh.sent[0].Signatures)
	}
	fh.signErr = errors.New("no key")
	o.broadcastBtcSolvencyVote(400, `{"c":400}`)
	if len(fh.sent) != 1 {
		t.Fatal("an unsigned vote was broadcast")
	}
}
