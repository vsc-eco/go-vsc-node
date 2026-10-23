package tss

import (
	"strconv"
	"testing"

	"vsc-node/lib/datalayer"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/vaultrotation"

	"go.mongodb.org/mongo-driver/mongo"
)

// 0.13.0 (account-mapping C1): the EVM bridge's vault key is reshared for good
// and only the current election could sign or reshare it, so if more than a
// third of its holders left before a reshare handed it on, the key froze with
// every share still on disk. From 0.13.0 the holders named by its newest
// commitment stay readiness-eligible after leaving, as the active BTC
// generation's do (0.11.0).

const evmBridge = "vsc1evmbridgecontractxxxxxxxxxxxxxxxx"

var evmKey = evmBridge + "-primary"

// evmHolderDeps: the EVM key held by carol and dave (epoch 9); optionally the
// BTC registry of activeHoldersDeps (gen 2 active, holders bob and carol).
func evmHolderDeps(include, withBtc bool) vaultrotation.RetiringSignerDeps {
	d := vaultrotation.RetiringSignerDeps{
		ReadKey: func(string) ([]byte, bool) { return nil, false },
		GetCommitment: func(keyId string) (tss_db.TssCommitment, error) {
			if keyId == evmKey {
				return tss_db.TssCommitment{Epoch: 9, Commitment: bitsetB64(2, 3)}, nil
			}
			return tss_db.TssCommitment{}, errNoCommitment
		},
		GetElection: func(epoch uint64) *elections.ElectionResult {
			return vaElection(epoch, "alice", "bob", "carol", "dave", "erin")
		},
		EvmVaultKeyIds:         []string{evmKey},
		IncludeEvmVaultHolders: include,
	}
	if withBtc {
		btc := activeHoldersDeps(include, false)
		d.BtcContract, d.ReadKey, d.IncludeActiveHolders = btc.BtcContract, btc.ReadKey, btc.IncludeActiveHolders
		inner := btc.GetCommitment
		d.GetCommitment = func(keyId string) (tss_db.TssCommitment, error) {
			if keyId == evmKey {
				return tss_db.TssCommitment{Epoch: 9, Commitment: bitsetB64(2, 3)}, nil
			}
			return inner(keyId)
		}
	}
	return d
}

func TestComputeRetiringSignerSet_EvmVaultHoldersFrom013(t *testing.T) {
	for _, withBtc := range []bool{false, true} {
		pre := vaultrotation.ComputeRetiringSignerSet(evmHolderDeps(false, withBtc))
		if pre.HasReadiness("dave") {
			t.Fatalf("btc=%v: below 0.13.0 an EVM-key holder outside the election must not be eligible", withBtc)
		}

		set := vaultrotation.ComputeRetiringSignerSet(evmHolderDeps(true, withBtc))
		for _, a := range []string{"carol", "dave"} {
			if !set.HasReadiness(a) {
				t.Fatalf("btc=%v: EVM-key holder %s must be readiness-eligible from 0.13.0", withBtc, a)
			}
			if e, ok := set.ReadinessElection(a); !ok || e.Epoch != 9 {
				t.Fatalf("btc=%v: %s must verify against the commitment's epoch-9 election, got %v %v", withBtc, a, e.Epoch, ok)
			}
			// Readiness only: never the #11 retiring bond lock or the retiring-sign waiver.
			if set.Has(a) && !withBtc {
				t.Fatalf("%s must not join SignerElection (the #11 bond lock)", a)
			}
		}
		if set.KeyIds[evmKey] || set.ReshareSkipKeyIds[evmKey] || set.PendingKeyIds[evmKey] {
			t.Fatalf("btc=%v: the EVM key must not be a retiring, reshare-skipped or pending key: %+v", withBtc, set)
		}
		// erin holds nothing; alice holds only a retiring BTC share (fixture).
		if set.HasReadiness("erin") || (!withBtc && set.HasReadiness("alice")) {
			t.Fatalf("btc=%v: accounts holding no EVM share must not be added by the EVM rule", withBtc)
		}
		if withBtc && !set.HasReadiness("bob") {
			t.Fatal("the BTC active holders are still added next to the EVM ones")
		}
	}
	// No keygen yet: nothing added, no panic.
	d := evmHolderDeps(true, false)
	d.GetCommitment = func(string) (tss_db.TssCommitment, error) { return tss_db.TssCommitment{}, errNoCommitment }
	if s := vaultrotation.ComputeRetiringSignerSet(d); len(s.ActiveSignerElection) != 0 {
		t.Fatalf("an EVM key without a commitment added holders: %v", s.ActiveSignerElection)
	}
}

// evmOnlyConfig is a network with an EVM bridge and no BTC contract.
type evmOnlyConfig struct {
	systemconfig.SystemConfig
	op params.OracleParams
}

func (c evmOnlyConfig) OracleParams() params.OracleParams { return c.op }

type evmCommitments struct {
	tss_db.TssCommitments
	rows map[string]tss_db.TssCommitment
}

func (c evmCommitments) GetCommitmentByHeight(keyId string, _ uint64, _ ...string) (tss_db.TssCommitment, error) {
	if r, ok := c.rows[keyId]; ok {
		return r, nil
	}
	return tss_db.TssCommitment{}, mongo.ErrNoDocuments
}

// countingOutputs counts contract-output reads (the BTC registry reader reads
// the contract's last output as it is built).
type countingOutputs struct {
	contracts.ContractState
	reads int
}

