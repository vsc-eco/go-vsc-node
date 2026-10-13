package tss

import (
	"context"
	"errors"
	"testing"
	"time"

	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/ipfs/boxo/ipld/merkledag"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multicodec"
)

type fixedOutputState struct {
	contracts.ContractState
	stateMerkle string
}

func (f *fixedOutputState) GetLastOutput(string, uint64) (contracts.ContractOutput, error) {
	return contracts.ContractOutput{StateMerkle: f.stateMerkle}, nil
}

// failingDagServ fails every read of one CID, the way a corrupt or unreadable
// block does on the full data layer.
type failingDagServ struct {
	format.DAGService
	bad cid.Cid
}

func (f *failingDagServ) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	if c.Equals(f.bad) {
		return nil, errors.New("injected unreadable block")
	}
	return f.DAGService.Get(ctx, c)
}

// The BTC sign gates read contract state under the global TSS lock. A state
// state read that fails must come back as absent, not block (the full data
// layer retries a failed read forever): a blocked read holds the lock and stops every sign,
// reshare and keygen on the node.
func TestContractStateReaderDoesNotBlockOnMissingBlock(t *testing.T) {
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
	ctx := context.Background()

	missingSub := uio.NewDirectory(da.DagServ)
	if err := missingSub.AddChild(ctx, "x", merkledag.NodeWithData([]byte("only in the sub-directory"))); err != nil {
		t.Fatal(err)
	}
	subNode, err := missingSub.GetNode() // never stored
	if err != nil {
		t.Fatal(err)
	}
	present := merkledag.NodeWithData([]byte("v"))
	if err := da.DagServ.Add(ctx, present); err != nil {
		t.Fatal(err)
	}
	root := uio.NewDirectory(da.DagServ)
	if err := root.AddChild(ctx, "sub", subNode); err != nil {
		t.Fatal(err)
	}
	if err := root.AddChild(ctx, "v", present); err != nil {
		t.Fatal(err)
	}
	rootNode, err := root.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := da.DagServ.Add(ctx, rootNode); err != nil {
		t.Fatal(err)
	}

	// Only the full data layer sees the fault; the LocalOnly view reads the same
	// blockstore through its own service.
	da.DagServ = &failingDagServ{DAGService: da.DagServ, bad: rootNode.Cid()}
	tssMgr := &TssManager{contractState: &fixedOutputState{stateMerkle: rootNode.Cid().String()}, da: da}
	done := make(chan struct{})
	go func() {
		read := tssMgr.contractStateReaderAt("vsc1contract", 100)
		read("v")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("contract state read blocked on a failing state read")
	}
}

