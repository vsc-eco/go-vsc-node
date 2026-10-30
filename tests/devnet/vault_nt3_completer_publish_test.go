package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultNT3CompleterPublish — the fails-without-NT-3 devnet proof.
//
// NT-3 fix (modules/tss/tss.go:1621-1624): a sign result is published when
// `isLeader || !sr.leaderInParty`. Before it, ONLY the slot leader published. So if a
// retiring vault key's signing party is entirely OUTSIDE the current election, the
// witness-leader (always an election member) is never in the party, no one publishes the
// sign result, SignComplete is never applied, and the migration sweep strands — the
// retiring generation can never be drained, its BTC stuck.
//
// WHY THE TRIGGER IS A REAL PRODUCTION PATH (not the unstake churn the #11 bond lock blocks):
// disabling a witness (on-chain `enabled=false`) takes a funded key holder OUT of the
// election in-band, nodes still up, bond lock blind to it. On mainnet a node even
// auto-announces enabled=false when its reachability is not public (announcements.go:300),
// so this is a natural state, not only a deliberate act. A RETIRING key does NOT reshare
// (vaultrotation/eligibility.go ComputeRetiringSignerSet puts Retiring/Draining/Inactive/
// Purged in ReshareSkipKeyIds — "only the single Active gen reshares"), so a retired gen
// stays with its original party and V-A keeps that party bound to sign it from outside the
// election.
//
// ORDER IS LOAD-BEARING: gen-0 must RETIRE while the election is still {1..5}. If the
// election churns while gen-0 is still ACTIVE, gen-0 reshares to {6,7,8}, its party then
// contains the leader, and the test passes even on the mutant (vacuous). So rotate
// gen-0 -> gen-1 on {1..5} FIRST (gen-0 retires), THEN churn the election to {6,7,8}.
//
// ANNOUNCE OVERWRITE: announce() has no dedup — every node startup
// broadcasts enabled=true on devnet (OnDevnet, announcements.go:300), which is indexed and
// overwrites a mongo disable. So the ONLY RestartAllMagiNodes runs BEFORE any disable, and
// after it we wait for all 8 rows to re-index before disabling 6-8.
//
// POA STAGGER: POA may be live at floor 11 (PoaFlatWeightActive && seatGateApplied).
// With BondInclusionActive false on devnet a re-enabled witness is a NEW entrant admitted
// one per election. So the churn is TWO awaited phases (enable 6-8 -> {1..8}, then disable
// 1-5 -> {6,7,8}), never a simultaneous swap that could yield a 1-member or refused election.
// Non-vacuity holds regardless: gen-0 already retired on {1..5}; gen-1 reshares
// {1..5}->{1..8}->{6,7,8} and the 0.11 rule keeps the departed holders in gen-0's old party.
//
// A/B (same test, two node binaries):
//   - ship tree (NT-3 present): a completer in {1..5} publishes -> sweep signs -> gen-0 drains. PASS.
//   - mutated COPY (remove `|| !sr.leaderInParty` at tss.go:1624): no publish -> sweep
//     strands -> gen-0 never drains. FAIL. Mutate in a copy only; revert by re-copy. On the
//     mutant strand the test greps nodes 1-5 for the sign session so the FAIL reads as
//     "signed but never published", not merely "never signed".
//
// ASSERTIONS ("exactly one publisher" is WRONG — leader-absent = every completer
// publishes by design; duplicates collapse because tss_requests is an idempotent upsert on
// (key_id,msg)):
//
//	NT3-SIGN   every sweep sighash gets signed though the leader is out of the party.
//	NT3-PUB    the publish-broadcast delta is ≥1 on a holder in {1..5} and 0 on every leader
//	           {6,7,8} — direct positive evidence the completer path fired.
//	NT3-XNODE  the landed signature for each sighash is byte-identical on all 8 nodes.
//	NT3-DRAIN  gen-0 drains to 0 UTXOs (the funds actually move).
//	NT3-CREDIT gen-1's UTXO count rises BY THE SWEEP (gen1Before read after the fee reserve
//	           is credited, so the fee-reserve deposit cannot stand in for a sweep credit).
//
// Load: 8 nodes is heavier than the 5-node KPX runs. Keep DEVNET_TIMEOUT_SCALE=1.6. A TSS
// timeout here may be CPU starvation, not a finding — check the box before concluding.
//
//	VAULT_NT3_RUN=1 BTC_MAPPING_WASM_PATH=... go test -v -run TestVaultNT3CompleterPublish -timeout 75m ./tests/devnet/
func TestVaultNT3CompleterPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_NT3_RUN") == "" {
		t.Skip("set VAULT_NT3_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(70*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm")
	}

	const hpin = 400 // v2 activation AFTER genesis, so gen-0 mints on the v2-off path
	cfg := vfSlowReshareConfig()
	cfg.Nodes = 8
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 67*time.Minute)

	pass, fail := 0, 0
	rec := func(id, desc string, ok bool, detail string) {
		if ok {
			pass++
			t.Logf("CASE %s PASS — %s | %s", id, desc, detail)
		} else {
			fail++
			t.Errorf("CASE %s FAIL — %s | %s", id, desc, detail)
		}
	}
	leaders := []int{6, 7, 8}       // the committee gen-0's party is NOT in
	holders := []int{1, 2, 3, 4, 5} // gen-0's keygen committee / party
	acctsOf := func(ns []int) []string {
		out := make([]string, 0, len(ns))
		for _, n := range ns {
			out = append(out, d.witnessAccount(n))
		}
		return out
	}

	waitMembers := func(tag string, want []int, within time.Duration) []string {
		t.Helper()
		wantAccts := map[string]bool{}
		for _, n := range want {
			wantAccts[d.witnessAccount(n)] = true
		}
		var members []string
		deadline := devnetNow().Add(within)
		for devnetNow().Before(deadline) {
			ep := custodyCurrentEpoch(d, ctx, 2)
			members, _ = d.GetElectionMembers(ctx, 2, ep)
			if len(members) == len(want) {
				ok := true
				for _, m := range members {
					if !wantAccts[m] {
						ok = false
						break
					}
				}
				if ok {
					return members
				}
			}
			devnetSleep(20 * time.Second)
		}
		t.Fatalf("PRECONDITION FAILED (%s): election never became %v (last=%v)", tag, want, members)
		return members
	}

	// ── deploy FIRST, then the one restart, so no later restart re-announces 6-8 enabled ──
	waitMembers("startup", []int{1, 2, 3, 4, 5, 6, 7, 8}, vfTestBudget(12*time.Minute))
	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "nt3 completer-publish", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s hpin=%d", cid, hpin)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx) // the ONLY restart; BEFORE any disable
	devnetSleep(10 * time.Second)
	// Wait for the post-restart enabled=true announces of all 8 to be indexed BEFORE
	// disabling — otherwise a late announce overwrites the disable and 6-8 slip back in.
	waitMembers("post-restart all 8 indexed", []int{1, 2, 3, 4, 5, 6, 7, 8}, vfTestBudget(10*time.Minute))

	// POA visibility: flat weights == POA live. Logged, not asserted.
	if e2, gerr := d.GetElectionGQL(ctx, 1, custodyCurrentEpoch(d, ctx, 2)); gerr == nil {
		t.Logf("POA visibility: current-epoch election weights = %v (flat => POA seat gate live)", e2.Weights)
	}

	// ── take 6,7,8 out so gen-0 keygens on {1..5} ──
	for _, n := range leaders {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), 8)
	}
	waitMembers("gen-0 committee = {1..5}", holders, vfTestBudget(15*time.Minute))

	// ── gen-0 keygen on {1..5} + register + fund (2 UTXOs) ──
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 10*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	primary0 := kd0.PublicKey
	vfWaitPreparams(t, d, ctx, 12*time.Minute)
	if s := vfRegisterGenesis(t, d, ctx, 1, cid,
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 60_000_000, seedH)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 40_000_000, contractLastHeight(t, d, ctx, cid))
	n0 := genUtxoCount(t, d, ctx, cid, 0)
	for i := 0; i < 18 && n0 < 2; i++ {
		devnetSleep(10 * time.Second)
		n0 = genUtxoCount(t, d, ctx, cid, 0)
	}
	rec("NT3-00", "gen-0 funded with multiple UTXOs on committee {1..5}", n0 >= 2, fmt.Sprintf("gen-0 holds %d UTXO(s)", n0))
	if n0 < 2 {
		return
	}

	// ── rotate gen-0 -> gen-1 WHILE the election is still {1..5}, so gen-0 RETIRES on
	//    {1..5} and (ReshareSkip) never follows the later churn. ──
	vfWaitV2On(t, d, ctx, uint64(hpin))
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd1, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-mainv1", "status": "active"}, 12*time.Minute)
	if err != nil {
		t.Fatalf("gen1 keygen (on {1..5}): %v", err)
	}
	primary1 := kd1.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary1, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen1 register: %s", s)
	}
	activated := false
	for i := 0; i < 20 && !activated; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			activated = true
			break
		}
		devnetSleep(15 * time.Second)
	}
	if !activated {
		t.Fatal("gen-1 never activated")
	}
	g0 := -1
	for i := 0; i < 8 && g0 < 2; i++ {
		g0 = vaultStatusOf(t, d, ctx, cid, 0)
		devnetSleep(10 * time.Second)
	}
	rec("NT3-01", "gen-0 RETIRED while election still {1..5} (will not reshare away)", g0 >= 2, "gen-0 status="+statusStr(g0))
	if g0 < 2 {
		return
	}

	// ── churn to the disjoint committee {6,7,8} in TWO awaited phases (POA-safe). ──
	for _, n := range leaders {
		enableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), 8)
	}
	waitMembers("phase-1 election = {1..8}", []int{1, 2, 3, 4, 5, 6, 7, 8}, vfTestBudget(20*time.Minute))
	for _, n := range holders {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), 8)
	}
	newMembers := waitMembers("phase-2 election = {6,7,8}", leaders, vfTestBudget(20*time.Minute))
	rec("NT3-02", "election churned to {6,7,8}, disjoint from gen-0's party {1..5}", true, fmt.Sprintf("election=%v", newMembers))

	// ── PRECONDITIONS: prove the party is genuinely disjoint from the election. ──
	g0KeyEpoch, g0KeygenEpoch, _ := custodyEpochs(d, ctx, 2, cid+"-main")
	g0CommitMembers, _ := d.GetElectionMembers(ctx, 2, g0KeygenEpoch)
	disjoint := true
	for _, hm := range acctsOf(holders) {
		if contains(newMembers, hm) {
			disjoint = false
		}
	}
	partyIsHolders := len(g0CommitMembers) == len(holders)
	for _, m := range g0CommitMembers {
		if !contains(acctsOf(holders), m) {
			partyIsHolders = false
		}
	}
	rec("NT3-PRE", "gen-0's party == {1..5} and current election ∩ {1..5} = ∅ (non-vacuity)",
		disjoint && partyIsHolders,
		fmt.Sprintf("gen-0 keygen-epoch=%d members=%v (keyEpoch=%d); election=%v", g0KeygenEpoch, g0CommitMembers, g0KeyEpoch, newMembers))
	if !(disjoint && partyIsHolders) {
		return
	}

	// ── fund the fee reserve and settle it into gen-1 FIRST, so the NT3-CREDIT baseline
	//    (gen1Before) already includes it and the later delta is purely the sweep. ──
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	gen1Before := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	for i, stable := 0, 0; i < 18 && stable < 2; i++ { // wait for the fee-reserve UTXO to land + settle
		devnetSleep(10 * time.Second)
		now := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
		if now == gen1Before {
			stable++
		} else {
			gen1Before, stable = now, 0
		}
	}
	t.Logf("NT3-CREDIT baseline: gen-1 holds %d UTXO(s) after the fee reserve settled (pre-sweep)", gen1Before)

	// ── THE NT-3 SEAM: drive the migration sweep. gen-0's party {1..5} signs it (V-A)
	//    while every slot leader ∈ {6,7,8}; a completer in {1..5} must publish. ──
	// NT3-PUB instrument: snapshot the two log markers BEFORE the sweep. After the churn
	// the only new sign activity is gen-0's sweep, so the deltas are attributable to it.
	//   markStart "starting sign party"            (dispatcher.go:1220, trace) = a session ran.
	//   markPub   "tss_sign: signatures broadcast" (tss.go:1703, debug)       = a node published.
	// Devnet runs LogLevel "error,tss=trace", so both markers are present.
	const markStart = "starting sign party"
	const markPub = "tss_sign: signatures broadcast"
	nodes8 := []int{1, 2, 3, 4, 5, 6, 7, 8}
	startBefore, pubBefore := map[int]int{}, map[int]int{}
	for _, n := range nodes8 {
		startBefore[n] = vfCountLogs(d, ctx, n, markStart)
		pubBefore[n] = vfCountLogs(d, ctx, n, markPub)
	}

	txid, sd, s := vfBuildSweep(t, d, ctx, 1, cid)
	if txid == "" || sd == nil {
		rec("NT3-SIGN", "migration sweep built for the retiring gen", false, fmt.Sprintf("no sweep pending-spend appeared (migrateVault=%s) — migration gate (gen-1 reshared?) or starvation", s))
		return
	}
	t.Logf("migration sweep txid=%s, %d input(s)", txid, len(sd.UnsignedSigHashes))

	raw, ok := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
	if !ok {
		// Give the completer a few more sign intervals before declaring a strand, then
		// re-check rather than assume the first per-input wait was enough.
		devnetSleep(vfTestBudget(3 * time.Minute))
		stillUnsigned := false
		for _, uh := range sd.UnsignedSigHashes {
			if !vfSignatureLanded(d, ctx, 2, cid+"-main", uh.SigHash) {
				stillUnsigned = true
				break
			}
		}
		if !stillUnsigned {
			raw, ok = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
		}
	}

	// Deltas after the sign attempt has resolved.
	startDelta, pubDelta := map[int]int{}, map[int]int{}
	holderRan, holderPub, leaderPub, totalPub := 0, 0, 0, 0
	for _, n := range nodes8 {
		startDelta[n] = vfCountLogs(d, ctx, n, markStart) - startBefore[n]
		pubDelta[n] = vfCountLogs(d, ctx, n, markPub) - pubBefore[n]
		totalPub += pubDelta[n]
	}
	for _, n := range holders {
		if startDelta[n] > 0 {
			holderRan++
		}
		if pubDelta[n] >= 1 {
			holderPub++
		}
	}
	for _, n := range leaders {
		if pubDelta[n] >= 1 {
			leaderPub++
		}
	}

	rec("NT3-SIGN", "retiring gen-0 sweep SIGNED though every slot leader is outside its party (NT-3 completer-publish)",
		ok, fmt.Sprintf("all %d input(s) signed=%v", len(sd.UnsignedSigHashes), ok))
	if !ok {
		// Causal strand evidence: the sign session ran on gen-0's party (>0) but nobody
		// published (0 broadcasts anywhere) = "signed, never published" — the exact pre-NT-3
		// failure, not merely "never signed".
		t.Logf("STRAND EVIDENCE: gen-0 sign session started on %d/%d holders; publishes across all 8 nodes=%d (expect sessions>0, publishes=0)", holderRan, len(holders), totalPub)
		vfDumpRegistry(t, d, ctx, 2, cid, "NT-3 STRAND: sweep signed internally but never published (leader not in party)")
		return
	}
	// NT3-PUB (ship-side positive): a completer IN gen-0's party {1..5} published, and NO
	// leader {6,7,8} did — direct evidence the `!leaderInParty` completer path fired.
	rec("NT3-PUB", "a completer in {1..5} published the sweep result and no leader {6,7,8} did",
		holderPub >= 1 && leaderPub == 0,
		fmt.Sprintf("holders published=%d (want ≥1), leaders published=%d (want 0), holder sessions ran=%d", holderPub, leaderPub, holderRan))

	// NT3-XNODE: each sighash's landed signature is byte-identical on all 8 nodes.
	xnodeOK, xnodeDetail := true, ""
	for _, uh := range sd.UnsignedSigHashes {
		var ref string
		for _, node := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
			reqs, rerr := d.GetTssRequests(ctx, node, cid+"-main")
			if rerr != nil {
				xnodeOK, xnodeDetail = false, fmt.Sprintf("getTssRequests node %d: %v", node, rerr)
				break
			}
			got := ""
			for _, r := range reqs {
				if r.Msg == hex.EncodeToString(uh.SigHash) && r.Sig != "" {
					got = r.Sig
					break
				}
			}
			if got == "" {
				xnodeOK, xnodeDetail = false, fmt.Sprintf("node %d has no sig for a sweep sighash", node)
				break
			}
			if ref == "" {
				ref = got
			} else if got != ref {
				xnodeOK, xnodeDetail = false, fmt.Sprintf("node %d sig differs from reference", node)
				break
			}
		}
		if !xnodeOK {
			break
		}
	}
	rec("NT3-XNODE", "each sweep signature byte-identical across all 8 nodes (one agreed sig; publishers collapse)", xnodeOK, xnodeDetail)

	// ── broadcast + settle this tranche, then drive remaining tranches until gen-0 drains ──
	// migrateVault (via vfBuildSweep) emits ONE sweep tranche per call, so a retiring gen with
	// more than one UTXO needs one tranche per batch (F21 shows the same multi-tranche drain).
	// Tranche 1 above already proved the NT-3 seam (SIGN/PUB/XNODE) with every slot leader
	// outside the party; here we just drive to FULL drain so NT3-DRAIN is not vacuous for a
	// >1-UTXO gen. Bounded, so a genuine strand (the mutant) still fails fast rather than looping.
	settle := func(rawHex string) bool {
		bcTxid, h, berr := vfBroadcastAndMine(t, d, ctx, rawHex)
		if berr != nil {
			rec("NT3-BCAST", "signed sweep broadcasts to regtest", false, berr.Error())
			return false
		}
		tipAfter, _ := d.MineBlocks(ctx, vfDepositMaturityBlocks)
		for hh := contractLastHeight(t, d, ctx, cid) + 1; hh <= tipAfter; hh++ {
			hx, _ := btcBlockHeaderHex(ctx, d, hh)
			vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
		}
		vfConfirmSpendOnly(t, d, ctx, 1, cid, bcTxid, h, 0)
		return true
	}
	if !settle(raw) {
		return
	}
	tranches := 1
	left := genUtxoCount(t, d, ctx, cid, 0)
	for i := 0; i < 5 && left > 0; i++ {
		txN, sdN, sN := vfBuildSweep(t, d, ctx, 1, cid)
		if txN == "" || sdN == nil {
			t.Logf("drain loop: no further sweep tranche (migrateVault=%s); gen-0 still holds %d UTXO(s)", sN, left)
			break
		}
		rawN, okN := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sdN)
		if !okN {
			t.Logf("drain loop: tranche %d (%d input(s)) did not fully sign — treating as strand", tranches+1, len(sdN.UnsignedSigHashes))
			break
		}
		t.Logf("drain loop: tranche %d signed (%d input(s)), settling", tranches+1, len(sdN.UnsignedSigHashes))
		if !settle(rawN) {
			return
		}
		tranches++
		left = genUtxoCount(t, d, ctx, cid, 0)
	}

	left = f8WaitGenDrained(t, d, ctx, cid, 0, vfTestBudget(6*time.Minute))
	rec("NT3-DRAIN", "gen-0 drained to 0 UTXOs across all tranches — funds moved, no strand", left == 0, fmt.Sprintf("gen-0 holds %d UTXO(s) after %d sweep tranche(s)", left, tranches))

	gen1After := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	rec("NT3-CREDIT", "gen-1 successor credited BY THE SWEEP (baseline already had the fee reserve)", gen1After > gen1Before, fmt.Sprintf("gen-1 UTXOs %d -> %d", gen1Before, gen1After))

	t.Logf("NT3 SUMMARY: %d PASS %d FAIL CONTRACT=%s sweep=%s tranches=%d", pass, fail, cid, txid, tranches)
}
