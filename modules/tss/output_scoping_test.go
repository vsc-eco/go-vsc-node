package tss

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"vsc-node/lib/btcvault"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/tinylib/msgp/msgp"
)

const scopeContract = "vsc1btcvaultcontractxxxxxxxxxxxxxxxxx"

func scopePub(seed byte) []byte {
	b := make([]byte, 32)
	b[31] = seed
	_, pub := btcec.PrivKeyFromBytes(b)
	return pub.SerializeCompressed()
}

func marshalVaultEntry(v btcvault.Vault) []byte {
	buf := make([]byte, btcvault.VaultEntrySize)
	binary.BigEndian.PutUint32(buf[0:], v.Generation)
	copy(buf[4:37], v.Primary)
	copy(buf[37:70], v.Backup)
	buf[70] = byte(v.Status)
	binary.BigEndian.PutUint32(buf[71:], v.Predecessor)
	binary.BigEndian.PutUint32(buf[75:], v.CreatedHeight)
	binary.BigEndian.PutUint32(buf[79:], v.ActivatedHeight)
	binary.BigEndian.PutUint32(buf[83:], v.RetiredHeight)
	return buf
}

func marshalRegistry(vs ...btcvault.Vault) []byte {
	var b []byte
	for _, v := range vs {
		b = append(b, marshalVaultEntry(v)...)
	}
	return b
}

// encodeSigningData builds the exact msgp map the contract codec produces.
// withAmount toggles the S3.5 "am" field.
func encodeSigningData(tx []byte, index uint32, sigHash, ws []byte, amount int64, withAmount bool) []byte {
	var b []byte
	b = msgp.AppendMapHeader(b, 2)
	b = msgp.AppendString(b, "tx")
	b = msgp.AppendBytes(b, tx)
	b = msgp.AppendString(b, "uh")
	b = msgp.AppendArrayHeader(b, 1)
	nf := uint32(3)
	if withAmount {
		nf = 4
	}
	b = msgp.AppendMapHeader(b, nf)
	b = msgp.AppendString(b, "i")
	b = msgp.AppendUint32(b, index)
	b = msgp.AppendString(b, "hs")
	b = msgp.AppendBytes(b, sigHash)
	b = msgp.AppendString(b, "ws")
	b = msgp.AppendBytes(b, ws)
	if withAmount {
		b = msgp.AppendString(b, "am")
		b = msgp.AppendInt64(b, amount)
	}
	return b
}

// scopeFixture builds a mid-rotation vault: gen0 Retiring, gen1 Active
// (successor), plus a valid successor sweep spending a gen0 UTXO to the gen1
// P2WSH, and the btcScopeDeps a test drives.
type scopeFixture struct {
	deps         btcScopeDeps
	state        map[string][]byte
	gen0KeyId    string // retiring
	gen1KeyId    string // active successor
	sweepSigHash []byte // a gen0-input sighash of the valid successor sweep
	sweepTxBytes []byte
	validTxid    string
	retiringWS   []byte
	amount       int64
	retiringPub  []byte
	successorPub []byte
	backupPub    []byte
	net          *chaincfg.Params
}

func addPending(state map[string][]byte, txidHex string, blob []byte) {
	raw, _ := hex.DecodeString(txidHex)
	state["p"] = append(state["p"], raw...)
	state["d-"+txidHex] = blob
}

