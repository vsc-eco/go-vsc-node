package chain

import (
	"context"
	"testing"
	"time"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/ipfs/boxo/ipld/merkledag"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
)

type fixedOutputState struct {
	contracts.ContractState
	stateMerkle string
}

func (f *fixedOutputState) GetLastOutput(string, uint64) (contracts.ContractOutput, error) {
	return contracts.ContractOutput{StateMerkle: f.stateMerkle}, nil
}

// Relay ticks run on goroutines with no overlap guard, so reading the contract's
// stored height must not wait on the network: a height value whose block this
// node does not hold must come back as an error at once.
func TestContractBlockHeightDoesNotBlockOnMissingValue(t *testing.T) {
	identityConfig, p2pConfig := common.NewIdentityConfig(), p2pInterface.NewConfig()
	p2p := p2pInterface.New(witnesses.NewEmptyWitnesses(), p2pConfig, identityConfig, systemconfig.MocknetConfig(), nil)
	da := DataLayer.New(p2p)
	agg := aggregate.New([]aggregate.Plugin{identityConfig, p2pConfig, p2p, da})
	if err := agg.Init(); err != nil {
		t.Fatalf("init datalayer: %v", err)
	}
	if _, err := agg.Start().Await(context.Background()); err != nil {
		t.Fatalf("start datalayer: %v", err)
	}
	t.Cleanup(func() { _ = agg.Stop() })
	ctx := context.Background()

	missingValue := merkledag.NewRawNode([]byte("123456")) // never stored
	root := uio.NewDirectory(da.DagServ)
	if err := root.AddChild(ctx, lastHeightStateKey, missingValue); err != nil {
		t.Fatal(err)
	}
	rootNode, err := root.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := da.DagServ.Add(ctx, rootNode); err != nil {
		t.Fatal(err)
	}

	c := &ChainOracle{contractState: &fixedOutputState{stateMerkle: rootNode.Cid().String()}, da: da}
	type res struct{ err error }
	done := make(chan res, 1)
	go func() {
		_, err := c.getContractBlockHeight("vsc1contract")
		done <- res{err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("a height value this node does not hold must be an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("getContractBlockHeight blocked on a value block this node does not hold")
	}
}
