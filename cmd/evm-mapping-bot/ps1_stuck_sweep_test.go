package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func resetStuckSweeps(t *testing.T) {
	t.Helper()
	stuckSweeps.Lock()
	stuckSweeps.versions, stuckSweeps.refused = map[string]int{}, map[string]string{}
	stuckSweeps.Unlock()
	t.Cleanup(func() {
		stuckSweeps.Lock()
		stuckSweeps.versions, stuckSweeps.refused = map[string]int{}, map[string]string{}
		stuckSweeps.Unlock()
	})
}

// PS-1 (11-04): a sweep unmined for a reason a fee does not fix is re-driven a
// version at a time until the bridge refuses past MaxSweepVersions, and then
// dry-run every two minutes; only the log said so. /health now names a sweep on
// its eighth version or later, and one whose re-drive the bridge refused, until
// the sweep is confirmed (no longer open).
func TestPS1_StuckSweepIsAnAlarm(t *testing.T) {
	resetStuckSweeps(t)
	noteSweepVersions("0-1", stuckSweepVersions-1)
	if a := stuckSweepAlarms(); len(a) != 0 {
		t.Fatalf("a sweep on version %d alarmed: %v", stuckSweepVersions-1, a)
	}
	noteSweepVersions("0-1", stuckSweepVersions)
	if a := stuckSweepAlarms(); len(a) != 1 || !strings.Contains(a[0], "sweep 0-1 is on its version 8") {
		t.Fatalf("version 8: %v", a)
	}
	// The bridge refuses the re-drive (the cap).
	_, _, view, _ := i26World(t)
	view, _ = decodeVaultView("vsc1bridge", hex.EncodeToString(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive})))
	succ := common.HexToAddress(rotAddr(2))
	unsigned := rotUnsigned(succ, big.NewInt(5), nil)
	sh, _ := computeSighash(unsigned)
	rec := sweepRecord{Gen: 0, Nonce: 7, Asset: "eth", BuiltL1: 10, Versions: []sweepVersion{{Unsigned: unsigned}}}
	l1, l2 := newFakeL1(), newFakeL2()
	l2.sigs[sh] = tssSigWith(t, rotKey(1), unsigned)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "100")
	refuse := true
	l2.sim = func(action string) map[string]any {
		if action == "redriveSweep" && refuse {
			return map[string]any{"success": false, "err": "ErrTransaction", "err_msg": "sweep 0-7 has 64 versions (MaxSweepVersions): no more re-drives", "rc_used": int64(300)}
		}
		return map[string]any{"success": true, "ret": "ok", "rc_used": int64(900)}
	}
	rpc, gql := l1.rpc(t), l2.gql(t)
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}}
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	cp := &Checkpoint{SentSweeps: map[string]SentSweep{}}
	driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, false) // signed + broadcast
	driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, true)  // unmined, past the window: re-drive refused
	a := stuckSweepAlarms()
	var refusedAlarm string
	for _, s := range a {
		if strings.Contains(s, "sweep 0-7 cannot be re-driven") {
			refusedAlarm = s
		}
	}
	if refusedAlarm == "" || !strings.Contains(refusedAlarm, "MaxSweepVersions") {
		t.Fatalf("no refused re-drive alarm naming the bridge's reason: %v", a)
	}
	// It shows on /health.
	_, _, hgql, hrpc := healthWorld(t, 10)
	h := newBotHealth(12 * time.Second)
	now := time.Now()
	h.recordTick(now, nil, emptyCP())
	h.check(context.Background(), now, hgql, hrpc, "vsc1bridge", []string{"vsc1bridge-primary"}, "did:bot", []string{healthVault}, nil, 0, nil)
	if r := h.report(now); !hasIssue(r, "PS-1: sweep 0-7 cannot be re-driven") {
		t.Fatalf("/health does not name the refused sweep: %v", r.Issues)
	}
	// A re-drive that goes through clears the refusal.
	refuse = false
	vaultOpMu.Lock()
	lastVaultOpAt = map[string]time.Time{} // the re-drive is rate-limited per sweep
	vaultOpMu.Unlock()
	driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, true)
	for _, s := range stuckSweepAlarms() {
		if strings.Contains(s, "0-7 cannot be re-driven") {
			t.Fatalf("refusal kept after a re-drive went through: %s", s)
		}
	}
	// Confirmed (no longer open): every alarm for it goes.
	noteSweepVersions("0-7", 64)
	pruneStuckSweeps([]string{"0-1"})
	for _, s := range stuckSweepAlarms() {
		if strings.Contains(s, "0-7") {
			t.Fatalf("a confirmed sweep still alarms: %s", s)
		}
	}
	pruneStuckSweeps(nil)
	if a := stuckSweepAlarms(); len(a) != 0 {
		t.Fatalf("alarms after every sweep confirmed: %v", a)
	}
}
