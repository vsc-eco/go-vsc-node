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

// 18an devnet proof — a fault in ONE key family must not cost a witness its
// seat in ANOTHER family's key.
//
// Before 18an the TSS ban score was one network-wide number: blames from every
// key were summed, and a witness over the threshold was excluded from keygen,
// signing and reshare of EVERY key. Once the EVM bridge holds keys, a witness
// with an EVM-only fault would be banned from the BTC vault's committee too,
// although its BTC shares are perfectly good. 18an scores per key family (the
// key's owning contract), behind the 0.13.0 consensus gate.
//
// Staging. Two call-tss contracts stand in for two bridges: each owns one key,
// so each is its own family. magi-5 is built from TSS_18AN_FAULT_SOURCE, a copy
// of this tree whose reshare round-1 message carries an empty VCommitment for
// keys owned by the contract named in /tmp/ban-fault-family, and only those --
// tss-lib's ValidateBasic then names magi-5 as the culprit. So magi-5 is blamed
// in every reshare of family A and in none of family B.
//
// Assertion. After magi-5 has at least TSS_BAN_MIN_FAMILY_BLAMES blames in
// family A (enough to ban it there), the next reshare of family B's key must
// still be held by magi-5.
//
// Control (TSS_18AN_GATE=off): the same run with the consensus floor below
// 0.13.0, i.e. the old global score. There the family-B reshare must EXCLUDE
// magi-5 -- the coupling 18an removes. If the control does not show it, this
// test is not discriminating and its pass means nothing.
//
// The fault tree: a worktree of this branch with the BAN-FAULT-FAMILY hook
// applied (never committed; kept as 012-postlaunch/18an-fault-source.patch):
//
//	git worktree add --detach ../go-vsc-node-18an-fault HEAD
//	git -C ../go-vsc-node-18an-fault apply 18an-fault-source.patch
//
// Results 2026-11-05: gate on PASS (2114 s) -- after 3 family-A blames the
// block-660 reshares were A: test1-4, B: test1-5. Control gate off OK
// (1622 s) -- after 2 family-A blames magi.test5 was excluded from BOTH.
//
//	TSS_18AN_FAULT_SOURCE=<tree> go test -v -run TestTss18anFamilyBanDevnet -timeout 120m ./tests/devnet/
func TestTss18anFamilyBanDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	faultSrc := os.Getenv("TSS_18AN_FAULT_SOURCE")
	if faultSrc == "" {
		t.Skip("TSS_18AN_FAULT_SOURCE not set (a copy of this tree with the /tmp/ban-fault-family devnet patch)")
	}
	gateOn := os.Getenv("TSS_18AN_GATE") != "off"
	requireDocker(t)

	bctx, bcancel := devnetWithTimeout(context.Background(), 10*time.Minute)
	wasmPath, err := BuildCallTssContract(bctx)
	bcancel()
	if err != nil {
		t.Fatalf("18an BLOCKED: building call-tss contract: %v", err)
	}

	const faulty = 5
	cfg := tssTestConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 1
	cfg.SkipFunding = false // two contracts are deployed
	cfg.OldCodeSourceDir = faultSrc
	cfg.OldCodeNodes = []int{faulty}
	cfg.OldCodeSysconfig = true
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	if gateOn {
		cp.ConsensusVersionFloorConsensus = 13 // TssPerKeyFamilyBanActive
	} else {
		cp.ConsensusVersionFloorConsensus = 12 // the old, global score
	}
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnetNoKey(t, cfg, 110*time.Minute)

	faultyAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, faulty)
	mode := "GATE ON (0.13.0, per-family score)"
	if !gateOn {
		mode = "CONTROL, gate off (0.12, global score)"
	}
	t.Logf("18an: %s; faulty witness %s", mode, faultyAcct)

	grepLogs := func(node int, pattern string) string {
		gctx, cancel := devnetWithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, _ := exec.CommandContext(gctx, "bash", "-c",
			fmt.Sprintf("docker logs %s 2>&1 | grep -E %q | tail -400", d.containerName(node), pattern)).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	named := func(c TssCommitmentDoc) []string {
		mem, _ := d.GetElectionMembers(ctx, 1, c.Epoch)
		bits := decodeBitset(t, c.Commitment)
		var out []string
		for j, m := range mem {
			if bits.Bit(j) == 1 {
				out = append(out, strings.TrimPrefix(m, "hive:"))
			}
		}
		return out
	}

	// ── 1. two families, one key each ─────────────────────────────────────
	if err := d.WaitForBlockProcessing(ctx, 2, 10, 5*time.Minute); err != nil {
		t.Fatalf("18an BLOCKED: node 2 never synced: %v", err)
	}
	deploy := func(name string) string {
		id, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasmPath, Name: name})
		if err != nil {
			t.Fatalf("18an BLOCKED: deploying %s: %v", name, err)
		}
		return id
	}
	famA := deploy("family-a-evm")
	famB := deploy("family-b-btc")
	if famA == famB {
		t.Fatalf("18an BLOCKED: both contracts got id %s", famA)
	}
	keyA, keyB := famA+"-keyA", famB+"-keyB"
	for _, k := range []struct{ contract, name string }{{famA, "keyA"}, {famB, "keyB"}} {
		if _, err := d.CallContract(ctx, 2, k.contract, "tssCreate", fmt.Sprintf(`{"key_name":%q,"epochs":50}`, k.name)); err != nil {
			t.Fatalf("18an BLOCKED: tssCreate %s: %v", k.name, err)
		}
	}
	for _, key := range []string{keyA, keyB} {
		if _, err := d.WaitForCommitment(ctx, 1, bson.M{"key_id": key, "type": "keygen"}, 20*time.Minute); err != nil {
			t.Fatalf("18an BLOCKED: no keygen for %s: %v", key, err)
		}
	}
	t.Logf("1. family A = %s (key %s), family B = %s (key %s); both keygens landed", famA, keyA, famB, keyB)

	// A new witness is exempt from bans for TSS_BAN_GRACE_PERIOD_EPOCHS. Wait it out
	// so the ban, when earned, can actually be applied.
	if err := d.waitForElectionEpoch(ctx, 1, 6, 15*time.Minute); err != nil {
		t.Fatalf("18an BLOCKED: never reached epoch 6: %v", err)
	}

	// Baseline: magi-5 holds family B's key before any fault.
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	baseB, err := d.WaitForCommitment(ctx, 1, bson.M{"key_id": keyB, "type": "reshare", "block_height": bson.M{"$gt": uint64(head)}}, 10*time.Minute)
	if err != nil {
		t.Fatalf("18an BLOCKED: no baseline reshare of %s: %v", keyB, err)
	}
	if !contains(named(*baseB), faultyAcct) {
		t.Fatalf("18an BLOCKED: %s is not even in the baseline reshare of %s (%v)", faultyAcct, keyB, named(*baseB))
	}
	t.Logf("1. baseline: reshare of %s at %d held by %v", keyB, baseB.BlockHeight, named(*baseB))

	// ── 2. fault in family A only, until magi-5 has enough blames there ───
	faultStart, _ := getHeadBlock(d.HiveRPCEndpoint())
	if out, err := exec.CommandContext(ctx, "docker", "exec", d.containerName(faulty), "sh", "-c",
		fmt.Sprintf("echo %s > /tmp/ban-fault-family", famA)).CombinedOutput(); err != nil {
		t.Fatalf("18an BLOCKED: arming the fault: %v %s", err, out)
	}
	t.Logf("2. %s faults on family A from block %d", faultyAcct, faultStart)

	// Gate on: TSS_BAN_MIN_FAMILY_BLAMES, the per-family floor. Gate off: the
	// global score has no floor, so magi-5 is banned EVERYWHERE after two blames
	// (2/2 > 60%) -- including from family A's own sessions, which means a third
	// family-A blame never comes. Two is what the old behaviour needs.
	needBlames := 3
	if !gateOn {
		needBlames = 2
	}
	var blamesA []TssCommitmentDoc
	last := uint64(faultStart)
	deadline := devnetNow().Add(40 * time.Minute)
	for len(blamesA) < needBlames && devnetNow().Before(deadline) {
		b, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "blame", "block_height": bson.M{"$gt": last}}, 15*time.Minute)
		if err != nil {
			break
		}
		last = b.BlockHeight
		who := named(*b)
		t.Logf("2. blame at %d for %s -> %v", b.BlockHeight, b.KeyId, who)
		if b.KeyId == keyA && contains(who, faultyAcct) {
			blamesA = append(blamesA, *b)
		}
	}
	if len(blamesA) < needBlames {
		t.Fatalf("18an BLOCKED: only %d family-A blames naming %s landed (need %d); fault log: %q",
			len(blamesA), faultyAcct, needBlames, lastLine(grepLogs(faulty, "BAN-FAULT-FAMILY")))
	}
	thirdBlame := blamesA[needBlames-1].BlockHeight
	t.Logf("2. %d family-A blames naming %s, the last at %d", len(blamesA), faultyAcct, thirdBlame)

	// PRECONDITION: the fault really was confined to family A. A fault line for
	// a family-B key, or a family-B blame naming magi-5, would make this a test
	// of something else.
	faultLines := grepLogs(faulty, "BAN-FAULT-FAMILY")
	if !strings.Contains(faultLines, keyA) {
		t.Fatalf("18an INCONCLUSIVE: %s logged no fault for %s", faultyAcct, keyA)
	}
	if strings.Contains(faultLines, keyB) {
		t.Fatalf("18an INCONCLUSIVE: the fault also fired for family B's key %s", keyB)
	}
	blamesB, _ := d.GetCommitments(ctx, 1, bson.M{"type": "blame", "key_id": keyB, "block_height": bson.M{"$gt": uint64(faultStart)}})
	for _, b := range blamesB {
		if contains(named(b), faultyAcct) {
			t.Fatalf("18an INCONCLUSIVE: a family-B blame at %d names %s; the fault was not confined to family A", b.BlockHeight, faultyAcct)
		}
	}
	t.Logf("2. precondition met: %d fault lines, all for %s; 0 family-B blames name %s",
		strings.Count(faultLines, "BAN-FAULT-FAMILY"), keyA, faultyAcct)

	// ── 3. the next reshares of BOTH families, after the ban is earned ────
	//
	// Bans are computed from on-chain blames when a session is planned, so the
	// first reshare whose planning saw the third blame is the one that counts:
	// skip one rotate boundary past it.
	after := uint64(nextReshareBoundary(int(thirdBlame)) + testRotateInterval)
	waitB := func() *TssCommitmentDoc {
		r, err := d.WaitForCommitment(ctx, 1, bson.M{"key_id": keyB, "type": "reshare", "block_height": bson.M{"$gt": after}}, 15*time.Minute)
		if err != nil {
			t.Fatalf("18an BLOCKED: no reshare of %s after %d: %v", keyB, after, err)
		}
		return r
	}
	rB := waitB()
	holdersB := named(*rB)
	t.Logf("3. reshare of family B (%s) at %d held by %v", keyB, rB.BlockHeight, holdersB)
	if rA, err := d.WaitForCommitment(ctx, 1, bson.M{"key_id": keyA, "type": "reshare", "block_height": bson.M{"$gt": after}}, 10*time.Minute); err == nil {
		t.Logf("3. reshare of family A (%s) at %d held by %v", keyA, rA.BlockHeight, named(*rA))
		if contains(named(*rA), faultyAcct) {
			t.Errorf("18an: %s still holds family A's key after %d blames there", faultyAcct, len(blamesA))
		}
	} else {
		t.Logf("3. no family-A reshare landed after %d (%v) -- informational", after, err)
	}
	t.Logf("3. ban decisions on magi-1 (last): %s", lastLine(grepLogs(1, "banned|ban cap|below the minimum blame")))

	inB := contains(holdersB, faultyAcct)
	switch {
	case gateOn && inB:
		t.Logf("18an PASS: with the per-family score, %s was blamed %d times in family A and still holds family B's key", faultyAcct, len(blamesA))
	case gateOn && !inB:
		t.Fatalf("18an FAIL: with the gate on, %s lost its family-B seat over family-A faults (holders %v)", faultyAcct, holdersB)
	case !gateOn && !inB:
		t.Logf("18an CONTROL OK: with the global score, %s's family-A faults cost it family B too (holders %v) -- the coupling 18an removes", faultyAcct, holdersB)
	default:
		t.Fatalf("18an CONTROL NOT DISCRIMINATING: with the gate off %s still holds family B's key (%v); the gate-on pass would prove nothing", faultyAcct, holdersB)
	}
}