func (c *countingOutputs) GetLastOutput(string, uint64) (contracts.ContractOutput, error) {
	c.reads++
	return contracts.ContractOutput{}, mongo.ErrNoDocuments
}

func evmOnlyManager(t *testing.T, version consensusversion.Version, bridges map[string]string) (*TssManager, *countingOutputs) {
	t.Helper()
	mgr := newTestTssManager(t, "observer")
	from := map[string]uint64{}
	for sym := range bridges {
		from[sym] = 0
	}
	mgr.sconf = evmOnlyConfig{SystemConfig: systemconfig.MocknetConfig(), op: params.OracleParams{EvmBridgeContracts: bridges, EvmBridgeFromHeight: from}}
	mgr.scheduler = &fakeSolvencyScheduler{minVer: version}
	mgr.tssCommitments = evmCommitments{rows: map[string]tss_db.TssCommitment{
		evmKey: {Epoch: 9, Commitment: bitsetB64(2, 3)},
	}}
	mgr.electionDb = &test_utils.MockElectionDb{Elections: map[uint64]*elections.ElectionResult{
		9: vaElection(9, "alice", "bob", "carol", "dave", "erin"),
	}}
	outputs := &countingOutputs{}
	mgr.contractState = outputs
	mgr.da = &datalayer.DataLayer{} // so a built BTC reader gets as far as the output read
	return mgr, outputs
}

func TestRetiringGenSignerSet_EvmOnlyNetwork(t *testing.T) {
	bridges := map[string]string{"ETH": evmBridge}

	pre, _ := evmOnlyManager(t, consensusversion.V0_12_0, bridges)
	if s := pre.retiringGenSignerSet(1000); s.HasReadiness("dave") {
		t.Fatal("0.12.0: the EVM holders must not be eligible")
	}

	mgr, outputs := evmOnlyManager(t, consensusversion.V0_13_0, bridges)
	set := mgr.retiringGenSignerSet(1000)
	if !set.HasReadiness("carol") || !set.HasReadiness("dave") || set.HasReadiness("alice") {
		t.Fatalf("0.13.0: want carol and dave eligible, got %v", set.ActiveSignerElection)
	}
	if outputs.reads != 0 {
		t.Fatalf("an EVM-only height read a contract output %d times (no BTC contract to read)", outputs.reads)
	}
	if !mgr.retiringGenSignerSetCached(1000).HasReadiness("dave") {
		t.Fatal("the cached accessor (gossip receive path) must see the EVM holders too")
	}

	// C-R2: below the bridge's activation height its key is not covered.
	early, _ := evmOnlyManager(t, consensusversion.V0_13_0, bridges)
	early.sconf = evmOnlyConfig{SystemConfig: systemconfig.MocknetConfig(), op: params.OracleParams{EvmBridgeContracts: bridges, EvmBridgeFromHeight: map[string]uint64{"ETH": 1001}}}
	if early.retiringGenSignerSet(1000).HasReadiness("dave") {
		t.Fatal("below the bridge's activation height: the EVM holders must not be eligible")
	}
	if !early.retiringGenSignerSet(1001).HasReadiness("dave") {
		t.Fatal("at the bridge's activation height: the EVM holders must be eligible")
	}

	// No bridge configured: inert, byte-identical to before.
	none, _ := evmOnlyManager(t, consensusversion.V0_13_0, nil)
	if keys := none.evmVaultKeysBound(1000); keys != nil {
		t.Fatalf("no EVM bridge configured: keys %v", keys)
	}
	if s := none.retiringGenSignerSet(1000); len(s.ActiveSignerElection) != 0 || s.ReadinessWidened {
		t.Fatalf("no EVM bridge and no BTC contract: want the inert set, got %+v", s)
	}
}

// The real receive path: an attestation from an EVM-key holder outside the
// current election is stored from 0.13.0 and dropped at 0.12.0.
func TestReadyGossip_EvmVaultHoldersOutsideTheElectionFrom013(t *testing.T) {
	const currentBh = uint64(1 << 30)
	keys := map[string]string{}
	type signer struct {
		account string
		seed    byte
	}
	signers := []signer{{"alice", 1}, {"carol", 3}, {"dave", 4}}
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
		mgr, _ := evmOnlyManager(t, version, map[string]string{"ETH": evmBridge})
		mgr.lastBlockHeight.Store(currentBh)
		mgr.gossipAttestations = map[string]map[string]ReadyAttestation{}
		// The current election holds only alice; carol and dave hold the EVM key
		// (epoch-9 election alice, bob, carol, dave, erin; bits 2 and 3).
		mgr.electionDb = &test_utils.MockElectionDb{
			ElectionsByHeight: map[uint64]elections.ElectionResult{currentBh: *electionOf(20, "alice")},
			Elections:         map[uint64]*elections.ElectionResult{9: electionOf(9, "alice", "bob", "carol", "dave", "erin")},
		}
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

	pre := run(t, consensusversion.V0_12_0)
	if !pre["alice"] || pre["carol"] || pre["dave"] {
		t.Fatalf("0.12.0: only the election member is stored, got %v", pre)
	}
	post := run(t, consensusversion.V0_13_0)
	if !post["alice"] || !post["carol"] || !post["dave"] {
		t.Fatalf("0.13.0: the EVM-key holders outside the election must be stored, got %v", post)
	}
}
