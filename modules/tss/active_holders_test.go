package tss

import (
	"strconv"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"
)

// 0.11.0 (THORChain parity: members of a vault that holds funds keep signing it). Today only the current election can sign or
// reshare the ACTIVE key, so if more than a third of its holders leave before a
// reshare hands it on, the key freezes with every share still on disk. From
// 0.11.0 the holders named by the active generation's newest commitment stay
// readiness-eligible after leaving. Their bond is POA-1's (0.9.0): no unbond
// while any generation is funded.

// activeHoldersDeps: gen 1 RETIRING (committee alice, bob at epoch 5), gen 2
// ACTIVE (holders bob, carol at epoch 9), optionally gen 3 PENDING (keygen
// committee erin at epoch 10).
func activeHoldersDeps(include, withPending bool) vaultrotation.RetiringSignerDeps {
	vaults := []btcvault.Vault{
		{Generation: 1, Primary: scopePub(1), Backup: scopePub(9), Status: btcvault.VaultStatusRetiring},
		{Generation: 2, Primary: scopePub(2), Backup: scopePub(9), Status: btcvault.VaultStatusActive},
	}
	if withPending {
		vaults = append(vaults, btcvault.Vault{Generation: 3, Primary: scopePub(3), Backup: scopePub(9), Status: btcvault.VaultStatusPending})
	}
	reg := marshalRegistry(vaults...)
	members := []string{"alice", "bob", "carol", "dave", "erin"}
	return vaultrotation.RetiringSignerDeps{
		BtcContract: scopeContract,
		ReadKey: func(k string) ([]byte, bool) {
			if k == "v" {
				return reg, true
			}
			return nil, false
		},
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			switch keyId {
			case scopeContract + "-" + btcvault.VaultKeyName(1):
				return tss_db.TssCommitment{Epoch: 5, Commitment: bitsetB64(0, 1)}, nil
			case scopeContract + "-" + btcvault.VaultKeyName(2):
				return tss_db.TssCommitment{Epoch: 9, Commitment: bitsetB64(1, 2)}, nil
			case scopeContract + "-" + btcvault.VaultKeyName(3):
				return tss_db.TssCommitment{Epoch: 10, Commitment: bitsetB64(4)}, nil
			}
			return tss_db.TssCommitment{}, errNoCommitment
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return vaElection(epoch, members...)
		},
		IncludeActiveHolders: include,
	}
}

func TestComputeRetiringSignerSet_ActiveHoldersFrom011(t *testing.T) {
	// Below 0.11.0: byte-identical to before. carol (active only) is not
	// readiness-eligible outside the election; the pending committee is still
	// emitted (HasReadiness) but not accepted on receipt.
	pre := vaultrotation.ComputeRetiringSignerSet(activeHoldersDeps(false, true))
	if len(pre.ActiveSignerElection) != 0 || pre.ReadinessWidened {
		t.Fatalf("below 0.11.0 the active holders must not be added: %v", pre.ActiveSignerElection)
	}
	if pre.HasReadiness("carol") {
		t.Fatal("below 0.11.0 carol (active-key holder only) must not be eligible")
	}
	if _, ok := pre.ReadinessElection("erin"); ok {
		t.Fatal("below 0.11.0 the pending keygen committee is not accepted on receipt (unchanged)")
	}
	if _, ok := pre.ReadinessElection("alice"); !ok {
		t.Fatal("the retiring committee is accepted as before")
	}

	set := vaultrotation.ComputeRetiringSignerSet(activeHoldersDeps(true, true))
	for _, a := range []string{"bob", "carol"} {
		if _, ok := set.ActiveSignerElection[a]; !ok || !set.HasReadiness(a) {
			t.Fatalf("%s holds the active key: must be readiness-eligible outside the election", a)
		}
		e, ok := set.ReadinessElection(a)
		if !ok || (a == "carol" && e.Epoch != 9) {
			t.Fatalf("%s must verify against its commitment epoch election, got %v %v", a, e.Epoch, ok)
		}
	}
	if set.Has("carol") {
		t.Fatal("an active-key holder must not enter the #11 retiring-member set (its bond is POA-1's)")
	}
	if _, ok := set.ActiveSignerElection["alice"]; ok {
		t.Fatal("only the active generation's newest commitment counts")
	}
	if _, ok := set.ActiveSignerElection["dave"]; ok {
		t.Fatal("only the active generation's newest commitment counts")
	}
	if e, ok := set.ReadinessElection("erin"); !ok || e.Epoch != 10 {
		t.Fatalf("from 0.11.0 a pending keygen member outside the election is accepted on receipt, got %v %v", e.Epoch, ok)
	}
	if !set.Has("alice") || !set.Has("bob") {
		t.Fatal("the retiring committee keeps its #11 lock")
	}
}

