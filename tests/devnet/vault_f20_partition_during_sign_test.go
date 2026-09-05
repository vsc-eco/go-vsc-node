package devnet

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"
)

// f20BlamedNames decodes a TSS blame commitment (base64 raw-url encoded, big-endian
// bitset over the election member ordering) into the member names it points at. It
// never fails the test: an empty or undecodable commitment yields nil plus a reason
// string, which is recorded as evidence instead of aborting the run. This is a local
// copy of decodeBitset's decoding rule on purpose, because decodeBitset calls
// t.Fatalf on a bad string and this test must keep gathering evidence after setup.
func f20BlamedNames(commitment string, members []string) ([]string, string) {
	if commitment == "" {
		return nil, "empty commitment"
	}
	raw, err := base64.RawURLEncoding.DecodeString(commitment)
	if err != nil {
		return nil, fmt.Sprintf("undecodable commitment %q: %v", commitment, err)
	}
	bits := new(big.Int).SetBytes(raw)
	var names []string
	for i := 0; i < len(members); i++ {
		if bits.Bit(i) == 1 {
			names = append(names, members[i])
		}
	}
	return names, ""
}

// TestVaultF20PartitionDuringSign is failure-state F20 of the BTC vault-rotation-v2
// suite: "network partition during migration-sweep signing".
//
// WHY A PARTITION AND NOT A STOP
// Every other failure test in this suite injects its fault by stopping a container,
// which is the clean, cooperative version of a node going away: the process dies, its
// TCP sessions reset immediately, and every peer learns the truth at once. That is NOT
// what an operator actually sees. The realistic failure is a node that is still RUNNING
// and still believes it is a full member of the committee (it still ingests L1 from
// HAF, still executes contract calls, still declares readiness) while its packets to
// and from its peers are silently dropped. Peers do not get a reset, they get silence,
// and they only find out at a protocol timeout. So this test uses d.Disconnect(ctx, 3),
// which installs per-peer iptables DROP rules inside magi-3 and deliberately leaves the
// shared infrastructure (HAF, Mongo, drone) reachable so the node stays alive rather
// than shutting itself down on a dead L1 stream.
//
// WHAT IT PROVES
//  1. F20-SIGNS: signing liveness with one party unreachable. The TSS threshold is
//     ceil(2n/3)-1 = 3, so 4 of 5 parties must be live to produce a signature, and 4
//     are (nodes 1, 2, 4, 5). The sweep is built with the full committee up and the
//     partition is injected immediately afterwards, so the first sign session may well
//     have already picked node 3 into its party list. That session cannot finish and
//     must time out. The case therefore allows two attempts: the retry at the NEXT sign
//     interval must succeed without node 3. The evidence recorded is node 1's count of
//     "timeout result" log lines and whether the latest blame commitment for the
//     retiring key names node 3.
//  2. F20-SETTLED: the sweep signed under partition is a real, spendable Bitcoin
//     transaction, not a half-built artifact: it broadcasts, mines, relays and settles,
//     and gen-0 drains to 0 UTXOs.
//  3. F20-CATCHUP: after d.Reconnect the isolated node rejoins and its processed height
//     reaches the height the connected fleet had reached.
//  4. F20-NOPANIC: the isolated node survives the partition without a panic or a Go
//     runtime fatal error, so the "still running but unreachable" state is handled, not
//     crashed through.
//  5. F20-IDENT: byte-identical vault contract state across all 5 nodes, node 3
//     included. This is the fork detector. A node that missed the whole signing round
//     and then replayed L1 must land on exactly the same contract state as the nodes
//     that were present, otherwise the partition produced a silent fork.
//
// A precondition that cannot be established (rotation, sweep build) is a t.Fatalf,
// never a t.Skip, so this test can never pass vacuously. After setup it prefers
// t.Errorf so a single failure still yields the rest of the evidence. The Reconnect is
// both explicit (before the catch-up and identity checks) and deferred, so a failed or
// panicking run can never leave iptables DROP rules behind inside the container.
//
// RUN:
//
//	VAULT_F20_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF20PartitionDuringSign -timeout 60m ./tests/devnet/
func TestVaultF20PartitionDuringSign(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F20_RUN") == "" {
		t.Skip("set VAULT_F20_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (about block 190) so gen-0 is minted on the v2-off path
	// (no fresh-genesis deadlock) and v2 is ON for the rotation and the sweep.
	const hpin uint64 = 400

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 50*time.Minute)

	// Safety net: whatever happens below, magi-3's peer DROP rules are flushed. A fresh
	// context is used because the test context may already be cancelled or expired by
	// the time this runs. Deferred funcs run before t.Cleanup, so the container is still
	// up. Reconnect is a plain iptables flush and is safe to call when nothing is
	// partitioned, so registering it this early costs nothing.
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer rcancel()
		if err := d.Reconnect(rctx, 3); err != nil {
			t.Logf("WARNING: deferred reconnect of magi-3 failed, iptables DROP rules may survive in the container: %v", err)
		}
	}()

	c := &vfCase{t: t}

	// SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F20 partition during sign")
	cid := env.cid

	// v2 must really be in force before any v2 assertion, otherwise the whole test is
	// vacuous (flag inert, registry absent).
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q). Without a Retiring gen-0 and an Active gen-1 there is no migration sweep to sign under a partition", primary1)
	}
	t.Logf("rotation done: gen-1 active, primary1=%s, gen-0 retiring", primary1)

	// The migration sweep pays its miner fee out of FeeSupply, so seed it.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---- 1. build the sweep with the FULL committee up, then partition node 3 ----
	// Ordering matters: the sweep must exist as a pending spend with signing data
	// BEFORE the partition, so the fault lands on the SIGNING round and not on the
	// contract call that builds the sweep.
	txid, sd, buildStatus := vfBuildSweep(t, d, ctx, 1, cid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: no migration sweep signing data appeared (txid=%q migrateVault status=%s). There is nothing to sign under a partition", txid, buildStatus)
	}
	t.Logf("sweep built: txid=%s inputs=%d (migrateVault status=%s)", txid, len(sd.UnsignedSigHashes), buildStatus)

	if err := d.Disconnect(ctx, 3); err != nil {
		t.Fatalf("PRECONDITION FAILED: could not partition magi-3 (%v). Without the partition this test would prove nothing that vault_stage4 does not already prove", err)
	}
	t.Logf("magi-3 is now PARTITIONED from its peers (still running, still reading L1 from HAF, p2p gossip severed)")

	// ---- 2. the remaining 4 parties must sign the sweep ----
	// Attempt 1 may include node 3 in the party list and time out; the retry at the
	// next sign interval must then complete without it. Two attempts, no more: a third
	// would blur "retried once" into "eventually".
	const signAttempts = 2
	raw := ""
	signed := false
	attemptsUsed := 0
	for attempt := 1; attempt <= signAttempts; attempt++ {
		attemptsUsed = attempt
		r, ok := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
		if ok {
			raw = r
			signed = true
			break
		}
		t.Logf("sign attempt %d/%d did not collect every input signature; the session most likely included the partitioned node 3 and timed out, waiting for the next sign interval", attempt, signAttempts)
	}

	timeouts1 := vfCountLogs(d, ctx, 1, "timeout result")
	blameDetail := f20BlameDetail(t, d, ctx, cid)
	c.rec("F20-SIGNS", "the 4 reachable parties sign the migration sweep while node 3 is partitioned (retrying at the next sign interval if the first session included it)", signed,
		fmt.Sprintf("attempts=%d/%d inputs=%d magi-1 'timeout result' lines=%d; %s",
			attemptsUsed, signAttempts, len(sd.UnsignedSigHashes), timeouts1, blameDetail))

	// ---- 3. broadcast, mine, relay and settle ----
	settled := false
	settleDetail := "the sweep was never signed, so it could not be broadcast or settled"
	if signed {
		bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, raw)
		if err != nil {
			settleDetail = fmt.Sprintf("sweep broadcast/mine failed: %v (bcTxid=%q)", err, bcTxid)
			t.Errorf("the sweep signed under partition was REJECTED by bitcoind: %v. A signature collected with a party missing must still produce a valid spendable transaction", err)
		} else {
			t.Logf("sweep broadcast and mined: bcTxid=%s btcHeight=%d", bcTxid, h)
			st := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
			t.Logf("confirmSpend returned status=%s", st)

			deadline := time.Now().Add(4 * time.Minute)
			for {
				g0 := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
				g1 := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
				settleDetail = fmt.Sprintf("magi-2 gen0Utxos=%d gen1Utxos=%d confirmSpend=%s bcTxid=%s btcHeight=%d", g0, g1, st, bcTxid, h)
				if g0 == 0 {
					settled = true
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(15 * time.Second)
			}
		}
	}
	c.rec("F20-SETTLED", "the sweep signed under partition settles and gen-0 drains to 0 UTXOs on node 2", settled,
		"want gen0Utxos=0; "+settleDetail)

	// ---- 4. heal the partition and let the isolated node catch up ----
	if err := d.Reconnect(ctx, 3); err != nil {
		t.Errorf("could not reconnect magi-3: %v. The catch-up and identity cases below cannot mean anything while the node is still isolated", err)
	} else {
		t.Logf("magi-3 reconnected, waiting for it to catch up")
	}

	target, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Errorf("could not read magi-1 processed height for the catch-up target: %v", err)
		target = hpin + 5
	}
	caughtUp := vfWaitProcessed(t, d, ctx, 3, target, 8*time.Minute)
	bh3, err3 := d.getLastProcessedBlock(ctx, 3)
	c.rec("F20-CATCHUP", "the previously partitioned node 3 rejoins and reaches the connected fleet's processed height", caughtUp,
		fmt.Sprintf("target(from magi-1)=%d magi-3 processed=%d (err=%v) within 8m", target, bh3, err3))

	// ---- 5. the isolated node must not have crashed through the partition ----
	hit, panicked := vfLogsContainAny(d, ctx, 3, "panic:", "fatal error:")
	c.rec("F20-NOPANIC", "the partitioned node logged no panic and no Go runtime fatal error", !panicked,
		fmt.Sprintf("magi-3 logs matched=%q panicked=%v", hit, panicked))

	// ---- 6. the fork detector, node 3 included ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F20-IDENT")

	c.summary("F20")
	t.Logf("F20 COMPLETE CONTRACT=%s sweepTxid=%s signed=%v attempts=%d", cid, txid, signed, attemptsUsed)
}

