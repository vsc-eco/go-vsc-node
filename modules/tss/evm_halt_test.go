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

// The halt flags come from the consensus cache only (CG-1): the governance
// flag, and this bridge's own theft or custody flag as the state engine
// mirrored it. Another bridge's flag does not halt this one, and no contract
// state, datalayer or Mongo is read (the manager has none).
func TestN2_EvmKeysignHalted(t *testing.T) {
	sched := &fakeSolvencyScheduler{minVer: consensusversion.V0_13_0}
	mgr := &TssManager{scheduler: sched}
	if mgr.evmKeysignHalted("c") {
		t.Fatal("no flag set: halted")
	}
	sched.evmBridges = map[string]bool{"other": true}
	if mgr.evmKeysignHalted("c") {
		t.Fatal("another bridge's th/si halted this one")
	}
	sched.evmBridges = map[string]bool{"c": true}
	if !mgr.evmKeysignHalted("c") {
		t.Fatal("this bridge's mirrored th/si did not halt it")
	}
	sched.evmBridges = nil
	sched.evmHalted = true
	if !mgr.evmKeysignHalted("c") {
		t.Fatal("governance vsc.tss_halt evm did not halt")
	}
}

// Through evmSignRefused on a 0.13.0 bridge whose one (pre-rotation) key the
// scope allows: the bridge's mirrored theft/custody flag and the governance
// halt each refuse it; a 0.12.0 network never consults either.
func TestN2_EvmSignRefusedHonoursTheHalt(t *testing.T) {
	da := startTestDatalayer(t)
	clean := committedState(t, da, map[string]string{"chainid": "1"})
	bridges := map[string]string{"ETH": evmBridge}
	primary := evmBridge + "-primary"

	mgr, _ := evmOnlyManager(t, consensusversion.V0_13_0, bridges)
	mgr.tssKeys = stubTssKeys{}
	mgr.da = da
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: clean}}
	if mgr.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("no halt: the bridge's key was refused")
	}
	sched := mgr.scheduler.(*fakeSolvencyScheduler)
	sched.evmBridges = map[string]bool{evmBridge: true}
	if !mgr.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("th/si set: a withdrawal signature was issued")
	}
	sched.evmBridges = nil
	sched.evmHalted = true
	if !mgr.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("governance EVM halt: a withdrawal signature was issued")
	}

	pre, _ := evmOnlyManager(t, consensusversion.V0_12_0, bridges)
	pre.da = da
	pre.contractState = &heightOutputs{byHeight: map[uint64]string{10: clean}}
	pre.scheduler.(*fakeSolvencyScheduler).evmHalted = true
	pre.scheduler.(*fakeSolvencyScheduler).evmBridges = map[string]bool{evmBridge: true}
	if pre.evmSignRefused(primary, make([]byte, 32), 1000) {
		t.Fatal("0.12.0: the EVM gate, halt included, must be inert")
	}
}

// CG-1: a read this node cannot make is not a refusal, but it does not lift a
// halt either: the flags come from the consensus cache, not from that read.
func TestCG1_HaltHoldsWhenThisNodeCannotRead(t *testing.T) {
	mgr, _, _ := cg1Bridge(t)
	mgr.contractState = &heightOutputs{err: errors.New("connection reset by peer")}
	sched := mgr.scheduler.(*fakeSolvencyScheduler)
	if mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Fatal("read fault, no halt: the active generation was refused")
	}
	sched.evmBridges = map[string]bool{evmBridge: true}
	if !mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Fatal("read fault, bridge th/si set: a withdrawal signature was issued")
	}
	sched.evmBridges = nil
	sched.evmHalted = true
	if !mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Fatal("read fault, governance EVM halt: a withdrawal signature was issued")
	}
}
