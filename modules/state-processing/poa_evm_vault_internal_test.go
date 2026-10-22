package state_engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	vdb "vsc-node/modules/db"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"

	"go.mongodb.org/mongo-driver/mongo"
)

// C2 (0.12.0): POA-1 read only the BTC vault registry, so the bonds behind the
// EVM bridge's vault key were never held: a holder could unstake and keep a
// share that signs for the vault. From 0.12.0, while the EVM key exists, its
// newest holders, the current committee and every electable or recently active
// witness are held, as for a funded BTC generation.

const evmPoaBridge = "vsc1evmpoabridge"

type evmPoaConfig struct {
	systemconfig.SystemConfig
	op params.OracleParams
}

func (c evmPoaConfig) OracleParams() params.OracleParams { return c.op }

type evmPoaCommitments struct {
	tss_db.TssCommitments
	row    *tss_db.TssCommitment
	err    error
	heldAt uint64
}

func (c *evmPoaCommitments) GetCommitmentByHeight(keyId string, height uint64, _ ...string) (tss_db.TssCommitment, error) {
	c.heldAt = height
	if c.err != nil {
		return tss_db.TssCommitment{}, c.err
	}
	if c.row == nil || keyId != evmPoaBridge+"-primary" {
		return tss_db.TssCommitment{}, mongo.ErrNoDocuments
	}
	return *c.row, nil
}

// evmPoaElections: the chain runs `version`; the current election holds
// `current`; epoch 9 (the EVM key's newest commitment) has five members.
type evmPoaElections struct {
	fakeElections
	current []string
}

func (f *evmPoaElections) GetElectionByHeight(uint64) (elections.ElectionResult, error) {
	e := ratifiedAtVersion(f.version, 20, f.current...)
	return e, nil
}

func (f *evmPoaElections) GetElectionStrict(epoch uint64) (*elections.ElectionResult, error) {
	if epoch != 9 {
		return nil, mongo.ErrNoDocuments
	}
	e := ratified(9, "hive:alice", "hive:bob", "hive:carol", "hive:dave", "hive:erin")
	return &e, nil
}

func evmPoaEnv(t *testing.T, version uint64, row *tss_db.TssCommitment, current ...string) (*StateEngine, *evmPoaCommitments) {
	t.Helper()
	se, _, _ := poaEnv(t, version)
	se.sconf = evmPoaConfig{SystemConfig: systemconfig.MocknetConfig(),
		op: params.OracleParams{EvmBridgeContracts: map[string]string{"ETH": evmPoaBridge}, EvmBridgeFromHeight: map[string]uint64{"ETH": 0}}}
	commits := &evmPoaCommitments{row: row}
	se.tssCommitments = commits
	se.electionDb = &evmPoaElections{fakeElections: fakeElections{version: version}, current: current}
	return se, commits
}

func TestC2_EvmVaultHoldersBondsAreHeldFrom012(t *testing.T) {
	const h = uint64(1_000_000)
	// The EVM key's newest commitment (epoch 9) names carol and dave (bits 2, 3).
	newest := &tss_db.TssCommitment{Epoch: 9, Commitment: bits(2, 3)}

	pre, _ := evmPoaEnv(t, 11, newest)
	if held, err := pre.heldShareOfFundedVault("hive:carol", h); err != nil || held {
		t.Fatalf("0.11.0: an EVM-key holder is not held (BTC-only rule): held=%v err=%v", held, err)
	}

	se, commits := evmPoaEnv(t, 12, newest, "hive:bob")
	for _, tc := range []struct {
		account string
		want    bool
	}{
		{"hive:carol", true}, // newest EVM holder
		{"dave", true},       // same, unprefixed
		{"hive:bob", true},   // current committee (about to hold a share)
		{"hive:alice", false},
		{"hive:erin", false},
	} {
		held, err := se.heldShareOfFundedVault(tc.account, h)
		if err != nil || held != tc.want {
			t.Errorf("0.12.0: %s held=%v err=%v, want %v", tc.account, held, err, tc.want)
		}
	}
	if commits.heldAt != h {
		t.Errorf("the EVM commitment was read below %d, want strictly below the unstake height %d (POA-1's rule)", commits.heldAt, h)
	}

	// C-R2: before the bridge's activation height its key is not a
	// consensus input, so nothing is held for it.
	early, _ := evmPoaEnv(t, 12, newest)
	early.sconf = evmPoaConfig{SystemConfig: systemconfig.MocknetConfig(),
		op: params.OracleParams{EvmBridgeContracts: map[string]string{"ETH": evmPoaBridge}, EvmBridgeFromHeight: map[string]uint64{"ETH": h + 1}}}
	if held, err := early.heldShareOfFundedVault("hive:carol", h); err != nil || held {
		t.Fatalf("below the bridge's activation height: carol held=%v err=%v, want not held", held, err)
	}
	if held, err := early.heldShareOfFundedVault("hive:carol", h+1); err != nil || !held {
		t.Fatalf("at the bridge's activation height: carol held=%v err=%v, want held", held, err)
	}

	// No keygen yet: the key holds nothing, nothing is held for it.
	nokey, _ := evmPoaEnv(t, 12, nil, "hive:bob")
	if held, err := nokey.heldShareOfFundedVault("hive:bob", h); err != nil || held {
		t.Fatalf("no EVM keygen and no BTC registry: nothing is funded, held=%v err=%v", held, err)
	}

	// A transient read is returned (the caller blocks and retries), never decided.
	flaky, fc := evmPoaEnv(t, 12, newest)
	fc.err = errors.New("mongo blip")
	if held, err := flaky.heldShareOfFundedVault("hive:alice", h); err == nil || !held {
		t.Fatalf("a commitment read error must hold and be returned: held=%v err=%v", held, err)
	}

	// A stored row that does not decode is the same on every node: the unstake
	// decides (held) instead of retrying forever (POA-1's ErrDecode rule).
	corrupt, cc := evmPoaEnv(t, 12, newest)
	cc.err = fmt.Errorf("failed to decode commitment: %w: bad epoch", vdb.ErrDecode)
	decided := make(chan bool, 1)
	go func() { decided <- corrupt.HeldShareOfFundedVaultOrBlock("hive:alice", h) }()
	select {
	case held := <-decided:
		if !held {
			t.Fatal("an undecodable EVM key commitment must hold the bond")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an undecodable EVM key commitment is retried instead of decided")
	}

	// No bridge configured: no EVM read at all.
	plain, pc := evmPoaEnv(t, 12, newest)
	plain.sconf = evmPoaConfig{SystemConfig: systemconfig.MocknetConfig()}
	pc.heldAt = 0
	if held, _ := plain.heldShareOfFundedVault("hive:carol", h); held || pc.heldAt != 0 {
		t.Fatalf("no EVM bridge configured: held=%v, commitment read at %d", held, pc.heldAt)
	}
}
