package devnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// TestVaultF11EmptyFeeReserve is failure-state F11 of the BTC vault-rotation-v2
// suite: "the fee reserve is empty when the rotation tries to drain".
//
// WHY THIS ONE MATTERS MOST FOR THE TESTNET BRING-UP
// A migration sweep pays its Bitcoin miner fee out of the contract's FeeSupply
// reserve, and that reserve is funded ONLY by an explicit topUpFeeReserve deposit
// to the ACTIVE generation's untagged vault address. Nobody has ever topped up the
// live testnet vault, so its FeeSupply is 0 today. That makes an empty fee reserve
// the very FIRST thing a real testnet rotation will hit, before any of the exotic
// failure states in this suite. This test is the rehearsal of that moment.
//
// WHAT THE CONTRACT DOES (btc-mapping-contract/contract/mapping/migration.go,
// HandleMigrateVault, the BRK-1 fee RESERVE CHECK)
// The FeeSupply debit is DEFERRED to confirmSpend, so migrateVault instead CHECKS at
// build time that the reserve covers every already-pending sweep's fee plus this
// one, and returns ErrBalance ("insufficient fee reserve to cover pending and
// current migration sweeps") when it does not. That check sits BEFORE
// signSpendTransaction, so an abort here leaves absolutely nothing behind: no "d-"
// signing record, no "p" pending-spend entry, no "ms-" sweep record, no "msl" index
// entry, no FeeSupply mutation, and crucially NOT the RETIRING to DRAINING status
// transition, which is written further down. The whole contract call reverts, so the
// retiring generation keeps every one of its UTXOs and stays fully recoverable.
//
// WHAT THIS TEST PROVES
//  1. F11-REFUSE: with FeeSupply at 0, migrateVault is REFUSED and the contract
//     state is byte-for-byte the state it was before the call. Specifically the
//     pending spend list "p", the vault registry "v", the migration sweep index
//     "msl", the UTXO registry "r" and the supply blob "s" are all unchanged, gen-0
//     still holds every UTXO it held, and gen-0 is still Retiring (status 2), not
//     Draining. This is the "no half-built sweep" property: a stalled rotation must
//     not leave a partially constructed spend behind.
//  2. F11-DRIVER: the real system is driven by the mapping-bot, which retries
//     migrateVault on a timer. There is no mapping-bot in the devnet, so the retry
//     loop is reproduced by hand as three migrateVault calls 10 seconds apart. After
//     every one of them the pending spend list must NOT have grown, the vault
//     registry must still be byte-equal to the pre-refusal snapshot, and gen-0 must
//     still be Retiring. A bot hammering a rotation it cannot fund must not be able
//     to accumulate junk state or leak the generation into Draining.
//  3. F11-RESUME: topping the reserve up (fundFeeReserve, 10,000,000 sats to the
//     gen-1 untagged address) makes the SAME rotation complete. The stall is a
//     recoverable pause, not a brick: migrateAndSettle drives the sweep through
//     signing, broadcast and confirmSpend, and gen-0 drains to 0 UTXOs.
//  4. F11-IDENT: the vault contract state is byte-identical across all 5 nodes at
//     the end, so neither the refusals nor the resume forked the fleet.
//
// A precondition that cannot be established (v2 not really on, rotation did not
// complete, FeeSupply not actually 0, gen-0 holding no UTXOs) is a t.Fatalf, never a
// t.Skip, so this test can never pass vacuously.
//
// RUN:
//
//	VAULT_F11_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF11EmptyFeeReserve -timeout 50m ./tests/devnet/
func TestVaultF11EmptyFeeReserve(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F11_RUN") == "" {
		t.Skip("set VAULT_F11_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (~block 190) so gen-0 is minted on the v2-off path (no
	// fresh-genesis deadlock) and v2 is ON for the rotation and the sweep.
	const hpin uint64 = 400

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 45*time.Minute)

	c := &vfCase{t: t}

	// SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F11 empty fee reserve")
	cid := env.cid

	// v2 must really be in force before any v2 assertion, otherwise the whole test
	// is vacuous (flag inert, registry absent).
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// ---- 1. rotate so there IS something to migrate, and deliberately do NOT fund
	//         the fee reserve ----
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q). Without a Retiring gen-0 and an Active gen-1 there is no migration for an empty fee reserve to refuse", primary1)
	}
	t.Logf("rotation done: gen-1 active, primary1=%s, gen-0 retiring (fee reserve deliberately NOT funded)", primary1)

	// PRECONDITION: the reserve really is empty and gen-0 really is funded and
	// Retiring. If any of that is false the refusal below would prove nothing.
	sup0 := vf11ReadSupply(d, ctx, 2, cid)
	if !sup0.readable {
		t.Fatalf("PRECONDITION FAILED: contract supply blob 's' is unreadable on magi-2 (%d bytes, want 32). The fee reserve value is the whole subject of this test", len(sup0.raw))
	}
	if sup0.fee != 0 {
		t.Fatalf("PRECONDITION FAILED: FeeSupply is %d, not 0, so migrateVault may legitimately succeed and F11 would be vacuous (active=%d user=%d baseFeeRate=%d)", sup0.fee, sup0.active, sup0.user, sup0.feeRate)
	}
	base := vf11Snap(t, d, ctx, cid)
	if base.gen0Stat != int(btcvault.VaultStatusRetiring) {
		t.Fatalf("PRECONDITION FAILED: gen-0 status on magi-2 is %d, want %d (Retiring). The refusal path under test only applies to a retiring or draining generation", base.gen0Stat, int(btcvault.VaultStatusRetiring))
	}
	if base.gen0Utxo < 1 {
		t.Fatalf("PRECONDITION FAILED: gen-0 holds %d registry UTXOs on magi-2, want at least 1. With nothing to sweep migrateVault returns 'nothing to migrate' and never reaches the fee reserve check", base.gen0Utxo)
	}
	t.Logf("PRECONDITION OK: FeeSupply=0 (active=%d user=%d baseFeeRate=%d), gen-0 status=%d Retiring holding %d UTXO(s), pending spends=%v, registry:%s",
		sup0.active, sup0.user, sup0.feeRate, base.gen0Stat, base.gen0Utxo, base.spends, vf11RegistryLine(d, ctx, 2, cid))

	// ---- 2. F11-REFUSE: migrateVault must be refused and must change NOTHING ----
	refuseStatus := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
	refused := !isOK(refuseStatus)
	terminal := refuseStatus == "FAILED" || refuseStatus == "REVERTED"
	afterRefuse := vf11Snap(t, d, ctx, cid)
	drift := vf11Diff(base, afterRefuse)
	stillRetiring := afterRefuse.gen0Stat == int(btcvault.VaultStatusRetiring)
	c.rec("F11-REFUSE", "migrateVault with an empty fee reserve is refused and leaves no half-built sweep",
		refused && len(drift) == 0 && stillRetiring,
		fmt.Sprintf("migrateVault status=%s (terminal FAILED/REVERTED=%v), gen-0 status=%d (want %d Retiring), gen-0 utxos %d -> %d, pending spends %v -> %v, stateDrift=%v",
			refuseStatus, terminal, afterRefuse.gen0Stat, int(btcvault.VaultStatusRetiring),
			base.gen0Utxo, afterRefuse.gen0Utxo, base.spends, afterRefuse.spends, drift))
	if refused && !terminal {
		t.Logf("NOTE: migrateVault status %q is not a terminal FAILED/REVERTED. The byte-level state comparison above, not the status string, is the authority on whether the call was refused", refuseStatus)
	}

	// ---- 3. F11-DRIVER: stand in for the mapping-bot's retry loop ----
	// No mapping-bot runs in the devnet, so the bot is reproduced as three
	// migrateVault calls 10 seconds apart. Nothing may accumulate across them.
	driverOK := true
	driverDetail := ""
	for i := 1; i <= 3; i++ {
		if i > 1 {
			time.Sleep(10 * time.Second)
		}
		s := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
		snap := vf11Snap(t, d, ctx, cid)
		grew := len(snap.spends) > len(base.spends)
		regSame := bytes.Equal(base.registry, snap.registry)
		retiring := snap.gen0Stat == int(btcvault.VaultStatusRetiring)
		sweepIdxSame := bytes.Equal(base.sweeps, snap.sweeps)
		if isOK(s) || grew || !regSame || !retiring || !sweepIdxSame {
			driverOK = false
		}
		driverDetail += fmt.Sprintf(" try%d(status=%s pendingSpends=%d grew=%v registryByteEqual=%v sweepIndexByteEqual=%v gen0Status=%d gen0Utxos=%d)",
			i, s, len(snap.spends), grew, regSame, sweepIdxSame, snap.gen0Stat, snap.gen0Utxo)
	}
	c.rec("F11-DRIVER", "three migrateVault retries 10s apart never grow the pending spend list, never move the vault registry, and leave gen-0 Retiring",
		driverOK,
		fmt.Sprintf("baseline pendingSpends=%d gen0Utxos=%d;%s", len(base.spends), base.gen0Utxo, driverDetail))

	// ---- 4. F11-RESUME: top the reserve up, the same rotation now completes ----
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	supFunded := vf11ReadSupply(d, ctx, 2, cid)
	if !supFunded.readable || supFunded.fee <= 0 {
		t.Errorf("fee reserve top-up did not land: FeeSupply=%d readable=%v (%d bytes). F11-RESUME below is expected to fail for that reason, not because the resume path is broken",
			supFunded.fee, supFunded.readable, len(supFunded.raw))
	} else {
		t.Logf("fee reserve topped up: FeeSupply 0 -> %d sats", supFunded.fee)
	}

	migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)

	drainDeadline := time.Now().Add(5 * time.Minute)
	drained := false
	drainDetail := ""
	for {
		g0 := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
		g1 := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
		st0 := vfVaultStatusOn(d, ctx, 2, cid, 0)
		drainDetail = fmt.Sprintf("magi-2 gen0Utxos=%d gen1Utxos=%d gen0Status=%d", g0, g1, st0)
		if g0 == 0 {
			drained = true
			break
		}
		if time.Now().After(drainDeadline) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	supAfter := vf11ReadSupply(d, ctx, 2, cid)
	c.rec("F11-RESUME", "funding the fee reserve resumes the stalled rotation and gen-0 drains to 0 UTXOs",
		drained,
		fmt.Sprintf("FeeSupply 0 -> %d (topped up) -> %d (after the sweep settled and its miner fee was debited), gen-0 utxos %d -> 0 wanted, %s",
			supFunded.fee, supAfter.fee, base.gen0Utxo, drainDetail))

	// ---- 5. no fork anywhere along the way ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F11-IDENT")

	c.summary("F11")
	t.Logf("F11 COMPLETE CONTRACT=%s (refusal status=%s, gen-0 drained=%v)", cid, refuseStatus, drained)
}