func buildSweep(t *testing.T, inputWS, payToPk []byte, amount int64, prevSeed byte) (txBytes, sigHash []byte, txid string) {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	var prev chainhash.Hash
	for i := range prev {
		prev[i] = byte(i) + prevSeed
	}
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: prev, Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(amount-500, payToPk))
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	txBytes = buf.Bytes()
	sh, err := btcvault.RecomputeSegwitV0Sighash(txBytes, 0, inputWS, amount)
	if err != nil {
		t.Fatalf("sighash: %v", err)
	}
	return txBytes, sh, tx.TxHash().String()
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	net := &chaincfg.MainNetParams
	csv := btcvault.CSVBlocksForNet(net)
	f := &scopeFixture{
		net:          net,
		retiringPub:  scopePub(11),
		successorPub: scopePub(12),
		backupPub:    scopePub(13),
		amount:       750000,
		gen0KeyId:    scopeContract + "-" + btcvault.VaultKeyName(0),
		gen1KeyId:    scopeContract + "-" + btcvault.VaultKeyName(1),
	}
	retiringWS, _, err := btcvault.SuccessorPkScriptRaw(f.retiringPub, f.backupPub, csv, net)
	if err != nil {
		t.Fatalf("retiring ws: %v", err)
	}
	_, successorPk, err := btcvault.SuccessorPkScriptRaw(f.successorPub, f.backupPub, csv, net)
	if err != nil {
		t.Fatalf("successor pk: %v", err)
	}
	f.retiringWS = retiringWS

	txBytes, sh, txid := buildSweep(t, retiringWS, successorPk, f.amount, 3)
	f.sweepTxBytes, f.sweepSigHash, f.validTxid = txBytes, sh, txid

	f.state = map[string][]byte{}
	f.state["v"] = marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: f.retiringPub, Backup: f.backupPub, Status: btcvault.VaultStatusRetiring, CreatedHeight: 1, ActivatedHeight: 2},
		btcvault.Vault{Generation: 1, Primary: f.successorPub, Backup: f.backupPub, Status: btcvault.VaultStatusActive, Predecessor: 0, CreatedHeight: 10, ActivatedHeight: 11},
	)
	activeGen := make([]byte, 4)
	binary.BigEndian.PutUint32(activeGen, 1)
	f.state["va"] = activeGen
	addPending(f.state, txid, encodeSigningData(txBytes, 0, sh, retiringWS, f.amount, true))

	f.deps = btcScopeDeps{
		contractId: scopeContract,
		mainnet:    true,
		readKey: func(cid string, bh uint64, key string) ([]byte, bool) {
			if cid != scopeContract {
				return nil, false
			}
			v, ok := f.state[key]
			return v, ok
		},
		findKey: func(id string) (tss_db.TssKey, error) {
			if id == f.gen1KeyId {
				return tss_db.TssKey{Id: id, Status: tss_db.TssKeyActive, PublicKey: hex.EncodeToString(f.successorPub)}, nil
			}
			return tss_db.TssKey{Id: id, Status: tss_db.TssKeyActive, PublicKey: hex.EncodeToString(f.retiringPub)}, nil
		},
	}
	return f
}

func TestEvaluateScope_SuccessorSweepAllowed(t *testing.T) {
	f := newScopeFixture(t)
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeSuccessorSweep {
		t.Fatalf("valid successor sweep: got %v, want scopeSuccessorSweep", got)
	}
}

func TestEvaluateScope_ActiveGenUnrestricted(t *testing.T) {
	f := newScopeFixture(t)
	if got := f.deps.evaluateScope(f.gen1KeyId, []byte{0xde, 0xad}, 100); got != scopeAllow {
		t.Fatalf("active-gen user withdrawal: got %v, want scopeAllow", got)
	}
}

func TestEvaluateScope_InertWhenNoRegistry(t *testing.T) {
	f := newScopeFixture(t)
	delete(f.state, "v")
	// Registry ABSENT (pre-fold): the legacy gen-0 "main" key signs as today.
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeAllow {
		t.Fatalf("no registry, gen-0 main (inert): got %v, want scopeAllow", got)
	}
	// But a higher-generation keyId cannot exist without a registry → fail closed.
	if got := f.deps.evaluateScope(f.gen1KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("no registry, mainv1: got %v, want scopeRefuse (cannot exist without a registry)", got)
	}
}

// TestEvaluateScope_UnresolvableRegistryFailsClosed — council B1/C-FS-1/A-F1: a
// PRESENT-but-corrupt vault registry must FAIL CLOSED (refuse), never fail open.
// A lying/compromised contract writing a malformed / 0-active / 2+-active "v"
// must NOT be able to disable output scoping for a retiring key.
func TestEvaluateScope_UnresolvableRegistryFailsClosed(t *testing.T) {
	// Two Active gens (ambiguous successor).
	f := newScopeFixture(t)
	f.state["v"] = marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: f.retiringPub, Backup: f.backupPub, Status: btcvault.VaultStatusActive},
		btcvault.Vault{Generation: 1, Primary: f.successorPub, Backup: f.backupPub, Status: btcvault.VaultStatusActive},
	)
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("two-active registry: got %v, want scopeRefuse (unresolvable, fail closed)", got)
	}
	// Zero Active gens (no successor to sweep to).
	f.state["v"] = marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: f.retiringPub, Backup: f.backupPub, Status: btcvault.VaultStatusRetiring},
	)
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("zero-active registry: got %v, want scopeRefuse", got)
	}
	// Malformed blob (length not a multiple of VaultEntrySize).
	f.state["v"] = make([]byte, btcvault.VaultEntrySize+7)
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("malformed registry: got %v, want scopeRefuse", got)
	}
	// A malformed registry must freeze the ACTIVE gen too (cannot classify) —
	// safe recoverable freeze, never a fail-open.
	if got := f.deps.evaluateScope(f.gen1KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("malformed registry, active-gen keyId: got %v, want scopeRefuse", got)
	}
}

