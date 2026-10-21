//go:build evm_devnet

package devnet

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// C2 (consensus 0.12.0) on a devnet of its own: a holder of the EVM bridge's
// vault key keeps its bond while it still holds a share. All five members hold
// the key; magi-4 and magi-5 stand down and go offline, so only 3 of the 5
// holders are left and the key (4 signers) cannot be reshared away from them:
// they stay its newest holders. Once their exit window has passed nothing else
// holds magi-4's bond (no seat, not electable, not in the committee, no BTC
// vault), so the only thing that can refuse its unstake is C2.
//
// EVM_CUSTODY_AB=off is the A/B arm: the nodes are not told the bridge's
// contract id, so C2 has no key to cover and the same unstake must go through.
// Run with EVM_CUSTODY_RUN=1 (own 5-node devnet).
func TestEVMBridge_C2_DepartedHolderBondHeld(t *testing.T) {
	if testing.Short() || os.Getenv("EVM_CUSTODY_RUN") == "" {
		t.Skip("set EVM_CUSTODY_RUN=1")
	}
	ruleOn := os.Getenv("EVM_CUSTODY_AB") != "off"
	const exitHalt = 40 // blocks (~2 min): the departed holders stop being "recently active" fast
	cfg := evmBridgeConfig()
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 12, 1
	cp.PoaExitHaltBlocks = exitHalt
	if cfg.SysConfigOverrides.TssParams == nil {
		cfg.SysConfigOverrides.TssParams = &params.TssParams{}
	}
	// The key is reshared at every rotate block whose epoch moved (elections
	// come every 20 blocks), so magi-4 and magi-5 must be offline before the
	// first rotate block of the election without them: 120 makes that block
	// land on the election's first block 1 time in 6, and the loop below
	// stops them within seconds of the election.
	cfg.SysConfigOverrides.TssParams.RotateInterval = 120
	d, ctx := startDevnetNoKey(t, cfg, 150*time.Minute)
	if err := bootstrapEvmOwner(ctx, d); err != nil {
		t.Fatal(err)
	}
	e := setupEvmBridgeOn(t, ctx, d)

	if d.cfg.SysConfigOverrides == nil {
		d.cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	if d.cfg.SysConfigOverrides.OracleParams == nil {
		d.cfg.SysConfigOverrides.OracleParams = &params.OracleParams{}
	}
	if ruleOn {
		d.cfg.SysConfigOverrides.OracleParams.EvmBridgeContracts = map[string]string{"ETH": e.amID}
	}
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		t.Fatal(err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 5*time.Minute, func() bool { _, _, err := d.LocalNodeInfo(ctx, e.gqlNode); return err == nil }); err != nil {
		t.Fatal("nodes did not come back after the config restart")
	}
	if err := waitForConsensus(ctx, d, "0.12", 10*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: %v", err)
	}

	if err := e.proposeExecute("registerRelayer", `{"account":"`+e.ownerAcct+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tssVault(25 * time.Minute); err != nil { // the key exists and is checked (E23)
		t.Fatal(err)
	}
	keyId := e.amID + "-primary"
	_, keygenEpoch, ok := custodyEpochs(d, ctx, 2, keyId)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: no keygen commitment for %s", keyId)
	}
	// magi-4 holds a share: it is in the activating keygen's participant bitset
	// (not merely in that epoch's election).
	holders, err := c2KeygenHolders(d, ctx, keyId, keygenEpoch)
	if err != nil || !contains(holders, d.witnessAccount(4)) {
		t.Fatalf("PRECONDITION FAILED: magi-4 is not a holder of the key's keygen (epoch %d, holders %v, err %v)", keygenEpoch, holders, err)
	}

	// magi-4 and magi-5 stand down, leave the election, then go offline.
	departed := []int{4, 5}
	e0 := custodyCurrentEpoch(d, ctx, 2)
	for _, n := range departed {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), cfg.Nodes)
	}
	out := false
	for end := devnetNow().Add(20 * time.Minute); devnetNow().Before(end) && !out; devnetSleep(3 * time.Second) {
		ep := custodyCurrentEpoch(d, ctx, 2)
		if ep <= e0 {
			continue
		}
		members, _ := d.GetElectionMembers(ctx, 2, ep)
		out = len(members) == 3 && !contains(members, d.witnessAccount(4)) && !contains(members, d.witnessAccount(5))
	}
	if !out {
		t.Fatal("PRECONDITION FAILED: the departing holders never left the election")
	}
	vfStopNodes(t, d, ctx, departed)
	defer vfStartNodes(t, d, ctx, departed)

	// Past their exit window, and the key not handed to the 3 (3 of its 5
	// holders cannot reshare it), so magi-4 is still one of its newest holders
	// (the set C2 binds: StateEngine.evmVaultHolders) and nothing else. The key
	// is reshared to the same five at every epoch before this, so "a reshare
	// exists" says nothing; the newest commitment's bitset does.
	devnetSleep(time.Duration(exitHalt*3+120) * time.Second)
	newest, holders, err := c2NewestHolders(d, ctx, keyId)
	if err != nil || !contains(holders, d.witnessAccount(4)) {
		t.Fatalf("PRECONDITION FAILED: magi-4 is not a holder of the key's newest commitment (%s at epoch %d, holders %v, err %v): the handover ran before the departed holders went offline", newest.Type, newest.Epoch, holders, err)
	}
	t.Logf("newest commitment: %s at epoch %d, holders %v", newest.Type, newest.Epoch, holders)
	acct := "hive:" + d.witnessAccount(4)
	before, err := d.GetAccountBalance(ctx, 1, acct)
	if err != nil || before == nil || before.HiveConsensus <= 0 {
		t.Fatalf("PRECONDITION FAILED: %s has no consensus bond to unstake (%+v, %v)", acct, before, err)
	}

	txId, err := d.ConsensusUnstake(4, "1.000")
	if err != nil {
		t.Fatalf("consensus_unstake broadcast: %v", err)
	}
	status := ""
	if err := pollUntil(ctx, 6*time.Minute, func() bool {
		status, _ = d.FindTransactionStatus(ctx, 1, txId)
		return status != "" && !strings.EqualFold(status, "UNCONFIRMED") && !strings.EqualFold(status, "INCLUDED")
	}); err != nil {
		t.Fatalf("the unstake %s was never processed (status %q): INCONCLUSIVE", txId, status)
	}
	after, err := d.GetAccountBalance(ctx, 1, acct)
	if err != nil || after == nil {
		t.Fatalf("re-reading %s: %v", acct, err)
	}
	t.Logf("unstake %s: status %s, hive_consensus %d -> %d", txId, status, before.HiveConsensus, after.HiveConsensus)
	if ruleOn {
		if !strings.EqualFold(status, "FAILED") || after.HiveConsensus != before.HiveConsensus {
			t.Fatalf("C2: the departed holder's unstake went through (status %s, %d -> %d) while it still holds a share of the EVM vault key", status, before.HiveConsensus, after.HiveConsensus)
		}
		t.Logf("C2 PASS: a departed holder of the EVM vault key keeps its bond")
		return
	}
	if !strings.EqualFold(status, "CONFIRMED") {
		t.Fatalf("A/B (rule unwired): the unstake was refused (status %s), so something other than C2 holds the bond and the rule-on arm proves nothing", status)
	}
	// A consensus_unstake that passes every gate is CONFIRMED at once, but its
	// bond debit lands with the ledger records written at its block's end, so
	// a read right after CONFIRMED can still show the old bond.
	if err := pollUntil(ctx, 6*time.Minute, func() bool {
		if b, err := d.GetAccountBalance(ctx, 1, acct); err == nil && b != nil {
			after = b
		}
		return after.HiveConsensus < before.HiveConsensus
	}); err != nil {
		t.Fatalf("A/B (rule unwired): the unstake was CONFIRMED but the bond never dropped (hive_consensus %d -> %d, unstaking %d -> %d)", before.HiveConsensus, after.HiveConsensus, before.ConsensusUnstaking, after.ConsensusUnstaking)
	}
	t.Logf("A/B PASS (rule unwired): the same unstake goes through (hive_consensus %d -> %d, unstaking %d -> %d), so C2 is what holds the bond", before.HiveConsensus, after.HiveConsensus, before.ConsensusUnstaking, after.ConsensusUnstaking)
}