// vf11Supply is the decoded contract supply blob "s" (32 bytes: four big-endian
// int64 fields, ActiveSupply, UserSupply, FeeSupply, BaseFeeRate, mirroring
// contract/mapping/utils.go MarshalSupply).
type vf11Supply struct {
	active   int64
	user     int64
	fee      int64
	feeRate  int64
	raw      []byte
	readable bool
}

// vf11ReadSupply reads and decodes "s" from one node. readable is false when the key
// is missing or is not the expected 32 bytes.
func vf11ReadSupply(d *Devnet, ctx context.Context, node int, cid string) vf11Supply {
	st, err := getStateHex(d, ctx, node, cid, []string{"s"})
	if err != nil {
		return vf11Supply{}
	}
	raw := st["s"]
	if len(raw) != 32 {
		return vf11Supply{raw: raw}
	}
	return vf11Supply{
		active:   int64(binary.BigEndian.Uint64(raw[0:8])),
		user:     int64(binary.BigEndian.Uint64(raw[8:16])),
		fee:      int64(binary.BigEndian.Uint64(raw[16:24])),
		feeRate:  int64(binary.BigEndian.Uint64(raw[24:32])),
		raw:      raw,
		readable: true,
	}
}

// vf11Snapshot is every piece of contract state a successful HandleMigrateVault
// would have to touch. A refused call must leave all of it untouched.
type vf11Snapshot struct {
	spends   []string // "p" pending spend txids
	registry []byte   // "v" vault registry
	sweeps   []byte   // "msl" migration sweep index
	utxos    []byte   // "r" UTXO registry
	supply   []byte   // "s" supply blob
	gen0Utxo int
	gen0Stat int
}

