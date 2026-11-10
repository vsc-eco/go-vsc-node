package state_engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/multiformats/go-multicodec"
	"go.mongodb.org/mongo-driver/mongo"
)

// CG-1: each rotating EVM bridge's theft ("th") and custody ("si") flags are
// mirrored into the consensus cache at the end of every block, so the EVM sign
// gate reads them with no I/O. A read error keeps the bridge's last value.

type evmBridgeConfig struct {
	systemconfig.SystemConfig
	bridges map[string]string
}

func (c evmBridgeConfig) OracleParams() params.OracleParams {
	op := c.SystemConfig.OracleParams()
	op.EvmBridgeContracts = c.bridges
	op.EvmBridgeFromHeight = map[string]uint64{}
	for sym := range c.bridges {
		op.EvmBridgeFromHeight[sym] = 0
	}
	return op
}

// evmHaltState holds one ChainConsensusState and counts EVM bridge halt writes.
type evmHaltState struct {
	consensus_state.ConsensusState
	s         consensus_state.ChainConsensusState
	writes    int
	failWrite error
}

func (m *evmHaltState) Get(context.Context) (consensus_state.ChainConsensusState, error) {
	return m.s, nil
}

func (m *evmHaltState) SetEvmBridgeHalted(_ context.Context, halted []string) error {
	if m.failWrite != nil {
		return m.failWrite
	}
	m.s.EvmBridgeHalted = slices.Clone(halted)
	m.writes++
	return nil
}

// bridgeOutputs serves each contract's outputs by height ({block_height <= h},
// newest first); err[c] is this node's query for c failing.
type bridgeOutputs struct {
	contracts.ContractState
	byContract map[string]map[uint64]string
	err        map[string]error
}

func (b *bridgeOutputs) GetLastOutputStrict(c string, at uint64) (contracts.ContractOutput, error) {
	if err := b.err[c]; err != nil {
		return contracts.ContractOutput{}, err
	}
	best, found := uint64(0), false
	for h := range b.byContract[c] {
		if h <= at && (!found || h > best) {
			best, found = h, true
		}
	}
	if !found {
		return contracts.ContractOutput{}, mongo.ErrNoDocuments
	}
	return contracts.ContractOutput{ContractId: c, StateMerkle: b.byContract[c][best], BlockHeight: int64(best)}, nil
}

