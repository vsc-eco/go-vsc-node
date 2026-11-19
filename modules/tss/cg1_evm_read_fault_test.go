package tss

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db"
	tss_db "vsc-node/modules/db/vsc/tss"

	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/multiformats/go-multicodec"
	"go.mongodb.org/mongo-driver/mongo"
)

// CG-1: an EVM vault signature needs every listed signer, so a node that
// refuses because of a read only IT failed to make (its own Mongo query, a
// state block it does not hold, its own tss_keys query) stops the signature on
// every node. A signature the gate must stop is still stopped by any one signer
// that reads the state. These tests run the real gate (evmSignRefused) over a
// real datalayer; they build on the tree before the fix too, where the three
// read-fault cases are refused.

// cg1Keys answers FindKey from keys, and a missing key as tss_keys does
// (mongo.ErrNoDocuments); err, when set, is this node's tss_keys query failing.
type cg1Keys struct {
	stubTssKeys
	keys map[string]tss_db.TssKey
	err  error
}

func (k cg1Keys) FindKey(id string) (tss_db.TssKey, error) {
	if k.err != nil {
		return tss_db.TssKey{}, k.err
	}
	if v, ok := k.keys[id]; ok {
		return v, nil
	}
	return tss_db.TssKey{}, mongo.ErrNoDocuments
}

// committedRawState builds a contract state directory whose values are raw
// blocks, as the block producer stores them (MakeOutputs), and returns its
// root CID. With store false the root is not written to this node's
// blockstore (the values are): a state this node does not hold.
func committedRawState(t *testing.T, da *datalayer.DataLayer, values map[string][]byte, store bool) string {
	t.Helper()
	ctx := context.Background()
	dir := uio.NewDirectory(da.DagServ)
	for k, v := range values {
		c, err := da.PutRaw(v, common_types.PutRawOptions{Codec: multicodec.Raw})
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
	if store {
		if err := da.DagServ.Add(ctx, root); err != nil {
			t.Fatal(err)
		}
	}
	return root.Cid().String()
}

// cg1Bridge is a rotated 0.13.0 bridge: gen 0 Draining (no open sweep), gen 1
// Active. held is its committed state; notHeld is a state root this node does
// not hold.
func cg1Bridge(t *testing.T) (mgr *TssManager, held, notHeld string) {
	t.Helper()
	da := startTestDatalayer(t)
	reg := registryBytes(scopeVault{0, btcvault.VaultStatusDraining}, scopeVault{1, btcvault.VaultStatusActive})
	held = committedRawState(t, da, map[string][]byte{"v": reg, "chainid": []byte("1")}, true)
	notHeld = committedRawState(t, da, map[string][]byte{"v": reg, "chainid": []byte("1"), "n": []byte("7")}, false)
	if held == notHeld {
		t.Fatal("the two states must differ")
	}
	mgr, _ = evmOnlyManager(t, consensusversion.V0_13_0, map[string]string{"ETH": evmBridge})
	mgr.da = da
	mgr.tssKeys = cg1Keys{keys: map[string]tss_db.TssKey{
		evmBridge + "-primary":   activeKey(evmPub1),
		evmBridge + "-primaryv1": activeKey(evmPub2),
	}}
	return mgr, held, notHeld
}

const (
	cg1Gen0 = evmBridge + "-primary"   // Draining
	cg1Gen1 = evmBridge + "-primaryv1" // Active
)

var cg1Junk = make([]byte, 32) // no sweep, no withdrawal, no check message

// A healthy node: the active generation signs; the draining one is refused a
// digest that is not its sweep or its own withdrawal.
func TestCG1_HealthyNodeScopesTheRotatedBridge(t *testing.T) {
	mgr, held, _ := cg1Bridge(t)
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: held}}
	if mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Fatal("active generation refused on a healthy node")
	}
	if !mgr.evmSignRefused(cg1Gen0, cg1Junk, 1000) {
		t.Fatal("draining generation signed an arbitrary digest on a healthy node")
	}
	if !mgr.evmSignRefused(evmBridge+"-primaryv5", cg1Junk, 1000) {
		t.Fatal("a generation the registry does not list was signed")
	}
}

// This node's own read failures: its contract-output query, a state root it
// does not hold, its tss_keys query. None refuses (before the fix each one
// refused: the first two read the registry as absent, which refuses every
// generation after the first; the third failed the committed-key lookup).
func TestCG1_ThisNodesReadFaultDoesNotRefuse(t *testing.T) {
	mgr, held, notHeld := cg1Bridge(t)

	mgr.contractState = &heightOutputs{err: errors.New("connection reset by peer")}
	if mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Error("contract-output query failed on this node: the active generation was refused")
	}

	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: notHeld}}
	if mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Error("state root not held by this node: the active generation was refused")
	}

	// The draining generation's checks need its committed key; this node's
	// tss_keys query fails.
	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: held}}
	keys := mgr.tssKeys.(cg1Keys)
	keys.err = errors.New("server selection timeout")
	mgr.tssKeys = keys
	if mgr.evmSignRefused(cg1Gen0, cg1Junk, 1000) {
		t.Error("tss_keys query failed on this node: the draining generation was refused")
	}
}

// A bridge before its first rotation signs with its one key; before the fix
// this node's failed output query refused it through the halt-flag read.
func TestCG1_PreRotationKeySurvivesThisNodesReadFault(t *testing.T) {
	mgr, _, _ := cg1Bridge(t)
	mgr.contractState = &heightOutputs{err: errors.New("connection reset by peer")}
	if mgr.evmSignRefused(cg1Gen0, cg1Junk, 1000) {
		t.Fatal("contract-output query failed on this node: the pre-rotation key was refused")
	}
}

// What the committed state says is still a refusal, on every node: an output
// that does not decode, a generation the registry does not list, no registry.
func TestCG1_DeterministicRefusalsStay(t *testing.T) {
	mgr, held, _ := cg1Bridge(t)

	mgr.contractState = &heightOutputs{err: fmt.Errorf("%w: cannot decode", db.ErrDecode)}
	if !mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Error("an undecodable committed output was signed")
	}

	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{10: held}}
	if !mgr.evmSignRefused(evmBridge+"-primaryv5", cg1Junk, 1000) {
		t.Error("a generation the registry does not list was signed")
	}

	mgr.contractState = &heightOutputs{byHeight: map[uint64]string{}} // no output yet
	if !mgr.evmSignRefused(cg1Gen1, cg1Junk, 1000) {
		t.Error("a generation key of a bridge with no registry was signed")
	}
	if mgr.evmSignRefused(cg1Gen0, cg1Junk, 1000) {
		t.Error("the pre-rotation key of a bridge with no output was refused")
	}
}
