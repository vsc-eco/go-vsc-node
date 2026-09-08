package devnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// vault_f7_mixed_fleet_flag_on_test.go, F7 of the BTC vault-rotation-v2
// FAILURE-STATE suite. Shared helpers live in vault_failure_helpers_test.go (vf*);
// the mixed-fleet layout is modelled on vault_mixed_version_test.go and the
// rotation flow on vault_stage4_test.go.

// vfF7SlotRow is one block_headers row: the VSC slot and the block CID the node
// committed at that slot. A row exists only for a block that gathered BLS quorum,
// which is why this collection, and not hive_blocks, is the fork/halt instrument.
type vfF7SlotRow struct {
	SlotHeight int    `bson:"slot_height"`
	Block      string `bson:"block"`
}

// vfF7ScanBlockHeaders reads block_headers from every node's database in one pass
// and returns the per-node maximum slot height, how many cross-node comparisons it
// made, the FIRST divergence found ("" when every shared slot agrees) and a
// description of any read problem ("" when every node was readable).
//
// The comparison is the same one requireConverged makes (same collection, same
// slot keying, same `block` field), lifted out so it can be run repeatedly without
// aborting the test.
// vfF7DumpHaltLogs prints, per node, the last `tail` block-producer / ERROR / WARN
// lines (oracle "elected members" spam excluded). At the F7 halt this is the
// evidence the block_headers scan cannot give: which node produced the block that
// carries the first map, which nodes rejected it and why ("CID MISMATCH", "sig
// rejected", "not enough signatures"), and the tx-level reason on the producing
// tree ("tx dropped - rc_limit insufficient for ops", "tx RC consume failed").
// Run 2 measured HALT-not-fork but could not name the diverging step.
func vfF7DumpHaltLogs(t *testing.T, d *Devnet, ctx context.Context, nodes []int, tail int) {
	t.Helper()
	for _, n := range nodes {
		container := d.containerName(n)
		out, err := exec.CommandContext(ctx, "bash", "-c",
			fmt.Sprintf("docker logs --tail 6000 %s 2>&1 | grep -aE 'module=bp|\\[ERROR\\]|\\[WARN\\]' | grep -av 'elected members' | tail -%d", container, tail),
		).CombinedOutput()
		if err != nil {
			t.Logf("F7 halt logs magi-%d: could not read: %v", n, err)
			continue
		}
		t.Logf("F7 halt logs magi-%d (last %d bp/ERROR/WARN lines):\n%s", n, tail, strings.TrimSpace(string(out)))
	}
}

func vfF7ScanBlockHeaders(d *Devnet, ctx context.Context, nodes int) (map[int]int, int, string, string) {
	maxSlots := map[int]int{}
	client, err := d.mongoClient(ctx)
	if err != nil {
		return maxSlots, 0, "", fmt.Sprintf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)

	ref := map[int]string{}
	refNode := map[int]int{}
	compared := 0
	readErr := ""
	for n := 1; n <= nodes; n++ {
		cur, err := client.Database(d.nodeDbName(n)).Collection("block_headers").Find(ctx, bson.M{})
		if err != nil {
			readErr += fmt.Sprintf(" magi-%d find: %v;", n, err)
			continue
		}
		var rows []vfF7SlotRow
		if err := cur.All(ctx, &rows); err != nil {
			readErr += fmt.Sprintf(" magi-%d decode: %v;", n, err)
			continue
		}
		maxSlots[n] = -1
		for _, r := range rows {
			if r.SlotHeight > maxSlots[n] {
				maxSlots[n] = r.SlotHeight
			}
			prev, seen := ref[r.SlotHeight]
			if !seen {
				ref[r.SlotHeight] = r.Block
				refNode[r.SlotHeight] = n
				continue
			}
			compared++
			if prev != r.Block {
				return maxSlots, compared, fmt.Sprintf("slot %d: magi-%d block=%s, magi-%d block=%s",
					r.SlotHeight, n, r.Block, refNode[r.SlotHeight], prev), strings.TrimSpace(readErr)
			}
		}
	}
	return maxSlots, compared, "", strings.TrimSpace(readErr)
}

// vfF7FormatMax renders the per-node max slot map in node order (-1 = the node had
// no readable block_headers rows, "?" = the node could not be read at all).
func vfF7FormatMax(maxSlots map[int]int, nodes int) string {
	parts := make([]string, 0, nodes)
	for n := 1; n <= nodes; n++ {
		if v, ok := maxSlots[n]; ok {
			parts = append(parts, fmt.Sprintf("magi-%d=%d", n, v))
		} else {
			parts = append(parts, fmt.Sprintf("magi-%d=?", n))
		}
	}
	return strings.Join(parts, " ")
}

