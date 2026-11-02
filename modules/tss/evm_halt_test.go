package tss

import (
	"context"
	"errors"
	"testing"

	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"go.mongodb.org/mongo-driver/mongo"
)

// N2: the halt freezes everything but what evacuates or protects the funds,
// as BTC's V-8 / BRK-2 exemptions do, plus a superseded generation's own
// in-flight withdrawals (their nonces come before its sweep's).
func TestN2_EvmSignGateDecision(t *testing.T) {
	cases := []struct {
		name    string
		verdict scopeVerdict
		halted  bool
		refuse  bool
	}{
		{"refuse, no halt", scopeRefuse, false, true},
		{"refuse, halt", scopeRefuse, true, true},
		{"allow, no halt", scopeAllow, false, false},
		{"allow (active-generation withdrawal), halt -> frozen", scopeAllow, true, true},
		{"sweep, no halt", scopeSuccessorSweep, false, false},
		{"sweep, halt -> exempt (evacuation)", scopeSuccessorSweep, true, false},
		{"checksig, no halt", scopeCheckSig, false, false},
		{"checksig, halt -> exempt (moves no funds)", scopeCheckSig, true, false},
		{"own in-flight withdrawal, no halt", scopeOwnWithdrawal, false, false},
		{"own in-flight withdrawal, halt -> exempt (its nonces precede the sweep)", scopeOwnWithdrawal, true, false},
	}
	for _, tc := range cases {
		if got := evmSignGateDecision(tc.verdict, tc.halted); got != tc.refuse {
			t.Fatalf("%s: evmSignGateDecision(%v,%v)=%v want %v", tc.name, tc.verdict, tc.halted, got, tc.refuse)
		}
	}
}

// heightOutputs serves a contract's outputs by block height, as the contracts
// collection does ({block_height <= h}, newest first).
type heightOutputs struct {
	contracts.ContractState
	byHeight map[uint64]string // block height -> state merkle
	err      error
}

func (h *heightOutputs) GetLastOutputStrict(_ string, at uint64) (contracts.ContractOutput, error) {
	if h.err != nil {
		return contracts.ContractOutput{}, h.err
	}
	best, found := uint64(0), false
	for height := range h.byHeight {
		if height <= at && (!found || height > best) {
			best, found = height, true
		}
	}
	if !found {
		return contracts.ContractOutput{}, mongo.ErrNoDocuments
	}
	return contracts.ContractOutput{StateMerkle: h.byHeight[best], BlockHeight: int64(best)}, nil
}

func (h *heightOutputs) GetLastOutput(c string, at uint64) (contracts.ContractOutput, error) {
	o, err := h.GetLastOutputStrict(c, at)
	if err != nil {
		return contracts.ContractOutput{}, nil
	}
	return o, nil
}

func startTestDatalayer(t *testing.T) *datalayer.DataLayer {
	t.Helper()
	identityConfig, p2pConfig := common.NewIdentityConfig(), p2pInterface.NewConfig()
	p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, systemconfig.MocknetConfig(), nil)
	da := datalayer.New(p2p)
	agg := aggregate.New([]aggregate.Plugin{identityConfig, p2pConfig, p2p, da})
	if err := agg.Init(); err != nil {
		t.Fatalf("init datalayer: %v", err)
	}
	if _, err := agg.Start().Await(context.Background()); err != nil {
		t.Fatalf("start datalayer: %v", err)
	}
	t.Cleanup(func() { _ = agg.Stop() })
	return da
}

// committedState stores a contract state directory holding keys, each value a
// DagCbor object as the state engine stores contract values, and returns its CID.
func committedState(t *testing.T, da *datalayer.DataLayer, keys map[string]string) string {
	t.Helper()
	ctx := context.Background()
	dir := uio.NewDirectory(da.DagServ)
	for k, v := range keys {
		c, err := da.PutObject(map[string]interface{}{"v": v})
		if err != nil {
			t.Fatal(err)
		}
		n, err := da.DagServ.Get(ctx, *c)
		if err != nil {
			t.Fatal(err)
		}
		if err := dir.AddChild(ctx, k, n); err != nil {
			t.Fatal(err)
		}
	}
	root, err := dir.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := da.DagServ.Add(ctx, root); err != nil {
		t.Fatal(err)
	}
	return root.Cid().String()
}