// vf11Snap takes that snapshot from magi-2. The node is fixed at 2 because
// txSpendIds always reads node 2, so mixing nodes here would compare readings taken
// from two different replicas.
func vf11Snap(t *testing.T, d *Devnet, ctx context.Context, cid string) vf11Snapshot {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"v", "msl", "r", "s"})
	if err != nil {
		t.Logf("vf11Snap: state read on magi-2 failed: %v", err)
	}
	return vf11Snapshot{
		spends:   txSpendIds(t, d, ctx, cid),
		registry: st["v"],
		sweeps:   st["msl"],
		utxos:    st["r"],
		supply:   st["s"],
		gen0Utxo: vfGenUtxoCountOn(d, ctx, 2, cid, 0),
		gen0Stat: vfVaultStatusOn(d, ctx, 2, cid, 0),
	}
}

// vf11Diff lists everything that moved between two snapshots. An empty result means
// the refused call was a true no-op at the byte level.
func vf11Diff(before, after vf11Snapshot) []string {
	var out []string
	if !vf11SameIds(before.spends, after.spends) {
		out = append(out, fmt.Sprintf("p pendingSpends %v -> %v", before.spends, after.spends))
	}
	if !bytes.Equal(before.registry, after.registry) {
		out = append(out, fmt.Sprintf("v vaultRegistry not byte-equal (%d -> %d bytes)", len(before.registry), len(after.registry)))
	}
	if !bytes.Equal(before.sweeps, after.sweeps) {
		out = append(out, fmt.Sprintf("msl migrationSweepIndex not byte-equal (%d -> %d bytes)", len(before.sweeps), len(after.sweeps)))
	}
	if !bytes.Equal(before.utxos, after.utxos) {
		out = append(out, fmt.Sprintf("r utxoRegistry not byte-equal (%d -> %d bytes)", len(before.utxos), len(after.utxos)))
	}
	if !bytes.Equal(before.supply, after.supply) {
		out = append(out, fmt.Sprintf("s supply not byte-equal (%x -> %x)", before.supply, after.supply))
	}
	if before.gen0Utxo != after.gen0Utxo {
		out = append(out, fmt.Sprintf("gen0Utxos %d -> %d", before.gen0Utxo, after.gen0Utxo))
	}
	if before.gen0Stat != after.gen0Stat {
		out = append(out, fmt.Sprintf("gen0Status %d -> %d", before.gen0Stat, after.gen0Stat))
	}
	return out
}

// vf11SameIds reports whether two pending spend id lists hold the same set.
func vf11SameIds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := append([]string{}, a...)
	cb := append([]string{}, b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

// vf11RegistryLine renders the decoded vault registry of one node for the log.
func vf11RegistryLine(d *Devnet, ctx context.Context, node int, cid string) string {
	vs := vfVaultRegistryOn(d, ctx, node, cid)
	if len(vs) == 0 {
		return " registry ABSENT"
	}
	out := ""
	for _, v := range vs {
		out += fmt.Sprintf(" gen%d=status%d", v.Generation, v.Status)
	}
	return out
}
