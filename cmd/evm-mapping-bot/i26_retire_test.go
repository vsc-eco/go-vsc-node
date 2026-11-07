package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// i26World is a bridge whose gen 0 is Inactive (its purge grace passed) and
// gen 1 Active, with the verifier's stored L1 height at 12061029 (testnet
// 11-04 09:10). retireVault's dry run purges gen 0; migrateVault has nothing.
func i26World(t *testing.T) (*fakeL2, *l2Submitter, vaultView, func() int) {
	t.Helper()
	oldNow := vaultOpNow
	cur := time.Unix(1_000_000, 0)
	vaultOpNow = func() time.Time { return cur }
	reset := func() {
		vaultOpMu.Lock()
		notOperatorAt, lastVaultOpAt = time.Time{}, map[string]time.Time{}
		vaultOpMu.Unlock()
	}
	reset()
	t.Cleanup(func() { vaultOpNow = oldNow; reset() })
	l2 := newFakeL2()
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: l2.gql(t),
		cfg: EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}}
	l2.sim = func(action string) map[string]any {
		ret := map[string]string{"retireVault": "retire: purged=0", "migrateVault": "nothing to migrate"}[action]
		return map[string]any{"success": true, "ret": ret, "rc_used": 865}
	}
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "12061029")
	// Gen 1's MagiDeposit is attested (I-28), as on any running bridge: the
	// rotation pass's attestation step then reads only this and needs no L1.
	l2.set("vsc1bridge", depositAttestedKey(1), "12061029")
	view := vaultView{Registry: true, Gens: []vaultGen{
		{Gen: 0, Status: btcvault.VaultStatusInactive, Address: rotAddr(1)},
		{Gen: 1, Status: btcvault.VaultStatusActive, Address: rotAddr(2)},
	}}
	view.Active = &view.Gens[1]
	retires := func() int {
		l2.mu.Lock()
		defer l2.mu.Unlock()
		n := len(l2.submitted)
		reset() // each pass may retire again
		return n
	}
	return l2, sub, view, retires
}

func i26Drive(sub *l2Submitter, cp *Checkpoint, view vaultView) {
	driveVaultRotation(context.Background(), nil, sub.gql, sub, cp, sub.cfg, view)
}

// I-26, testnet 11-04: the 0.02 ETH deposit to the Inactive gen 0 was parked
// for its header when the proof landed that crossed the purge grace; the bot
// sent retireVault at once (09:10:12), gen 0 was purged, the map 39 s later was
// refused and the ETH was stranded. retireVault now waits for that deposit.
func TestI26_ParkedDepositToAnInactiveGenerationHoldsTheRetire(t *testing.T) {
	_, sub, view, submitted := i26World(t)
	dep := detectedDeposit{BlockHeight: 12060988, TxHash: "0xb9b3", LogIndex: -1, DepositType: "eth", Vault: rotAddr(1)}
	cp := &Checkpoint{LastScannedBlock: 12061040, WaitingDeposits: map[uint64][]detectedDeposit{12060988: {dep}}, PendingMaps: map[string]PendingMap{}}
	i26Drive(sub, cp, view)
	if n := submitted(); n != 0 {
		t.Fatalf("%d calls submitted with a deposit to the Inactive gen 0 parked: retireVault would purge it before its map", n)
	}
	// its map is submitted: still no retire until the map has a result
	delete(cp.WaitingDeposits, 12060988)
	cp.PendingMaps["bafymap"] = PendingMap{Block: 12060988, DepositTx: "0xb9b3", Vault: rotAddr(1)}
	i26Drive(sub, cp, view)
	if n := submitted(); n != 0 {
		t.Fatalf("%d calls submitted while the deposit's map is in flight", n)
	}
	// the map confirmed: the deposit is booked and the retire may go
	delete(cp.PendingMaps, "bafymap")
	i26Drive(sub, cp, view)
	if n := submitted(); n != 1 {
		t.Fatalf("%d calls submitted once the deposit is mapped, want the retireVault", n)
	}
}

func TestI26_TheGateHoldsOnlyWhatThePurgeCouldLose(t *testing.T) {
	for _, c := range []struct {
		name    string
		vault   string // the parked deposit's
		scanned uint64
		status  btcvault.VaultStatus // gen 0's
		retire  bool
	}{
		{"deposit to the Active generation", rotAddr(2), 12061040, btcvault.VaultStatusInactive, true},
		{"deposit from a checkpoint before the Vault field", "", 12061040, btcvault.VaultStatusInactive, false},
		{"deposit to a Draining generation (this call cannot purge it)", rotAddr(1), 12061040, btcvault.VaultStatusDraining, true},
		{"scan behind the stored L1 height", rotAddr(2), 12061028, btcvault.VaultStatusInactive, false},
		{"scan at the stored L1 height", rotAddr(2), 12061029, btcvault.VaultStatusInactive, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, sub, view, submitted := i26World(t)
			view.Gens[0].Status = c.status
			dep := detectedDeposit{BlockHeight: 12060988, TxHash: "0xb9b3", LogIndex: -1, DepositType: "eth", Vault: c.vault}
			cp := &Checkpoint{LastScannedBlock: c.scanned, WaitingDeposits: map[uint64][]detectedDeposit{12060988: {dep}}, PendingMaps: map[string]PendingMap{}}
			i26Drive(sub, cp, view)
			if got := submitted() == 1; got != c.retire {
				t.Fatalf("retireVault submitted = %v, want %v", got, c.retire)
			}
		})
	}
}