// TestBtcSignGateDecision_V8 — the halt × scope decision, including the V-8
// exemption: a proven successor-scoped sweep issues even while the solvency
// halt FLAG is up (the honest evacuation must complete), while everything else
// freezes under the halt.
func TestBtcSignGateDecision_V8(t *testing.T) {
	cases := []struct {
		name    string
		verdict scopeVerdict
		halted  bool
		refuse  bool
	}{
		{"refuse, no halt", scopeRefuse, false, true},
		{"refuse, halt", scopeRefuse, true, true},
		{"allow, no halt", scopeAllow, false, false},
		{"allow, halt -> M1.1a freeze", scopeAllow, true, true},
		{"sweep, no halt", scopeSuccessorSweep, false, false},
		{"sweep, halt -> V-8 exempt, issue", scopeSuccessorSweep, true, false},
	}
	for _, tc := range cases {
		if got := btcSignGateDecision(tc.verdict, tc.halted); got != tc.refuse {
			t.Fatalf("%s: btcSignGateDecision(%v,%v)=%v want %v", tc.name, tc.verdict, tc.halted, got, tc.refuse)
		}
	}
}

func TestEvaluateScope_RetiringSighashNotAPendingSweep(t *testing.T) {
	f := newScopeFixture(t)
	if got := f.deps.evaluateScope(f.gen0KeyId, []byte{0x01, 0x02, 0x03}, 100); got != scopeRefuse {
		t.Fatalf("retiring sign of non-sweep digest: got %v, want scopeRefuse", got)
	}
}

func TestEvaluateScope_RetiringSweepToNonSuccessorRefused(t *testing.T) {
	f := newScopeFixture(t)
	// A sweep from gen0 that pays an ATTACKER script instead of the successor.
	_, attackerPk, _ := btcvault.SuccessorPkScriptRaw(scopePub(99), f.backupPub, btcvault.CSVBlocksForNet(f.net), f.net)
	txBytes, sh, txid := buildSweep(t, f.retiringWS, attackerPk, f.amount, 40)
	// Replace the fixture's pending set with ONLY this attacker sweep.
	f.state["p"] = nil
	delete(f.state, "d-"+f.validTxid)
	addPending(f.state, txid, encodeSigningData(txBytes, 0, sh, f.retiringWS, f.amount, true))
	if got := f.deps.evaluateScope(f.gen0KeyId, sh, 100); got != scopeRefuse {
		t.Fatalf("retiring sweep to non-successor: got %v, want scopeRefuse", got)
	}
}

func TestEvaluateScope_LyingTemplateRecomputeCatch(t *testing.T) {
	f := newScopeFixture(t)
	// Decoy: an attacker-paying tx stored with a LIE — its stored SigHash is the
	// valid successor-sweep digest (so the sighash match succeeds), but the tx's
	// REAL sighash differs. The independent recompute must catch the mismatch.
	_, attackerPk, _ := btcvault.SuccessorPkScriptRaw(scopePub(77), f.backupPub, btcvault.CSVBlocksForNet(f.net), f.net)
	tx := wire.NewMsgTx(wire.TxVersion)
	var prev chainhash.Hash
	prev[0] = 5
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: prev, Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(f.amount-500, attackerPk))
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	txBytes := buf.Bytes()
	txid := tx.TxHash().String()
	// Remove the valid sweep so only the decoy matches; stored SigHash = the lie.
	f.state["p"] = nil
	delete(f.state, "d-"+f.validTxid)
	addPending(f.state, txid, encodeSigningData(txBytes, 0, f.sweepSigHash, f.retiringWS, f.amount, true))
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("lying decoy template: got %v, want scopeRefuse", got)
	}
}

func TestEvaluateScope_MissingAmountFailsClosed(t *testing.T) {
	f := newScopeFixture(t)
	// Pre-S3.5 contract: SigningData without "am" → cannot recompute → fail closed.
	f.state["p"] = nil
	delete(f.state, "d-"+f.validTxid)
	addPending(f.state, f.validTxid, encodeSigningData(f.sweepTxBytes, 0, f.sweepSigHash, f.retiringWS, 0, false))
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("missing amount: got %v, want scopeRefuse (fail closed)", got)
	}
}

func TestEvaluateScope_UnknownAndNonFundHoldingGen(t *testing.T) {
	f := newScopeFixture(t)
	unknown := scopeContract + "-" + btcvault.VaultKeyName(9)
	if got := f.deps.evaluateScope(unknown, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("unknown gen: got %v, want scopeRefuse", got)
	}
	f.state["v"] = marshalRegistry(
		btcvault.Vault{Generation: 0, Primary: f.retiringPub, Backup: f.backupPub, Status: btcvault.VaultStatusInactive},
		btcvault.Vault{Generation: 1, Primary: f.successorPub, Backup: f.backupPub, Status: btcvault.VaultStatusActive},
	)
	if got := f.deps.evaluateScope(f.gen0KeyId, f.sweepSigHash, 100); got != scopeRefuse {
		t.Fatalf("inactive gen sign: got %v, want scopeRefuse", got)
	}
}

