package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// the contract's refusal when its pricing header is older than MaxPricingHeaderAge
// (account-mapping handlers.go pricingHeader), as the dry run reports it
var errStaleHeaderRefusal = errors.New("refused in dry run: migrateVault contract_not_initialized: newest L1 header is 9411s old: withdrawals wait until the header feed catches up")

func resetSweepStaleHeader() {
	sweepStaleHeader.Lock()
	sweepStaleHeader.at = nil
	sweepStaleHeader.Unlock()
}

// EVM-HDRFEED-2: a rotation op refused on a stale pricing header must make
// writeNeedsFile request a proof (migrateVault, redriveSweep and writeOffDust
// are not in the withdrawal queue, so the queued/in-flight trigger never covers
// them). noteSweepStaleHeader latches that per op; sweepStaleHeaderWants
// reports it within the retry window and an op going through clears its entry.
func TestEVMHdrFeed2_SweepStaleHeaderWantsProof(t *testing.T) {
	resetSweepStaleHeader()
	defer resetSweepStaleHeader()

	now := time.Now()
	if sweepStaleHeaderWants(now) {
		t.Fatal("no refusal latched yet: must not want a proof")
	}
	noteSweepStaleHeader("migrateVault", errStaleHeaderRefusal)
	if !sweepStaleHeaderWants(time.Now()) {
		t.Fatal("stale-header refusal latched: must want a proof")
	}
	noteSweepStaleHeader("migrateVault", nil) // a migrate going through clears it
	if sweepStaleHeaderWants(time.Now()) {
		t.Fatal("migrate succeeded: must no longer want a proof")
	}
	// an unrelated refusal must NOT latch (only the stale-header marker does)
	noteSweepStaleHeader("migrateVault", errors.New("send eth_proof (an account proof of the retiring address)"))
	if sweepStaleHeaderWants(time.Now()) {
		t.Fatal("eth_proof signal is not a stale-header refusal: must not want a proof")
	}
	// a noop (errVaultOpNoop wraps the contract's "nothing to migrate") clears an active latch
	noteSweepStaleHeader("migrateVault", errStaleHeaderRefusal)
	noteSweepStaleHeader("migrateVault", errVaultOpNoop)
	if sweepStaleHeaderWants(time.Now()) {
		t.Fatal("a noop (nothing to migrate) must clear the latch")
	}
	// a latch older than the retry window lapses (the driver re-latches it every
	// cycle while the op stays blocked, so an old one means it stopped being retried)
	noteSweepStaleHeader("migrateVault", errStaleHeaderRefusal)
	sweepStaleHeader.Lock()
	sweepStaleHeader.at["migrateVault"] = now.Add(-rotationRCAlarmFor - time.Minute)
	sweepStaleHeader.Unlock()
	if sweepStaleHeaderWants(time.Now()) {
		t.Fatal("latch older than the retry window must lapse")
	}
}

// The ops keep separate entries: after the last sweep settles, migrateVault is a
// noop ("nothing to migrate" comes before the contract reads the header) while
// writeOffDust is still refused on the stale header. The migrate noop must not
// drop the write-off's proof request, or the generation's dust, its retire and
// its purge wait through every idle window.
func TestEVMHdrFeed2_OneOpGoingThroughKeepsAnotherOpsRequest(t *testing.T) {
	resetSweepStaleHeader()
	defer resetSweepStaleHeader()

	noteSweepStaleHeader("writeOffDust", errStaleHeaderRefusal)
	noteSweepStaleHeader("migrateVault", errVaultOpNoop)
	if !sweepStaleHeaderWants(time.Now()) {
		t.Fatal("writeOffDust is still blocked on the stale header: a migrate noop must not clear its request")
	}
	noteSweepStaleHeader("redrive-0-7", errStaleHeaderRefusal)
	noteSweepStaleHeader("writeOffDust", nil)
	if !sweepStaleHeaderWants(time.Now()) {
		t.Fatal("the re-drive of 0-7 is still blocked: the write-off going through must not clear it")
	}
	noteSweepStaleHeader("redrive-0-7", nil)
	if sweepStaleHeaderWants(time.Now()) {
		t.Fatal("every op went through: must no longer want a proof")
	}
}

