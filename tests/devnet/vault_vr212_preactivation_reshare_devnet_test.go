package devnet

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"go.mongodb.org/mongo-driver/bson"
)

// vault_vr212_preactivation_reshare_devnet_test.go: VR2-12 (consensus 0.12.0) on a
// real five-node devnet. A pending generation's key is reshared once its
// check-signature has verified, BEFORE activation, while the old generation stays
// ACTIVE and serves withdrawals; the contract refuses to activate a rotation until
// that reshare has landed. So migration starts at activation with no epoch wait and
// the only window users see is the sweep.
//
//	VR212-GATE       activateKey is refused (VR2-12) while the check-signed key has
//	                 not been reshared; the old generation stays active.
//	VR212-SERVE      a withdrawal from the old generation is signed, broadcast and
//	                 settled while the new generation is pending.
//	VR212-RESHARE    the pending key is reshared (epoch > keygen epoch) while it is
//	                 still pending; its public key and check-signature flag survive.
//	VR212-ACTIVATE   activateKey then succeeds.
//	VR212-MIGRATE    the first migrateVault right after activation builds the sweep.
//	VR212-SETTLE     the sweep signs, confirms and settles into gen 1.
//	VR212-NEWKEY     a withdrawal after the settle is signed by the reshared gen-1 key.
//	VR212-BOOKS      Sigma(registry) - (Active + Fee) is unchanged and Active == User.
//	VR212-IDENT      every node holds byte-identical vault state.
//
//	VAULT_VR212_RUN=1 DEVNET_FLOOR_CONSENSUS=12 BTC_MAPPING_WASM_PATH=... \
//	  go test -v -run TestVaultVR212PreActivationReshare -timeout 150m ./tests/devnet/

// vr212KeyRow is the part of a tss_keys row this test reads, including the BRK-2
// flag TssKeyDoc does not carry.
type vr212KeyRow struct {
	Status            string `bson:"status"`
	PublicKey         string `bson:"public_key"`
	Epoch             uint64 `bson:"epoch"`
	SignatureVerified bool   `bson:"signature_verified"`
}

func vr212Key(d *Devnet, ctx context.Context, node int, keyId string) (vr212KeyRow, bool) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return vr212KeyRow{}, false
	}
	defer client.Disconnect(ctx)
	var row vr212KeyRow
	if err := client.Database(d.nodeDbName(node)).Collection("tss_keys").FindOne(ctx, bson.M{"id": keyId}).Decode(&row); err != nil {
		return vr212KeyRow{}, false
	}
	return row, true
}

// vr212Books returns Sigma(registry amounts) - (Active + Fee) and whether Active == User,
// read from node 2.
func vr212Books(d *Devnet, ctx context.Context, cid string) (gap int64, balanced bool, detail string) {
	s := vf11ReadSupply(d, ctx, 2, cid)
	st, err := getStateHex(d, ctx, 2, cid, []string{"r"})
	if !s.readable || err != nil {
		return 0, false, fmt.Sprintf("supply or registry unreadable (%v)", err)
	}
	var sum int64
	reg := st["r"]
	for off := 0; off+8 <= len(reg); off += 8 {
		var amt int64
		for _, b := range reg[off+2 : off+8] {
			amt = amt<<8 | int64(b)
		}
		sum += amt
	}
	gap = sum - (s.active + s.fee)
	return gap, s.active == s.user, fmt.Sprintf("Sigma(r)=%d Active=%d User=%d Fee=%d gap=%d", sum, s.active, s.user, s.fee, gap)
}

// vr212Settle broadcasts a signed withdrawal, settles it with confirmSpend on its change
// output and waits for the pending spend to clear.
func vr212Settle(t *testing.T, d *Devnet, ctx context.Context, cid, txid, dest, raw string) (bool, string) {
	t.Helper()
	bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, raw)
	if err != nil {
		return false, "broadcast: " + err.Error()
	}
	cs := vfRelayAndConfirmIndex(t, d, ctx, 1, cid, bcTxid, h, vfChangeVout(d, ctx, bcTxid, dest))
	gone := f21WaitSpendGone(t, d, ctx, cid, txid, 4*time.Minute)
	return gone, fmt.Sprintf("bcTxid=%s confirmSpend=%s cleared=%v", bcTxid, cs, gone)
}

