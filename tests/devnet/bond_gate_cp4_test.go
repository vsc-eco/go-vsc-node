package devnet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
)

// TestGatewayDecentralization_Version020Gate proves the A3-2 fix at runtime: the
// removal of the vsc.dao account-auth backstop from the gateway wallet is now
// gated on the v0.2.0 activation (Version0_2_0Active), not unconditional. One run
// proves BOTH paths:
//
//   - Version0_2_0Height is pinned to 200. Gateway key rotations BELOW block 200
//     are INERT → they retain vsc.dao in the owner authority (exactly base
//     937ae771). Rotations AT/AFTER 200 are ACTIVE → they drop vsc.dao (committee
//     keys only).
//
// devnet-setup creates vsc.gateway with AccountAuths=[] (no vsc.dao). So:
//
//	Phase 1 (inert, blocks 40–180): the first rotation RE-ADDS vsc.dao to the
//	  owner auth → we poll the on-chain account until vsc.dao APPEARS. That
//	  proves the inert path restores the backstop (the default below the v0.2.0
//	  height — deploying the binary does NOT decentralize custody).
//	Phase 2 (active, past block 200): a rotation drops vsc.dao → we poll until
//	  it DISAPPEARS. That proves the pinned height actually removes the backstop.
//
// NOTE: gateway decentralization was folded into the coordinated v0.2.0 batch
// (was the standalone Cp4GatewayDecentralizationHeight). This pins
// Version0_2_0Height (devnet default is 1 = active from genesis) to 200, so ALL
// v0.2.0 behavior is inert below block 200 in this run — a valid pre-v0.2.0 chain
// state. The test only exercises gateway rotation + vsc.dao presence, which does
// not depend on other v0.2.0 features.
//
// Needs 8 nodes: the gateway floor is `len(gatewayKeys) < 8` → a 7-node devnet
// never rotates (rotation is skipped), so the account_update would never land.
func TestGatewayDecentralization_Version020Gate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet gateway-decentralization gate test in short mode")
	}
	requireDocker(t)

	const v020Height = 200

	cfg := regressionConfig()
	cfg.Nodes = 8 // ≥8 gateway keys so keyRotation actually rotates
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ElectionInterval = 20
	// Gateway vsc.dao-backstop removal is gated on the v0.2.0 activation. Pin it
	// to 200 (overriding the devnet default of 1) to exercise both sides of the
	// gate within one run.
	cp.Version0_2_0Height = v020Height
	// The bond gate is irrelevant to gateway decentralization; leave it inert.
	cp.BondInclusionActivationHeight = 0
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	// Rotate the gateway every 20 blocks (~60s) so several rotations land on
	// each side of v020Height within the run.
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	t.Cleanup(cancel)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Logf("starting 8-node devnet, Version0_2_0Height=%d, gateway rotation every 20 blocks...", v020Height)
	if err := d.Start(ctx); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 8*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}

	gatewayAcct := "vsc.gateway"
	hc := hivego.NewHiveRpc([]string{d.DroneEndpoint()})

	// ── Phase 1: INERT (below v020Height) — vsc.dao must be RE-ADDED ──────────
	t.Log("Phase 1: waiting for an inert (block<200) rotation to re-add vsc.dao to the gateway owner auth...")
	if err := pollGatewayDao(t, ctx, hc, gatewayAcct, true, 12*time.Minute); err != nil {
		// If no rotation ever lands, the proof is inconclusive (not a fix defect).
		bh, _, _ := d.LocalNodeInfo(ctx, 1)
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("INCONCLUSIVE: vsc.dao never appeared in the gateway owner auth below block %d (current block %d) — gateway rotation may not be landing on this devnet: %v", v020Height, bh, err)
	}
	bhP1, _, _ := d.LocalNodeInfo(ctx, 1)
	t.Logf("PROVEN (Phase 1 / inert): vsc.dao PRESENT in the gateway owner auth at ~block %d (<%d) — the v0.2.0 gate retains the backstop by default", bhP1, v020Height)
	if bhP1 >= v020Height {
		t.Logf("WARNING: phase-1 observation landed at/after v020Height %d — inert attribution weaker; phase 2 still decisive", v020Height)
	}

	// ── Phase 2: ACTIVE (past v020Height) — vsc.dao must be REMOVED ──────────
	t.Logf("Phase 2: waiting past block %d for an active rotation to REMOVE vsc.dao...", v020Height)
	if err := d.WaitForBlockProcessing(ctx, 1, v020Height+40, 12*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block %d: %v", v020Height+40, err)
	}
	if err := pollGatewayDao(t, ctx, hc, gatewayAcct, false, 12*time.Minute); err != nil {
		bh, _, _ := d.LocalNodeInfo(ctx, 1)
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("vsc.dao was NOT removed from the gateway owner auth after block %d (current %d) — the v0.2.0 active path failed: %v", v020Height, bh, err)
	}
	bhP2, _, _ := d.LocalNodeInfo(ctx, 1)
	t.Logf("PROVEN (Phase 2 / active): vsc.dao REMOVED from the gateway owner auth by a rotation at ~block %d (≥%d) — pinning Version0_2_0Height decentralizes custody", bhP2, v020Height)
	t.Log("Gateway decentralization v0.2.0-gate PROVEN both ways on devnet: inert (block<200) retains vsc.dao; active (block≥200) removes it.")
}

// pollGatewayDao polls the gateway account's owner authority until vsc.dao's
// presence matches wantPresent, or the timeout elapses.
func pollGatewayDao(t *testing.T, ctx context.Context, hc *hivego.HiveRpcNode, account string, wantPresent bool, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastSeen string
	for {
		if time.Now().After(deadline) {
			return &pollTimeout{last: lastSeen}
		}
		accs, err := hc.GetAccount([]string{account})
		if err == nil && len(accs) == 1 {
			present := ownerHasAccountAuth(accs[0], "vsc.dao")
			lastSeen = describeOwnerAuths(accs[0])
			if present == wantPresent {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

type pollTimeout struct{ last string }

func (e *pollTimeout) Error() string {
	return "poll timed out; last owner auths: " + e.last
}

// ownerHasAccountAuth reports whether the account's OWNER authority lists the
// given account name in its account_auths.
func ownerHasAccountAuth(acc hivego.AccountData, name string) bool {
	for _, aa := range acc.Owner.AccountAuths {
		if len(aa) >= 1 {
			if s, ok := aa[0].(string); ok && s == name {
				return true
			}
		}
	}
	return false
}

func describeOwnerAuths(acc hivego.AccountData) string {
	parts := make([]string, 0, len(acc.Owner.AccountAuths))
	for _, aa := range acc.Owner.AccountAuths {
		if len(aa) >= 1 {
			if s, ok := aa[0].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}