// End to end through the needs file the operator reads: no withdrawal is queued
// or in flight, so before the fix nothing asked for a proof while a rotation op
// sat refused on the stale header.
func TestEVMHdrFeed2_NeedsFileAsksForAProofWhileARotationOpIsBlocked(t *testing.T) {
	resetSweepStaleHeader()
	defer resetSweepStaleHeader()
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1bridge", "chainid", "11155111")
	gql := l2.gql(t)
	path := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", path)
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	read := func() (bool, []string) {
		var m struct {
			Want    bool     `json:"want_proof"`
			Reasons []string `json:"reasons"`
		}
		b, _ := os.ReadFile(path)
		_ = json.Unmarshal(b, &m)
		return m.Want, m.Reasons
	}
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{}}
	ctx := context.Background()

	writeNeedsFile(ctx, gql, cp, cfg, 1050, false)
	if want, r := read(); want {
		t.Fatalf("nothing blocked: asked for a proof %v", r)
	}
	noteSweepStaleHeader("writeOffDust", errStaleHeaderRefusal)
	writeNeedsFile(ctx, gql, cp, cfg, 1050, false)
	if want, r := read(); !want || len(r) != 1 || !strings.Contains(r[0], "rotation op waits on a stale pricing header") {
		t.Fatalf("writeOffDust blocked on the stale header: want_proof %v %v, want one rotation-op reason", want, r)
	}
	noteSweepStaleHeader("writeOffDust", nil)
	writeNeedsFile(ctx, gql, cp, cfg, 1050, false)
	if want, r := read(); want {
		t.Fatalf("the write-off went through: still asking for a proof %v", r)
	}
}

// The ops really feed the latch: writeOffDust and a sweep re-drive refused on
// the stale header (dry run) each leave a proof request behind. Calling
// noteSweepStaleHeader directly, as the tests above do, would not notice an op
// that stopped calling it.
func TestEVMHdrFeed2_WriteOffDustAndRedriveRefusalsRequestAProof(t *testing.T) {
	stale := func(string) map[string]any {
		return map[string]any{"success": false, "err": "contract_not_initialized", "err_msg": "newest L1 header is 9411s old: withdrawals wait until the header feed catches up"}
	}
	t.Run("writeOffDust", func(t *testing.T) {
		resetSweepStaleHeader()
		defer resetSweepStaleHeader()
		l2 := newFakeL2()
		l2.sim = stale
		botKey, _ := ethCrypto.GenerateKey()
		cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
		sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: l2.gql(t), cfg: cfg}
		writeOffDust(context.Background(), l2.gql(t), sub, cfg)
		if !sweepStaleHeaderWants(time.Now()) {
			t.Fatal("writeOffDust refused on the stale header left no proof request")
		}
	})
	t.Run("redriveSweep", func(t *testing.T) {
		resetSweepStaleHeader()
		defer resetSweepStaleHeader()
		resetOps := func() {
			vaultOpMu.Lock()
			notOperatorAt, lastVaultOpAt = time.Time{}, map[string]time.Time{}
			vaultOpMu.Unlock()
			noteSweepRedrive("0-7", nil) // the refused re-drive is also a PS-1 health issue: leave none behind
		}
		resetOps()
		t.Cleanup(resetOps)
		w := newSweepRestartWorld(t)
		cp := &Checkpoint{SentSweeps: map[string]SentSweep{}}
		w.drive(cp) // broadcast and recorded; not mined, so a re-drive is due (verifier h 100 >= built 10 + 33)
		if len(cp.SentSweeps["0-7"].TxHashes) != 1 {
			t.Fatalf("precondition: the sweep was not recorded: %+v", cp.SentSweeps["0-7"])
		}
		ok := w.l2.sim
		w.l2.sim = func(action string) map[string]any {
			if action == "redriveSweep" {
				return stale(action)
			}
			return ok(action)
		}
		w.driveRedrive(cp)
		if !sweepStaleHeaderWants(time.Now()) {
			t.Fatal("redriveSweep refused on the stale header left no proof request")
		}
	})
}