// vfForkOrStall watches the fleet's block_headers for `window` and classifies the
// outcome WITHOUT ever failing the test. It is requireConverged's cross-node
// comparison with every Fatalf removed, because in F7 a fork is the EXPECTED
// result and aborting the run would throw the evidence away.
//
//	forked  = two nodes committed a different block at the same slot height.
//	stalled = no node's max slot height grew for 3 minutes of readable polling.
//	detail  = the outcome string, prefixed "FORK", "STALL", "CONVERGED" or
//	          "INCONCLUSIVE" (the last one means the instrument itself went blind,
//	          which is NOT an observation of the fleet).
//
// Instrument note: an unreadable poll never counts toward the stall verdict, so a
// Mongo outage cannot masquerade as a halted chain.
func vfForkOrStall(t *testing.T, d *Devnet, ctx context.Context, nodes int, window time.Duration) (bool, bool, string) {
	t.Helper()
	const stallWindow = 3 * time.Minute
	const poll = 15 * time.Second

	deadline := time.Now().Add(window)
	best := -1
	noGrowth := time.Duration(0)
	blind := time.Duration(0)
	lastPoll := time.Now()
	compared := 0
	for {
		now := time.Now()
		step := now.Sub(lastPoll)
		lastPoll = now

		maxSlots, cmp, fork, readErr := vfF7ScanBlockHeaders(d, ctx, nodes)
		compared = cmp
		// The spec's named instrument, read directly on one new-code and one
		// old-code node so the two halves of the fleet are visible side by side.
		m1, e1 := vfMaxSlotHeight(d, ctx, 1)
		m4, e4 := -1, error(nil)
		if nodes >= 4 {
			m4, e4 = vfMaxSlotHeight(d, ctx, 4)
		}
		t.Logf("  F7 watch: vfMaxSlotHeight magi-1=%d (err=%v) magi-4=%d (err=%v), %d cross-node comparisons, per-node %s",
			m1, e1, m4, e4, cmp, vfF7FormatMax(maxSlots, nodes))
		if readErr != "" {
			t.Logf("  F7 watch: block_headers read problem: %s", readErr)
		}
		if fork != "" {
			return true, false, fmt.Sprintf("FORK at %s | per-node max slot %s | %d cross-node block comparisons made before the divergence",
				fork, vfF7FormatMax(maxSlots, nodes), cmp)
		}

		top := -1
		for _, v := range maxSlots {
			if v > top {
				top = v
			}
		}
		if len(maxSlots) == 0 {
			blind += step
			if blind >= stallWindow {
				return false, false, fmt.Sprintf("INCONCLUSIVE | block_headers unreadable on every node for %s: %s", stallWindow, readErr)
			}
		} else {
			blind = 0
			if top > best {
				best = top
				noGrowth = 0
			} else {
				noGrowth += step
			}
			if noGrowth >= stallWindow {
				return false, true, fmt.Sprintf("STALL at height %d | no node's block_headers grew for %s | per-node max slot %s",
					best, stallWindow, vfF7FormatMax(maxSlots, nodes))
			}
		}

		if !time.Now().Before(deadline) {
			return false, false, fmt.Sprintf("CONVERGED | watched %s, top slot %d, per-node max slot %s | %d cross-node block comparisons all identical",
				window, best, vfF7FormatMax(maxSlots, nodes), compared)
		}
		select {
		case <-ctx.Done():
			return false, false, fmt.Sprintf("INCONCLUSIVE | context ended during the watch (top slot %d, %d comparisons made)", best, compared)
		case <-time.After(poll):
		}
	}
}

// vfF7StatusName renders a vault generation status for the log.
func vfF7StatusName(s int) string {
	switch s {
	case 0:
		return "Pending"
	case 1:
		return "Active"
	case 2:
		return "Retiring"
	case 3:
		return "Draining"
	case 4:
		return "Inactive"
	case 5:
		return "Purged"
	default:
		return "absent-or-unreadable"
	}
}