// c2KeygenHolders decodes the participant bitset of keyId's keygen commitment
// at epoch against that epoch's election.
func c2KeygenHolders(d *Devnet, ctx context.Context, keyId string, epoch uint64) ([]string, error) {
	cs, err := d.GetCommitments(ctx, 2, bson.M{"key_id": keyId, "type": "keygen", "epoch": epoch})
	if err != nil || len(cs) == 0 {
		return nil, fmt.Errorf("no keygen commitment at epoch %d: %v", epoch, err)
	}
	return c2CommitmentHolders(d, ctx, cs[0])
}

// c2NewestHolders returns the key's newest keygen/reshare commitment (highest
// block) and the accounts its bitset names.
func c2NewestHolders(d *Devnet, ctx context.Context, keyId string) (TssCommitmentDoc, []string, error) {
	cs, err := d.GetCommitments(ctx, 2, bson.M{"key_id": keyId, "type": bson.M{"$in": []string{"keygen", "reshare"}}})
	if err != nil || len(cs) == 0 {
		return TssCommitmentDoc{}, nil, fmt.Errorf("no keygen/reshare commitment: %v", err)
	}
	holders, err := c2CommitmentHolders(d, ctx, cs[0])
	return cs[0], holders, err
}

// c2CommitmentHolders: the members of the commitment's epoch election whose
// bit is set in its participant bitset.
func c2CommitmentHolders(d *Devnet, ctx context.Context, c TssCommitmentDoc) ([]string, error) {
	members, err := d.GetElectionMembers(ctx, 2, c.Epoch)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Commitment)
	if err != nil {
		return nil, err
	}
	bits := new(big.Int).SetBytes(raw)
	var out []string
	for i, m := range members {
		if bits.Bit(i) == 1 {
			out = append(out, m)
		}
	}
	return out, nil
}
