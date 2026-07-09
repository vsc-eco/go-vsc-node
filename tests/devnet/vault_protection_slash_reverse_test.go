package devnet

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"

	"github.com/vsc-eco/hivego"
)

// protocolHeldHiveGQL sums the net HIVE held on the protocol slash accounts
// (reserve + pending-burn) via GQL — the exact quantity ledgerLiabilitySource.
// protocolHeldHive adds to the solvency monitor's expected HIVE liability. Signed
// sum, so a pending-release (cancel) row nets its pending credit back to zero.
func protocolHeldHiveGQL(t *testing.T, d *Devnet, node int) int64 {
	t.Helper()
	var total int64
	for _, acct := range []string{"system:protocol_slash_reserve", "system:protocol_slash_burn_pending"} {
		for _, r := range findLedgerTXs(t, d.GQLEndpoint(node), acct, nil) {
			if r.Asset == "hive" && r.Owner == acct {
				total += r.Amount
			}
		}
	}
	return total
}

// TestVaultProtectionSolvencyInvariantOnSlashReverse proves Milo's HIVE-leg
// no-double-count invariant for the solvency monitor (fix 1). The quantity the
// monitor treats as expected HIVE — the slashed account's bond (HIVE_CONSENSUS)
// PLUS protocolHeldHive (reserve + pending residual) — must be INVARIANT across a
// safety slash and its governance reverse.
//
// A slash moves X from the bond bucket to the pending bucket (both counted); the
// reverse ("both") must move it back — re-credit the bond AND cancel the pending.
// If the reverse re-credited the bond WITHOUT cancelling the pending,
// protocolHeldHive would double-count X and the monitor's expected liability would
// jump by X — which would MASK a real X shortfall (or, with a stale gap,
// false-halt the gateway). This test slashes node-3 by double-signing, reverses
// one incident via gateway multisig, and asserts bond+protocolHeld is unchanged.
func TestVaultProtectionSolvencyInvariantOnSlashReverse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	const (
		attacker = "magi.test3"
		honest   = "magi.test1"
	)

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"VSC_DOUBLE_SIGN_ACCOUNT":  attacker,
		"VSC_DOUBLE_SIGN_ONCE":     "1",
		"VSC_SLASH_BURN_DELAY":     "600", // long: residual stays PENDING through the reverse window
		"DEVNET_DETERMINISTIC_BLS": "1",   // deterministic gateway keys for re-derivation
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	baseH := waitForStableBond(t, d, "hive:"+honest, 8*time.Minute)
	t.Logf("[inv] honest baseline bond=%d", baseH)

	// 1) Wait for the double-sign slash to drop node-3's bond below the baseline.
	slashDeadline := time.Now().Add(10 * time.Minute)
	var cur3 int64 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	for time.Now().Before(slashDeadline) {
		cur3 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if cur3 >= 0 && cur3 < baseH {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !(cur3 >= 0 && cur3 < baseH) {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[inv] slash never fired (baseH=%d cur=%d)", baseH, cur3)
	}
	t.Logf("[inv] slashed: bond now=%d; ledger slash-debit total=%d", cur3, sumSlashDebits(t, d, "hive:"+attacker))

	// 2) Pick one slash row to reverse (prefer the double_block_sign incident).
	var slashRow ledgerRecord
	var slashRowKind string
	rowDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(rowDeadline) {
		var best ledgerRecord
		var bestKind string
		for _, r := range findLedgerTXs(t, d.GQLEndpoint(1), "hive:"+attacker, []string{"safety_slash_consensus"}) {
			if r.Amount >= 0 || r.Asset != "hive_consensus" {
				continue
			}
			k := evidenceKindFromSlashRowID(r.Id)
			if k == "" {
				continue
			}
			preferNew := best.TxId == "" ||
				(k == safetyslash.EvidenceVSCDoubleBlockSign && bestKind != safetyslash.EvidenceVSCDoubleBlockSign) ||
				((k == safetyslash.EvidenceVSCDoubleBlockSign) == (bestKind == safetyslash.EvidenceVSCDoubleBlockSign) && -r.Amount > -best.Amount)
			if preferNew {
				best, bestKind = r, k
			}
		}
		if best.TxId != "" {
			slashRow, slashRowKind = best, bestKind
			break
		}
		time.Sleep(3 * time.Second)
	}
	if slashRow.TxId == "" {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[inv] no slash row found for %s", attacker)
	}
	rowSlashedAmt := -slashRow.Amount

	// 3) MEASURE the invariant BEFORE the reverse: bond + protocolHeldHive.
	bondPre := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	heldPre := protocolHeldHiveGQL(t, d, 1)
	combinedPre := bondPre + heldPre
	t.Logf("[inv] PRE-reverse: bond=%d protocolHeld=%d combined=%d (reversing amount=%d kind=%s)",
		bondPre, heldPre, combinedPre, rowSlashedAmt, slashRowKind)

	// 4) Broadcast the gateway-multisig reverse (action "both" = cancel pending +
	// re-credit bond). Threshold = 6*2/3 = 4, so any 4 gateway keys satisfy it.
	payload, _ := json.Marshal(safetyslash.SafetySlashReverseRecord{
		SlashTxID: slashRow.TxId, EvidenceKind: slashRowKind, SlashedAccount: attacker,
		Action: safetyslash.ReverseActionBoth, Amount: rowSlashedAmt,
		Reason: "devnet solvency-invariant slash reverse",
	}.Normalize())
	allKeys := make([]*hivego.KeyPair, 0, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		allKeys = append(allKeys, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", cfg.WitnessPrefix, n)))
	}
	txID, err := broadcastGatewayMultisig(t, d, "vsc.safety_slash_reverse",
		[]string{"vsc.gateway"}, string(payload), allKeys[:4])
	if err != nil {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[inv] broadcast reverse: %v", err)
	}
	t.Logf("[inv] broadcast vsc.safety_slash_reverse tx=%s", txID)

	// 5) Wait for the bond to rise by the reversed incident's amount.
	wantBond := bondPre + rowSlashedAmt
	restored := false
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		if gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker) >= wantBond {
			restored = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !restored {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 60)
		t.Fatalf("[inv] bond never restored to %d after reverse", wantBond)
	}

	// 6) MEASURE the invariant AFTER the reverse. Poll until it settles — the
	// pending-cancel row lands a slot or two after the bond credit.
	var combinedPost, bondPost, heldPost int64
	settled := false
	for invDeadline := time.Now().Add(4 * time.Minute); time.Now().Before(invDeadline); {
		bondPost = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		heldPost = protocolHeldHiveGQL(t, d, 1)
		combinedPost = bondPost + heldPost
		if combinedPost == combinedPre {
			settled = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("[inv] POST-reverse: bond=%d protocolHeld=%d combined=%d (want=%d)", bondPost, heldPost, combinedPost, combinedPre)

	if !settled {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 60)
		t.Fatalf("[inv] SOLVENCY INVARIANT VIOLATED across slash reverse: bond+protocolHeld changed by %d "+
			"(pre=%d post=%d). A non-zero delta means the reverse re-credited the bond WITHOUT cancelling the "+
			"pending/reserve — protocolHeldHive double-counts and the solvency monitor's expected liability is wrong.",
			combinedPost-combinedPre, combinedPre, combinedPost)
	}
	// Sanity: the reverse actually moved value (bond up, protocol-held down by the same).
	if bondPost <= bondPre || heldPost >= heldPre {
		t.Fatalf("[inv] reverse did not move value as expected: bond %d->%d, protocolHeld %d->%d", bondPre, bondPost, heldPre, heldPost)
	}
	t.Logf("[inv] PASS: bond+protocolHeld invariant held across slash reverse (%d): bond rose %d->%d, protocolHeld fell %d->%d — no double-count",
		combinedPre, bondPre, bondPost, heldPre, heldPost)
}