// f20BlameDetail reads the latest blame commitment for the retiring generation's key
// from node 1 and renders it as an evidence string, including whether the partitioned
// node (magi-3) is one of the blamed members. It never fails the test: every read error
// becomes part of the recorded detail, because "was node 3 blamed" is evidence about
// the partition, not a pass/fail condition of its own.
func f20BlameDetail(t *testing.T, d *Devnet, ctx context.Context, cid string) string {
	t.Helper()
	keyId := cid + "-main"
	blame, err := d.GetLatestBlame(ctx, 1, keyId)
	if err != nil {
		return fmt.Sprintf("blame read on magi-1 failed: %v", err)
	}
	if blame == nil {
		return fmt.Sprintf("no blame commitment for %s (the sign session may have excluded node 3 by readiness instead of blaming it)", keyId)
	}
	node3 := d.witnessAccount(3)
	members, merr := d.GetElectionMembers(ctx, 1, blame.Epoch)
	memberSrc := fmt.Sprintf("elections epoch=%d", blame.Epoch)
	if merr != nil {
		// Fall back to the deterministic devnet witness ordering so the bitset can
		// still be read, and say so, because the ordering is then an assumption.
		members = nil
		for n := 1; n <= d.cfg.Nodes; n++ {
			members = append(members, d.witnessAccount(n))
		}
		memberSrc = fmt.Sprintf("ASSUMED witness ordering (elections epoch=%d unreadable: %v)", blame.Epoch, merr)
	}
	names, why := f20BlamedNames(blame.Commitment, members)
	if why != "" {
		return fmt.Sprintf("blame block=%d epoch=%d but its bitset could not be read (%s); members from %s", blame.BlockHeight, blame.Epoch, why, memberSrc)
	}
	return fmt.Sprintf("blame block=%d epoch=%d targets=%v node3=%s blamed=%v; members from %s",
		blame.BlockHeight, blame.Epoch, names, node3, contains(names, node3), memberSrc)
}