func TestActiveKeyHoldersBound_FollowsTheChainVersion(t *testing.T) {
	for _, tc := range []struct {
		v    consensusversion.Version
		want bool
	}{{consensusversion.V0_10_0, false}, {consensusversion.V0_11_0, true}} {
		mgr := &TssManager{scheduler: &fakeSolvencyScheduler{minVer: tc.v}}
		if got := mgr.activeKeyHoldersBound(1000); got != tc.want {
			t.Errorf("activeKeyHoldersBound at %s = %v, want %v", tc.v.Format(), got, tc.want)
		}
	}
	if (&TssManager{}).activeKeyHoldersBound(1000) {
		t.Error("no scheduler: inert")
	}
}

// The real receive path: an attestation from an active-key holder (and from a
// pending keygen member) who is outside the current election is stored from
// 0.11.0 and dropped below it; a retiring holder's is stored either way.
func TestReadyGossip_ActiveHoldersOutsideTheElectionFrom011(t *testing.T) {
	const currentBh = uint64(1 << 30)
	keys := map[string]string{}
	members := []string{"alice", "bob", "carol", "dave", "erin"}
	type signer struct {
		account string
		seed    byte
	}
	signers := []signer{{"alice", 1}, {"bob", 2}, {"carol", 3}, {"dave", 4}, {"erin", 5}}
	for _, s := range signers {
		_, pk := generateTestBLSKey(s.seed)
		keys[s.account] = testBlsDIDFromPubKey(pk)
	}
	electionOf := func(epoch uint64, accounts ...string) *elections.ElectionResult {
		ms := make([]elections.ElectionMember, len(accounts))
		for i, a := range accounts {
			ms[i] = elections.ElectionMember{Account: a, Key: keys[a]}
		}
		return &elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: ms},
		}
	}

	run := func(t *testing.T, version consensusversion.Version) map[string]bool {
		t.Helper()
		mgr := newTestTssManager(t, "observer")
		mgr.sconf = systemconfig.MainnetConfig()
		mgr.scheduler = &fakeSolvencyScheduler{minVer: version}
		mgr.lastBlockHeight.Store(currentBh)
		mgr.gossipAttestations = map[string]map[string]ReadyAttestation{}
		// The current election holds only dave: alice/bob are the retiring
		// committee, bob/carol the active key's holders, erin the pending keygen.
		current := electionOf(20, "dave")
		mgr.electionDb = &test_utils.MockElectionDb{ElectionsByHeight: map[uint64]elections.ElectionResult{currentBh: *current}}

		deps := activeHoldersDeps(mgr.activeKeyHoldersBound(currentBh), true)
		deps.GetElection = func(epoch uint64) *elections.ElectionResult { return electionOf(epoch, members...) }
		mgr.retiringSetCache = map[string]vaultrotation.RetiringSignerSet{
			strconv.FormatUint(currentBh, 10): vaultrotation.ComputeRetiringSignerSet(deps),
		}

		// One attestation per bundle, so only eligibility (not the bundle-size
		// cap) decides what is stored.
		target := currentBh + 50
		for _, s := range signers {
			sk, _ := generateTestBLSKey(s.seed)
			att, err := signAttestationDirect(&sk, s.account, target)
			if err != nil {
				t.Fatal(err)
			}
			p2pSpec{tssMgr: mgr}.handleReadyGossip(p2pMessage{Type: "ready_gossip", Data: map[string]interface{}{
				"target_block": float64(target),
				"attestations": []interface{}{map[string]interface{}{"account": att.Account, "sig": att.Sig}},
			}})
		}
		got := map[string]bool{}
		for a := range mgr.gossipAttestations[strconv.FormatUint(target, 10)] {
			got[a] = true
		}
		return got
	}

	pre := run(t, consensusversion.V0_10_0)
	if !pre["dave"] || !pre["alice"] || !pre["bob"] {
		t.Fatalf("0.10.0: election member and retiring committee must be stored, got %v", pre)
	}
	if pre["carol"] || pre["erin"] {
		t.Fatalf("0.10.0: active-only holder and pending member outside the election are dropped (unchanged), got %v", pre)
	}

	post := run(t, consensusversion.V0_11_0)
	for _, a := range members {
		if !post[a] {
			t.Fatalf("0.11.0: %s must be stored (election, retiring, active holder or pending keygen), got %v", a, post)
		}
	}
}