// An unreadable stored height waits: the gate cannot tell whether the scan has
// seen every block the contract counts.
func TestI26_UnreadableStoredHeightWaits(t *testing.T) {
	l2, sub, view, submitted := i26World(t)
	l2.mu.Lock()
	l2.failState = map[string]bool{"vsc1verifier": true}
	l2.mu.Unlock()
	cp := &Checkpoint{LastScannedBlock: 12061040, PendingMaps: map[string]PendingMap{}}
	i26Drive(sub, cp, view)
	if n := submitted(); n != 0 {
		t.Fatalf("%d calls submitted with the stored L1 height unreadable", n)
	}
}

// I-26 end to end, the live order: the tick scans and maps (runOnce), then the
// rotation driver runs. A deposit to the Inactive gen 0 whose map is
// submitted but has no result yet holds retireVault; once the map confirms,
// the retire goes out.
func TestI26_LiveOrderMapThenRetire(t *testing.T) {
	_, _, view, _ := i26World(t) // vault-op clock + timers reset, the registry
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 102)
	l2.sim = func(action string) map[string]any {
		ret := map[string]string{"retireVault": "retire: purged=0", "migrateVault": "nothing to migrate"}[action]
		return map[string]any{"success": true, "ret": ret, "rc_used": 865}
	}
	gen0 := strings.ToLower(vault.Hex())
	view.Gens[0].Address = gen0
	cfg.Vaults = []string{gen0, view.Gens[1].Address}
	l2.status = func(string) string { return "UNCONFIRMED" }
	dep := ethDeposit(t, l1.block(101), vault, big.NewInt(2e16), nil) // the 0.02 ETH
	l1.block(102)                                                     // finalized = the stored L1 height
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	var pm PendingMap
	for _, p := range cp.PendingMaps {
		pm = p
	}
	if len(cp.PendingMaps) != 1 || pm.DepositTx != dep.Hash().Hex() || pm.Vault != gen0 {
		t.Fatalf("pending maps %+v, want the deposit's map naming vault %s", cp.PendingMaps, gen0)
	}
	submitted := func() int {
		l2.mu.Lock()
		defer l2.mu.Unlock()
		vaultOpMu.Lock()
		notOperatorAt, lastVaultOpAt = time.Time{}, map[string]time.Time{}
		vaultOpMu.Unlock()
		return len(l2.submitted)
	}
	before := submitted()
	driveVaultRotation(context.Background(), rpc, gql, sub, cp, cfg, view)
	if n := submitted(); n != before {
		t.Fatalf("%d calls submitted while the map to the Inactive gen 0 is in flight: the purge would refuse it", n-before)
	}
	l2.status = nil // the map confirmed
	resolvePendingMaps(context.Background(), gql, cp)
	driveVaultRotation(context.Background(), rpc, gql, sub, cp, cfg, view)
	if n := submitted(); n != before+1 {
		t.Fatalf("%d calls submitted after the map confirmed, want the retireVault", n-before)
	}
}

// A map in flight to the Active generation does not hold the retire (a bridge
// with steady deposits would otherwise never purge, and the purged
// generation's committee bonds stay locked).
func TestI26_MapToTheActiveGenerationDoesNotHoldTheRetire(t *testing.T) {
	_, sub, view, submitted := i26World(t)
	cp := &Checkpoint{LastScannedBlock: 12061040, PendingMaps: map[string]PendingMap{
		"bafymap": {Block: 12061030, DepositTx: "0xa", Vault: rotAddr(2)},
	}}
	i26Drive(sub, cp, view)
	if n := submitted(); n != 1 {
		t.Fatalf("%d calls submitted, want the retireVault: a map to the Active generation cannot be purged", n)
	}
}

// PS-2 (BTC bot parity: an unreadable sweep status skips the re-drive): a
// sweep whose receipt read failed may have mined; re-driving it makes a version
// that can never mine (TSS signature, RC and booked gas for nothing). With the
// receipt simply absent (unmined), the re-drive still goes.
func TestPS2_NoRedriveWhenAReceiptIsUnreadable(t *testing.T) {
	oldPauses := callRetryPauses
	callRetryPauses = nil
	t.Cleanup(func() { callRetryPauses = oldPauses })
	_, _, view, _ := i26World(t) // vault-op clock + timers reset
	succ := common.HexToAddress(rotAddr(2))
	unsigned := rotUnsigned(succ, big.NewInt(5), nil)
	sh, _ := computeSighash(unsigned)
	view, _ = decodeVaultView("vsc1bridge", hex.EncodeToString(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive})))
	rec := sweepRecord{Gen: 0, Nonce: 7, Asset: "eth", BuiltL1: 10, Versions: []sweepVersion{{Unsigned: unsigned}}}
	l1, l2 := newFakeL1(), newFakeL2()
	l2.sigs[sh] = tssSigWith(t, rotKey(1), unsigned)
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", "100")
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": true, "ret": "re-drove sweep 0-7", "rc_used": int64(900)}
	}
	rpc, gql := l1.rpc(t), l2.gql(t)
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}}
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	cp := &Checkpoint{SentSweeps: map[string]SentSweep{}}
	driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, false) // signed + broadcast
	redrives := func() int {
		n := 0
		for _, c := range submittedCalls(t, l2) {
			if c.Action == "redriveSweep" {
				n++
			}
		}
		return n
	}
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	t.Cleanup(busy.Close)
	driveSweep(context.Background(), newEthRPC(busy.URL), gql, sub, cp, cfg, view, rec, true)
	if n := redrives(); n != 0 {
		t.Fatalf("%d re-drives while the receipt read failed (429): the sweep may have mined", n)
	}
	driveSweep(context.Background(), rpc, gql, sub, cp, cfg, view, rec, true)
	if n := redrives(); n != 1 {
		t.Fatalf("%d re-drives with no receipt (unmined, past the window), want 1", n)
	}
}
