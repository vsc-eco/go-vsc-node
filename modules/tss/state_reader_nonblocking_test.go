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