// The halt flags, read from a real datalayer: the governance flag, the
// bridge's "th" from the output committed before bh (an output written in bh
// itself counts from bh+1 on every node), no output yet, and a read failure
// (refuses).
func TestN2_EvmKeysignHalted(t *testing.T) {
	da := startTestDatalayer(t)
	clean := committedState(t, da, map[string]string{"chainid": "1"})
	theft := committedState(t, da, map[string]string{"chainid": "1", "th": "1"})
	sched := &fakeSolvencyScheduler{minVer: consensusversion.V0_13_0}
	mgr := &TssManager{da: da, scheduler: sched}

	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{}}
	if h, err := mgr.evmKeysignHalted("c", 100); h || err != nil {
		t.Fatalf("no output yet: halted=%v err=%v, want false, nil", h, err)
	}

	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{90: clean, 100: theft}}
	if h, err := mgr.evmKeysignHalted("c", 100); h || err != nil {
		t.Fatalf("th written in block 100, read at tick 100: halted=%v err=%v; must wait for tick 101 (every node has processed 100 then)", h, err)
	}
	if h, err := mgr.evmKeysignHalted("c", 101); !h || err != nil {
		t.Fatalf("tick 101 after th in block 100: halted=%v err=%v, want true, nil", h, err)
	}
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{90: clean, 100: theft, 110: clean}}
	if h, err := mgr.evmKeysignHalted("c", 111); h || err != nil {
		t.Fatalf("after clearTheftHalt (th deleted in 110): halted=%v err=%v", h, err)
	}

	sched.evmHalted = true
	if h, err := mgr.evmKeysignHalted("c", 111); !h || err != nil {
		t.Fatalf("governance vsc.tss_halt evm: halted=%v err=%v, want true", h, err)
	}
	sched.evmHalted = false

	mgr.contractState = &heightOutputs{err: errors.New("mongo: connection reset")}
	if h, err := mgr.evmKeysignHalted("c", 111); !h || err == nil {
		t.Fatalf("unreadable output: halted=%v err=%v, want true and the error (fail closed)", h, err)
	}
}

// Through evmSignRefused on a 0.13.0 bridge whose one (pre-rotation) key the
// scope allows: the theft flag and the governance halt each refuse it; a
// 0.12.0 network never consults either.
func TestN2_EvmSignRefusedHonoursTheHalt(t *testing.T) {
	da := startTestDatalayer(t)
	clean := committedState(t, da, map[string]string{"chainid": "1"})
	theft := committedState(t, da, map[string]string{"chainid": "1", "th": "1"})
	bridges := map[string]string{"ETH": evmBridge}
	primary := evmBridge + "-primary"

	mgr, _ := evmOnlyManager(t, consensusversion.V0_13_0, bridges)
	mgr.tssKeys = stubTssKeys{}
	mgr.da = da
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: clean}}
	if mgr.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("no halt: the bridge's key was refused")
	}
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: theft}}
	if !mgr.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("th set: a withdrawal signature was issued")
	}
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: clean}}
	mgr.scheduler.(*fakeSolvencyScheduler).evmHalted = true
	if !mgr.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("governance EVM halt: a withdrawal signature was issued")
	}

	pre, _ := evmOnlyManager(t, consensusversion.V0_12_0, bridges)
	pre.da = da
	pre.contractState = &heightOutputs{byHeight: map[uint64]string{10: theft}}
	pre.scheduler.(*fakeSolvencyScheduler).evmHalted = true
	if pre.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("0.12.0: the EVM gate, halt included, must be inert")
	}
}