func TestVaultVR212PreActivationReshare(t *testing.T) {
	if f, _ := strconv.Atoi(os.Getenv("DEVNET_FLOOR_CONSENSUS")); f < 12 {
		if testing.Short() || os.Getenv("VAULT_VR212_RUN") == "" {
			t.Skip("set VAULT_VR212_RUN=1 DEVNET_FLOOR_CONSENSUS=12")
		}
		t.Fatalf("PRECONDITION FAILED: VR2-12 is a 0.12.0 rule; DEVNET_FLOOR_CONSENSUS=%q", os.Getenv("DEVNET_FLOOR_CONSENSUS"))
	}
	if os.Getenv("VAULT_VR212_RUN") == "" {
		t.Skip("set VAULT_VR212_RUN=1")
	}
	// custodyDevnet checks the wasm, builds the slow-reshare v2 devnet, deploys and funds
	// gen-0 and waits for v2; it reads VAULT_CUSTODY_RUN, so hand it this test's switch.
	t.Setenv("VAULT_CUSTODY_RUN", "1")
	d, ctx, env, c := custodyDevnet(t, 140*time.Minute, "vr2-12: pre-activation reshare", 50_000_000, 60)
	cid := env.cid
	caller := "hive:" + d.witnessAccount(1)
	key0 := cid + "-" + btcvault.VaultKeyName(0)
	key1 := cid + "-" + btcvault.VaultKeyName(1)

	// The sweep's miner fee, before the rotation, so nothing but the gates under test
	// can refuse the first sweep.
	fundFeeReserve(t, d, ctx, cid, env.primary0, backupPubKeyG, 10_000_000)
	for end := devnetNow().Add(3 * time.Minute); devnetNow().Before(end); devnetSleep(10 * time.Second) {
		if vf11ReadSupply(d, ctx, 2, cid).fee >= 9_000_000 {
			break
		}
	}
	gap0, bal0, books0 := vr212Books(d, ctx, cid)
	t.Logf("books before the rotation: %s balanced=%v", books0, bal0)
	g0Epoch0 := uint64(0)
	if k, ok := vr212Key(d, ctx, 2, key0); ok {
		g0Epoch0 = k.Epoch
	}

	// 1. Mint gen-1 and register it. Its check-signature follows.
	primary1, ok := vfMintNextGen(t, d, ctx, cid, 1, 2, 8*time.Minute)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: gen-1 keygen never landed")
	}
	vfWaitPreparams(t, d, ctx, time.Duration(float64(12*time.Minute)*vfTimeoutScale()))
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary1, backupPubKeyG)); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: gen-1 registerPublicKey status=%s", s)
	}
	verified := false
	var tVerified time.Time
	for end := devnetNow().Add(vfTestBudget(15 * time.Minute)); devnetNow().Before(end) && !verified; {
		all := true
		for _, n := range vfAllNodes(5) {
			if k, ok := vr212Key(d, ctx, n, key1); !ok || !k.SignatureVerified {
				all = false
				break
			}
		}
		if all {
			verified, tVerified = true, devnetNow()
			break
		}
		devnetSleep(5 * time.Second)
	}
	if !verified {
		vfDumpCheckSigDiagnostics(t, d, ctx, cid)
		t.Fatalf("PRECONDITION FAILED: gen-1's check-signature never verified on every node")
	}

	// 2. Check-signed, not yet reshared: the contract refuses the activation.
	keyEpoch, keygenEpoch, found := custodyEpochs(d, ctx, 2, key1)
	if !found {
		t.Fatalf("PRECONDITION FAILED: no keygen commitment carrying gen-1's key")
	}
	if keyEpoch > keygenEpoch {
		c.rec("VR212-GATE", "activateKey refused while the check-signed key is not reshared", false,
			fmt.Sprintf("INCONCLUSIVE: gen-1 was reshared before the check (epoch %d > keygen %d)", keyEpoch, keygenEpoch))
	} else {
		simOK, simMsg := vfSimulate(d, ctx, 1, cid, caller, "activateKey", "")
		status, _ := vstatusTx(t, d, ctx, 1, cid, "activateKey", "")
		devnetSleep(10 * time.Second)
		e, kg, _ := custodyEpochs(d, ctx, 2, key1)
		still := vfVaultStatusOn(d, ctx, 2, cid, 0) == int(btcvault.VaultStatusActive) &&
			vfVaultStatusOn(d, ctx, 2, cid, 1) == int(btcvault.VaultStatusPending)
		refused := !simOK && strings.Contains(simMsg, "VR2-12") && !isOK(status) && still
		if !refused && e > kg && isOK(status) {
			c.rec("VR212-GATE", "activateKey refused while the check-signed key is not reshared", false,
				fmt.Sprintf("INCONCLUSIVE: the reshare landed between the dry run and the call (dry run ok=%v msg=%q; call=%s)", simOK, simMsg, status))
		} else {
			c.rec("VR212-GATE", "activateKey refused while the check-signed key is not reshared; gen-0 stays active, gen-1 pending", refused,
				fmt.Sprintf("dry run ok=%v msg=%q; call=%s; key epoch %d keygen %d", simOK, simMsg, status, e, kg))
		}
	}

	// 3. The old generation serves a withdrawal while gen-1 is pending.
	if vfVaultStatusOn(d, ctx, 2, cid, 1) == int(btcvault.VaultStatusPending) {
		wTxid, wDest, wRaw := rhIssueUnmap(t, d, ctx, cid, key0, 5_000_000)
		served, detail := false, "unmap not signed by gen-0"
		if wRaw != "" {
			served, detail = vr212Settle(t, d, ctx, cid, wTxid, wDest, wRaw)
		}
		pending := vfVaultStatusOn(d, ctx, 2, cid, 1) == int(btcvault.VaultStatusPending)
		c.rec("VR212-SERVE", "a withdrawal from gen-0 is signed and settled while gen-1 is pending", served && pending,
			fmt.Sprintf("%s; gen-1 still pending=%v", detail, pending))
	} else {
		c.rec("VR212-SERVE", "a withdrawal from gen-0 is signed and settled while gen-1 is pending", false,
			fmt.Sprintf("gen-1 is not pending (status %d)", vfVaultStatusOn(d, ctx, 2, cid, 1)))
	}

	// 4. The pending key's first reshare lands while it is still pending.
	reshared := false
	var tReshared time.Time
	for end := devnetNow().Add(vfTestBudget(20 * time.Minute)); devnetNow().Before(end); devnetSleep(10 * time.Second) {
		if e, kg, ok := custodyEpochs(d, ctx, 2, key1); ok && e > kg {
			reshared, tReshared = true, devnetNow()
			break
		}
	}
	pendingAtReshare := vfVaultStatusOn(d, ctx, 2, cid, 1) == int(btcvault.VaultStatusPending)
	k1, _ := vr212Key(d, ctx, 2, key1)
	reshares, _ := d.GetCommitments(ctx, 2, bson.M{"key_id": key1, "type": "reshare"})
	e1, kg1, _ := custodyEpochs(d, ctx, 2, key1)
	c.rec("VR212-RESHARE", "gen-1 is reshared while pending; public key and check-signature flag survive",
		reshared && pendingAtReshare && strings.EqualFold(k1.PublicKey, primary1) && k1.SignatureVerified,
		fmt.Sprintf("reshared=%v after %s from verification; pending=%v; key epoch %d keygen %d; reshare commitments %d; pubkey same=%v verified=%v",
			reshared, tReshared.Sub(tVerified).Round(time.Second), pendingAtReshare, e1, kg1, len(reshares),
			strings.EqualFold(k1.PublicKey, primary1), k1.SignatureVerified))
	if !reshared {
		c.summary("TestVaultVR212PreActivationReshare")
		t.FailNow()
	}

	// 5. Now the activation goes. A few tries: the calling node may not have applied the
	// reshare commitment yet.
	activated := false
	var tActivated time.Time
	activateStatus := ""
	for i := 0; i < 8 && !activated; i++ {
		if activateStatus = vstatus(t, d, ctx, 1, cid, "activateKey", ""); isOK(activateStatus) {
			activated, tActivated = true, devnetNow()
			break
		}
		devnetSleep(15 * time.Second)
	}
	c.rec("VR212-ACTIVATE", "activateKey succeeds once the pending key has been reshared", activated,
		fmt.Sprintf("status=%s after %s from the reshare", activateStatus, tActivated.Sub(tReshared).Round(time.Second)))
	if !activated {
		c.summary("TestVaultVR212PreActivationReshare")
		t.FailNow()
	}

	// 6. The FIRST migrateVault after activation builds the sweep: no reshare wait.
	g0Before := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	simOK, simMsg := vfSimulate(d, ctx, 1, cid, caller, "migrateVault", "")
	txid, sd, buildStatus := vfBuildSweep(t, d, ctx, 1, cid)
	tBuilt := devnetNow()
	c.rec("VR212-MIGRATE", "the first migrateVault right after activation builds the sweep", sd != nil && simOK,
		fmt.Sprintf("dry run ok=%v msg=%q; txid=%s status=%s; %s after activation", simOK, simMsg, txid, buildStatus, tBuilt.Sub(tActivated).Round(time.Second)))

	// 7. The sweep signs, confirms and settles.
	settled := false
	var tSettled time.Time
	if sd != nil {
		raw, signed := vfAwaitSweepSignatures(t, d, ctx, key0, sd)
		if signed {
			if bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, raw); err == nil {
				vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
				for end := devnetNow().Add(6 * time.Minute); devnetNow().Before(end) && !settled; devnetSleep(15 * time.Second) {
					settled = vfGenUtxoCountOn(d, ctx, 2, cid, 0) < g0Before && vfGenUtxoCountOn(d, ctx, 2, cid, 1) >= 1
				}
				tSettled = devnetNow()
			} else {
				t.Logf("sweep broadcast: %v", err)
			}
		}
		c.rec("VR212-SETTLE", "the sweep signs, confirms and settles into gen 1", settled,
			fmt.Sprintf("signed=%v gen-0 utxos %d -> %d, gen-1 utxos %d; user window (activate -> settle) %s",
				signed, g0Before, vfGenUtxoCountOn(d, ctx, 2, cid, 0), vfGenUtxoCountOn(d, ctx, 2, cid, 1),
				tSettled.Sub(tActivated).Round(time.Second)))
	}

	// 8. Withdrawals resume on gen 1, signed by the key that was reshared before it held funds.
	if settled {
		wTxid, wDest, wRaw := rhIssueUnmap(t, d, ctx, cid, key1, 5_000_000)
		served, detail := false, "unmap not signed by gen-1"
		if wRaw != "" {
			served, detail = vr212Settle(t, d, ctx, cid, wTxid, wDest, wRaw)
		}
		c.rec("VR212-NEWKEY", "a withdrawal after the settle is signed by gen-1 and settles", served, detail)
	}

	// 9. Books and agreement.
	gap1, bal1, books1 := vr212Books(d, ctx, cid)
	c.rec("VR212-BOOKS", "the conservation gap is unchanged across the rotation and Active == User", gap1 == gap0 && bal1,
		fmt.Sprintf("before: %s; after: %s", books0, books1))
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "VR212-IDENT")

	g0Epoch1 := uint64(0)
	if k, ok := vr212Key(d, ctx, 2, key0); ok {
		g0Epoch1 = k.Epoch
	}
	t.Logf("TIMELINE: check-sig verified -> reshared %s (hidden: gen-0 active) | reshared -> activated %s | activated -> sweep built %s | activated -> settled %s (the user window) | gen-0 key epoch %d -> %d",
		tReshared.Sub(tVerified).Round(time.Second), tActivated.Sub(tReshared).Round(time.Second),
		tBuilt.Sub(tActivated).Round(time.Second), tSettled.Sub(tActivated).Round(time.Second), g0Epoch0, g0Epoch1)
	c.summary("TestVaultVR212PreActivationReshare")
}