// TestVaultF7MixedFleetFlagOn is the F7 experiment: a MIXED-VERSION fleet standing
// at the vault-rotation-v2 activation height with the flag turning ON underneath it.
//
// What it proves: VaultRotationV2ActivationHeight is a BARE CONFIG PIN. Nothing in
// the node checks, before that height arrives, that the elected fleet is actually
// running a binary which understands the v2 rules. There is no readiness vote, no
// running-version gate, no refusal to enter the new rule set while a stale peer is
// still in the committee. So an operator fleet that is still mid-rollout at the
// pinned height runs TWO different rule sets over the same L1 ops, and the only
// question is what that costs: a FORK (nodes commit different blocks at the same
// slot) or a STALL (no side can gather BLS quorum any more).
//
// The test builds exactly that fleet, 3 new-code nodes and 3 old-code nodes from
// OLD_CODE_DIR, pins activation at block 400, and then drives the first genuinely
// v2-gated action across it: a gen-0 to gen-1 rotation whose activateKey needs the
// BRK-2 check signature that only the new binary produces and evaluates.
//
// This test does NOT assert cross-node identity at the end and does NOT fail on a
// fork. A fork is the EXPECTED outcome, so every post-activation step is recorded
// as an OBSERVATION with its full detail, and the verdict belongs in the report.
// The only hard assertions are the preconditions: the fleet must converge BEFORE
// the pin (the inert path is byte-identical, proven by vault_mixed_version_test.go)
// and v2 must really be active before the experiment starts, otherwise the run
// would prove nothing. CONVERGED is a legitimate observation too and is reported
// together with the activateKey status and the per-node gen-1 vault status, since
// it would mean the old binary somehow agreed with the new one on the output of a
// v2-gated action.
//
// Layout: 6 nodes, 1 to 3 on this tree's code, 4 to 6 on OLD_CODE_DIR (default
// /home/clauderfly/gvn-oldmain). Genesis on node 1 so the chain starts on new code.
// The pin at 400 lands AFTER genesis (about block 190), so gen-0 is minted with the
// flag still off (no fresh-genesis deadlock) and the flag then flips underneath a
// running mixed fleet, which is the real rollout shape. BLS quorum on 6 nodes is 4;
// the TSS signing threshold is also 4 of 6, and all 6 stay up for the whole run, so
// neither a quorum loss nor a signing loss can be confused with the fork this test
// is looking for.
//
// Run:
//
//	VAULT_F7_RUN=1 OLD_CODE_DIR=/home/clauderfly/gvn-oldmain \
//	  BTC_MAPPING_WASM_PATH=/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm \
//	  go test -v -run TestVaultF7MixedFleetFlagOn -timeout 95m ./tests/devnet/
func TestVaultF7MixedFleetFlagOn(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F7_RUN") == "" {
		t.Skip("set VAULT_F7_RUN=1 to run the F7 mixed-fleet-at-activation experiment")
	}
	requireDocker(t)

	oldCodeDir := os.Getenv("OLD_CODE_DIR")
	if oldCodeDir == "" {
		oldCodeDir = "/home/clauderfly/gvn-oldmain"
	}
	if _, err := os.Stat(oldCodeDir); err != nil {
		t.Fatalf("PRECONDITION FAILED: old-code repo not found at %s (%v). F7 is a MIXED-fleet experiment; without a second binary the run would silently degrade to a single-version devnet and prove nothing. Set OLD_CODE_DIR.", oldCodeDir, err)
	}
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("PRECONDITION FAILED: btc-mapping-contract regtest wasm not found at %s: %v", wasm, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(85*time.Minute))
	defer cancel()

	const hpin = uint64(400)
	newNodes := []int{1, 2, 3}
	oldNodes := []int{4, 5, 6}

	cfg := vfSlowReshareConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = false // contract deploy needs the deployer funded
	cfg.EnableBitcoind = true
	cfg.GenesisNode = 1 // genesis on a NEW-code node
	cfg.OldCodeSourceDir = oldCodeDir
	cfg.OldCodeNodes = oldNodes
	// The old-code image's default Go base is below the merge-base go.mod's
	// toolchain and cannot self-upgrade under GOTOOLCHAIN=local.
	cfg.OldCodeGoImage = "golang:1.25.10"
	// Run 3: the block producer's own view of the halt on BOTH trees (both vsclog
	// parsers take per-module levels). At bp=debug the signer side logs "CID MISMATCH",
	// "sig rejected", "not enough signatures" and the producer side logs the tx-level
	// reason ("tx dropped - rc_limit insufficient for ops", "tx RC consume failed").
	cfg.LogLevel = "error,tss=trace,bp=debug"
	// The pin is written into the sysconfig every node reads, but only the NEW
	// binary has the field, so the old nodes ignore it: that asymmetry IS the test.
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)
	t.Logf("F7 mixed fleet up: new-code=%v, old-code=%v from %s, activation pin=%d", newNodes, oldNodes, oldCodeDir, hpin)

	c := &vfCase{t: t}

	// ---- 1. deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	// vfSetup calls from node 1 and reads from node 2, both NEW-code nodes.
	// Run 1 (2026-09-06) died inside vfSetup: on the 3+3 main/develop fleet every call
	// confirmed until the first `map`, which stayed INCLUDED forever at block ~125, far
	// below the pin. That stall is itself an upgrade-path measurement, so the setup is
	// allowed to soft-fail and the fleet is scanned for fork-vs-halt before giving up.
	vfSetupSoftFail = true
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "F7 mixed fleet at activation")
	vfSetupSoftFail = false
	if env == nil {
		c := &vfCase{t: t}
		processed, perr := d.getLastProcessedBlock(ctx, 2)
		maxSlots, comparisons, fork, readErr := vfF7ScanBlockHeaders(d, ctx, cfg.Nodes)
		grew, gStart, gLast := vfGrewWithin(d, ctx, 2, 90*time.Second)
		c.rec("F7-SETUP-STALL", "INFO (measured, not a pass): the main/develop mixed fleet stopped finalizing BEFORE the activation pin, on the first map call",
			false, fmt.Sprintf("node2 processed=%d (err=%v) hpin=%d; block_headers grew in 90s=%v (%d->%d); per-node max slots %s; comparisons=%d; first divergence=%q; readErr=%q",
				processed, perr, hpin, grew, gStart, gLast, vfF7FormatMax(maxSlots, cfg.Nodes), comparisons, fork, readErr))
		vfF7DumpHaltLogs(t, d, ctx, vfAllNodes(cfg.Nodes), 60)
		c.summary("F7")
		t.Logf("F7 ABORTED at setup: the pin measurement is unreachable on this fleet mix; see F7-SETUP-STALL for fork-vs-halt evidence and the per-node halt logs above for the diverging step")
		return
	}
	cid := env.cid

	// ---- 2. F7-PRE: the fleet agrees BEFORE the pin.
	// Run the local comparator first so that, if requireConverged's Fatalf fires,
	// the divergence detail is already in the log instead of only its own message.
	proc2, procErr := d.getLastProcessedBlock(ctx, 2)
	preMax, preCompared, preFork, preReadErr := vfF7ScanBlockHeaders(d, ctx, cfg.Nodes)
	t.Logf("pre-activation scan: node 2 processed=%d (err=%v), hpin=%d, %d cross-node comparisons, per-node %s, fork=%q, readErr=%q",
		proc2, procErr, hpin, preCompared, vfF7FormatMax(preMax, cfg.Nodes), preFork, preReadErr)
	if proc2 > hpin {
		t.Logf("NOTE: node 2 is ALREADY past hpin=%d, so F7-PRE is no longer strictly a pre-activation baseline (setup ran slower than the pin); the experiment below is unaffected", hpin)
	}
	// requireConverged is fatal on a fork or a stall, which is correct HERE: this is
	// the precondition, and the inert pre-pin path is proven byte-identical by
	// vault_mixed_version_test.go. Reaching the record below therefore means PASS.
	base := requireConverged(t, ctx, d, cfg.Nodes, 0, 6*time.Minute)
	c.rec("F7-PRE", "mixed fleet converges before the activation height (inert path byte-identical)", true,
		fmt.Sprintf("common height %d, node 2 processed=%d, hpin=%d, %d cross-node block comparisons identical", base, proc2, hpin, preCompared))

	// ---- 3. the flag turns on, then drive the first v2-gated action.
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// Everything from here on is an OBSERVATION, not an assertion: if the fleet
	// forks or stalls at the pin then createKey, keygen, registerPublicKey and
	// activateKey are all expected to misbehave, and recording those as FAIL would
	// bury the actual finding under noise. The statuses are carried into the
	// F7-OUTCOME detail instead.
	ck := vstatus(t, d, ctx, 1, cid, "createKey", "")
	t.Logf("createKey (gen-1) status=%s", ck)

	primary1 := ""
	regStatus := "(not attempted)"
	actStatus := "(not attempted)"
	activated := false

	kd1, kerr := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-mainv1", "status": "active"}, 10*time.Minute)
	if kerr != nil {
		all, _ := d.GetTssKeys(ctx, 2, bson.M{})
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s epoch=%d", k.Id, k.Status, k.Epoch)
		}
		c.rec("F7-KEYGEN", "OBSERVATION: gen-1 keygen outcome on the mixed fleet recorded", true,
			fmt.Sprintf("createKey status=%s, keygen did NOT reach active within 10m: %v", ck, kerr))
	} else {
		primary1 = kd1.PublicKey
		c.rec("F7-KEYGEN", "OBSERVATION: gen-1 keygen outcome on the mixed fleet recorded", true,
			fmt.Sprintf("createKey status=%s, key %s-mainv1 active, epoch=%d, pubkey=%s", ck, cid, kd1.Epoch, primary1))

		regStatus = vstatus(t, d, ctx, 1, cid, "registerPublicKey",
			fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary1, backupPubKeyG))
		t.Logf("registerPublicKey (gen-1) status=%s", regStatus)
		// activateKey is the first output that depends on a v2-only evaluation
		// (BRK-2 check signature admission), so it is where old and new code have
		// their first chance to disagree on a committed result.
		for i := 0; i < 12; i++ {
			actStatus = vstatus(t, d, ctx, 1, cid, "activateKey", "")
			if isOK(actStatus) {
				activated = true
				break
			}
			t.Logf("activateKey not yet (awaiting BRK-2 check-sig), try %d status=%s", i, actStatus)
			time.Sleep(15 * time.Second)
		}
		c.rec("F7-ACTIVATE", "OBSERVATION: gen-1 activateKey outcome under a mixed fleet recorded", true,
			fmt.Sprintf("registerPublicKey=%s, activateKey final status=%s, activated=%v after up to 12 tries", regStatus, actStatus, activated))
	}

	// ---- 4. F7-OUTCOME: the experiment. Classify without failing.
	m1Before, e1 := vfMaxSlotHeight(d, ctx, 1)
	m4Before, e4 := vfMaxSlotHeight(d, ctx, 4)
	t.Logf("watch start: new-code magi-1 max slot=%d (err=%v), old-code magi-4 max slot=%d (err=%v)", m1Before, e1, m4Before, e4)

	forked, stalled, outcome := vfForkOrStall(t, d, ctx, cfg.Nodes, 6*time.Minute)

	m1After, _ := vfMaxSlotHeight(d, ctx, 1)
	m4After, _ := vfMaxSlotHeight(d, ctx, 4)
	observed := strings.HasPrefix(outcome, "FORK") || strings.HasPrefix(outcome, "STALL") || strings.HasPrefix(outcome, "CONVERGED")
	c.rec("F7-OUTCOME", "INFO: mixed-fleet outcome at the activation height was OBSERVED (the verdict belongs in the report, not in this pass/fail)", observed,
		fmt.Sprintf("%s || forked=%v stalled=%v || new-code magi-1 max slot %d->%d, old-code magi-4 max slot %d->%d || createKey=%s registerPublicKey=%s activateKey=%s activated=%v",
			outcome, forked, stalled, m1Before, m1After, m4Before, m4After, ck, regStatus, actStatus, activated))
	switch {
	case forked:
		t.Logf("F7 RESULT: FORK. A stale binary elected at the pinned height committed a different block than the new binary. %s", outcome)
	case stalled:
		t.Logf("F7 RESULT: STALL. Block production stopped at the pinned height with a mixed fleet. %s", outcome)
	case observed:
		t.Logf("F7 RESULT: CONVERGED. Old and new nodes agreed through the v2-gated activateKey; report this with the activateKey status and the per-node vault status below. %s", outcome)
	default:
		t.Logf("F7 RESULT: INCONCLUSIVE, the instrument went blind. %s", outcome)
	}

	// ---- 5. F7-STATUS: the gen-1 vault status on every node, new and old.
	// The vault-state fingerprint (vfStateOn + vfFingerprint over the same keys
	// vfAssertContractIdentical uses) is logged alongside it as evidence, but it is
	// deliberately NOT asserted: this test observes, it does not adjudicate.
	statusDetail := ""
	for n := 1; n <= cfg.Nodes; n++ {
		kind := "new"
		for _, o := range oldNodes {
			if n == o {
				kind = "old"
			}
		}
		g1 := vfVaultStatusOn(d, ctx, n, cid, 1)
		g0 := vfVaultStatusOn(d, ctx, n, cid, 0)
		fp := "unreadable"
		if st, err := vfStateOn(d, ctx, n, cid); err == nil {
			fp = vfFingerprint(st)
		}
		t.Logf("  magi-%d (%s code): gen-1 status=%d (%s), gen-0 status=%d (%s), vault-state fp=%s",
			n, kind, g1, vfF7StatusName(g1), g0, vfF7StatusName(g0), fp)
		statusDetail += fmt.Sprintf(" magi-%d(%s):gen1=%d/%s,gen0=%d/%s,fp=%s", n, kind, g1, vfF7StatusName(g1), g0, vfF7StatusName(g0), fp)
	}
	c.rec("F7-STATUS", "INFO: per-node gen-1 vault status recorded across the mixed fleet (-1 = absent or unreadable)", true, strings.TrimSpace(statusDetail))

	c.summary("F7")
	t.Logf("F7 COMPLETE CONTRACT=%s outcome=%s", cid, outcome)
}