// NT-6: contractStateReaderAtStrict must distinguish a genuinely ABSENT contract
// key from a READ ERROR. DataBin.Get returns os.ErrNotExist for a missing key
// (boxo BasicDirectory.Find maps mdag.ErrLinkNotFound -> os.ErrNotExist, and a
// missing parent-dir path or a directory-leaf also return os.ErrNotExist) and
// NEVER returns (nil, nil). If the strict reader treated os.ErrNotExist as a read
// failure, resolveVaultView would return vaultUnresolvable for an unfolded BTC
// contract that has no "v" registry yet and refuse every legacy "main" keysign ->
// a withdrawal halt. A value node that is present as a LINK but unreadable is a
// real read error and must fail closed.
//
// This exercises a REAL DataBin rather than a readKey stub, because the defect is
// exactly in how DataBin.Get's os.ErrNotExist is classified - a function stub
// cannot reproduce it (08 caught the first cut, which used a stub). Mutation
// guard: delete the `errors.Is(err, os.ErrNotExist)` branch in
// contractStateReaderAtStrict and the "absent" subtest FAILS (a missing key comes
// back as a read error).
func TestContractStateReaderStrict_AbsentIsNotReadError(t *testing.T) {
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
	ctx := context.Background()

	// "v" stored as a DagCbor value - the production contract-value codec
	// (state-processing/utils.go stores every contract KV value as DagCbor).
	// loadLeafFromCid eagerly recurses ONLY Protobuf links, so a DagCbor value is
	// loaded LAZILY at Find; an unreadable value therefore surfaces as a real error
	// (not os.ErrNotExist). A protobuf merkledag.NodeWithData would be eagerly
	// loaded and would NOT model production - hence PutObject here.
	vCid, err := da.PutObject(map[string]interface{}{"gen": uint64(1)})
	if err != nil {
		t.Fatalf("put v value: %v", err)
	}
	vNode, err := da.DagServ.Get(ctx, *vCid)
	if err != nil {
		t.Fatalf("get v node: %v", err)
	}
	if vNode.Cid().Prefix().Codec == uint64(multicodec.Protobuf) {
		t.Fatalf("test invariant: v must be a non-Protobuf (lazy) value to model production")
	}

	// root WITH "v"
	withV := uio.NewDirectory(da.DagServ)
	if err := withV.AddChild(ctx, "v", vNode); err != nil {
		t.Fatal(err)
	}
	withVNode, err := withV.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := da.DagServ.Add(ctx, withVNode); err != nil {
		t.Fatal(err)
	}

	// root WITHOUT "v" (an unfolded contract: v2 in force, nothing folded yet). A
	// "p" value key makes it a real, non-empty directory that simply lacks "v".
	otherCid, err := da.PutObject(map[string]interface{}{"x": uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	otherNode, err := da.DagServ.Get(ctx, *otherCid)
	if err != nil {
		t.Fatal(err)
	}
	noV := uio.NewDirectory(da.DagServ)
	if err := noV.AddChild(ctx, "p", otherNode); err != nil {
		t.Fatal(err)
	}
	noVNode, err := noV.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := da.DagServ.Add(ctx, noVNode); err != nil {
		t.Fatal(err)
	}

	// PRESENT: a readable "v" resolves (raw bytes, ok, no error).
	t.Run("present resolves", func(t *testing.T) {
		tssMgr := &TssManager{contractState: &fixedOutputState{stateMerkle: withVNode.Cid().String()}, da: da}
		raw, ok, err := tssMgr.contractStateReaderAtStrict("c", 100)("v")
		if err != nil {
			t.Fatalf("present v: unexpected read error: %v", err)
		}
		if !ok || len(raw) == 0 {
			t.Fatalf("present v: want ok+raw, got ok=%v len=%d", ok, len(raw))
		}
	})

	// ABSENT: a missing "v" is NOT a read error. It must come back (nil, false, nil)
	// so resolveVaultView stays vaultAbsent and legacy "main" signing is allowed -
	// otherwise an unfolded contract halts.
	t.Run("absent is not a read error", func(t *testing.T) {
		tssMgr := &TssManager{contractState: &fixedOutputState{stateMerkle: noVNode.Cid().String()}, da: da}
		raw, ok, err := tssMgr.contractStateReaderAtStrict("c", 100)("v")
		if err != nil {
			t.Fatalf("absent v must not be a read error (would halt an unfolded contract): %v", err)
		}
		if ok || raw != nil {
			t.Fatalf("absent v: want (nil,false,nil), got ok=%v raw=%v", ok, raw)
		}
	})

	// READ ERROR: "v" is present as a LINK but its value block is unreadable on the
	// LocalOnly view. The root dir still loads (the DagCbor "v" link is not eagerly
	// recursed), so the failure appears at Find -> a real (non-NotExist) error, and
	// the reader fails closed. The injection is on the LocalOnly DagServ because the
	// strict reader reads through tssMgr.da.LocalOnly().
	t.Run("unreadable value fails closed", func(t *testing.T) {
		local := da.LocalOnly()
		local.DagServ = &failingDagServ{DAGService: local.DagServ, bad: *vCid}
		tssMgr := &TssManager{contractState: &fixedOutputState{stateMerkle: withVNode.Cid().String()}, da: da}
		_, ok, err := tssMgr.contractStateReaderAtStrict("c", 100)("v")
		if err == nil {
			t.Fatalf("unreadable v value must be a read error (fail closed), got ok=%v err=nil", ok)
		}
	})
}