func startStateTestDatalayer(t *testing.T) *datalayer.DataLayer {
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

// rawState stores a contract state directory of raw values, as the block
// producer does, and returns its root CID.
func rawState(t *testing.T, da *datalayer.DataLayer, values map[string]string) string {
	t.Helper()
	ctx := context.Background()
	dir := uio.NewDirectory(da.DagServ)
	for k, v := range values {
		c, err := da.PutRaw([]byte(v), common_types.PutRawOptions{Codec: multicodec.Raw})
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

func evmMirrorEngine(t *testing.T, protocolVersion uint64) (*StateEngine, *evmHaltState, *bridgeOutputs, *datalayer.DataLayer) {
	t.Helper()
	da := startStateTestDatalayer(t)
	cs := &evmHaltState{}
	outs := &bridgeOutputs{byContract: map[string]map[uint64]string{"vsc1evmA": {}, "vsc1evmB": {}}, err: map[string]error{}}
	se := &StateEngine{
		sconf:          evmBridgeConfig{systemconfig.MocknetConfig(), map[string]string{"ETH": "vsc1evmA", "BASE": "vsc1evmB"}},
		consensusState: cs,
		contractState:  outs,
		da:             da,
		electionDb: oneElection{e: elections.ElectionResult{
			ElectionDataInfo: elections.ElectionDataInfo{ProtocolVersion: protocolVersion},
		}},
	}
	se.refreshChainConsensusCache()
	return se, cs, outs, da
}

func TestCG1_EvmBridgeHaltMirror(t *testing.T) {
	se, cs, outs, da := evmMirrorEngine(t, 13)
	clean := rawState(t, da, map[string]string{"chainid": "1"})
	theft := rawState(t, da, map[string]string{"chainid": "1", "th": "1"})
	short := rawState(t, da, map[string]string{"chainid": "1", "si": "100"})
	A, B := "vsc1evmA", "vsc1evmB"
	check := func(step string, wantA, wantB bool, wantWrites int) {
		t.Helper()
		if se.EvmBridgeHalted(A) != wantA || se.EvmBridgeHalted(B) != wantB {
			t.Fatalf("%s: halted A=%v B=%v, want A=%v B=%v (cache %v)", step, se.EvmBridgeHalted(A), se.EvmBridgeHalted(B), wantA, wantB, cs.s.EvmBridgeHalted)
		}
		if cs.writes != wantWrites {
			t.Fatalf("%s: %d consensus_state writes, want %d", step, cs.writes, wantWrites)
		}
	}

	outs.byContract[A][90], outs.byContract[B][90] = clean, clean
	outs.byContract[A][100] = theft
	se.refreshEvmBridgeHalts(99)
	check("block 99, th not yet written", false, false, 0)
	se.refreshEvmBridgeHalts(100)
	check("end of block 100, th written in 100", true, false, 1)
	se.refreshEvmBridgeHalts(101)
	check("block 101, unchanged: no write", true, false, 1)

	outs.err[A] = errors.New("connection reset by peer")
	outs.byContract[A][105] = clean // cleared, but this node cannot read it
	se.refreshEvmBridgeHalts(105)
	check("A unreadable: keeps halted", true, false, 1)
	delete(outs.err, A)
	se.refreshEvmBridgeHalts(106)
	check("A readable again: cleared", false, false, 2)

	outs.err[A] = errors.New("server selection timeout")
	outs.byContract[A][107] = theft // set, but this node cannot read it
	se.refreshEvmBridgeHalts(107)
	check("A unreadable while not halted: stays not halted", false, false, 2)
	delete(outs.err, A)

	outs.byContract[B][120] = short
	se.refreshEvmBridgeHalts(120)
	check("si on B (and th on A from 107)", true, true, 3)

	cs.failWrite = errors.New("write failed")
	outs.byContract[A][130], outs.byContract[B][130] = clean, clean
	se.refreshEvmBridgeHalts(130)
	check("write fails: cache keeps the last value", true, true, 3)
	cs.failWrite = nil
	se.refreshEvmBridgeHalts(131)
	check("next block: written", false, false, 4)
}

// Below 0.13.0 the EVM gate is inert and the mirror reads nothing.
func TestCG1_EvmBridgeHaltMirrorBelow013(t *testing.T) {
	se, cs, outs, da := evmMirrorEngine(t, 12)
	outs.byContract["vsc1evmA"][10] = rawState(t, da, map[string]string{"th": "1"})
	se.refreshEvmBridgeHalts(10)
	if se.EvmBridgeHalted("vsc1evmA") || cs.writes != 0 {
		t.Fatalf("0.12: halted=%v writes=%d, want false and 0", se.EvmBridgeHalted("vsc1evmA"), cs.writes)
	}
}

// The mirror must run after the block's transactions (where outputs are
// ingested) and its slot-boundary batch: the consumer starts the next block's
// TSS tick only after ProcessBlock returns, so that tick then reads the flags
// as of this block on every node. At the start of ProcessBlock it would race
// this block's tick and miss a flag set in the block before.
func TestCG1_EvmBridgeHaltMirrorRunsAtTheEndOfProcessBlock(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "state_engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "ProcessBlock" && fn.Recv != nil {
			body = fn.Body.List
		}
	}
	if body == nil {
		t.Fatal("ProcessBlock not found")
	}
	src := func(s ast.Stmt) string {
		var b strings.Builder
		ast.Inspect(s, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				b.WriteString(id.Name + " ")
			}
			return true
		})
		return b.String()
	}
	mirror, lastWork := -1, -1
	for i, s := range body {
		text := src(s)
		if strings.Contains(text, "refreshEvmBridgeHalts") {
			if mirror != -1 {
				t.Fatal("refreshEvmBridgeHalts is called twice")
			}
			mirror = i
		}
		if strings.Contains(text, "ExecuteBatch") || strings.Contains(text, "Transactions") {
			lastWork = i
		}
	}
	if mirror == -1 || lastWork == -1 {
		t.Fatalf("mirror at %d, last transaction/batch statement at %d", mirror, lastWork)
	}
	if mirror < lastWork {
		t.Fatalf("refreshEvmBridgeHalts (statement %d) runs before the block's transactions/batch (statement %d)", mirror, lastWork)
	}
}
