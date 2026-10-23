package witnesses_test

import (
	"context"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B1 (testnet 2026-10-22): GetWitnessesByPeerId returned every account that had
// EVER announced a peer id, in map order, and TSS took the first. These pin the
// lookup the attribution now relies on.

// b1Store is the store plus its current-row lookup (asserted the way TSS does).
type b1Store interface {
	witnesses.Witnesses
	GetCurrentWitnessesByPeerId(peerIds []string) ([]witnesses.Witness, error)
}

func b1Witnesses(t *testing.T) b1Store {
	t.Helper()
	// Own config dir: the rename is persisted, and must not leak into the package's
	// shared test config. Init loads the defaults first; renaming before it drops the URI.
	conf := db.NewDbConfig(t.TempDir())
	require.NoError(t, conf.Init())
	require.NoError(t, conf.SetDbName("go-vsc-b1-peerid-test"))
	d := db.New(conf)
	vscDb := vsc.New(d, conf)
	w := witnesses.New(vscDb)
	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{d, vscDb, w}))
	dropper, ok := w.(interface{ Drop(context.Context) error })
	require.True(t, ok)
	require.NoError(t, dropper.Drop(context.Background()))
	t.Cleanup(func() { _ = dropper.Drop(context.Background()) })
	store, ok := w.(b1Store)
	require.True(t, ok, "the production store implements the current-row lookup")
	return store
}

func b1Announce(t *testing.T, w witnesses.Witnesses, account, peerId string, height uint64, popOk bool) {
	t.Helper()
	meta := witnesses.PostingJsonMetadata{
		Services: []string{"vsc.network"},
		DidKeys:  []witnesses.PostingJsonKeys{{CryptoType: "DID-BLS", Type: "consensus", Key: "did:key:" + account}},
	}
	meta.VscNode.PeerId = peerId
	require.NoError(t, w.SetWitnessUpdate(witnesses.SetWitnessUpdateType{
		Account: account, Height: height, TxId: "tx-" + account, BlockId: "b", Metadata: meta, PeerIdPoPOk: popOk,
	}))
}

func accountsOf(ws []witnesses.Witness) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Account
	}
	return out
}

// An account that announced a peer id and later moved to another one no longer
// answers for the old peer id.
func TestB1_PeerIdLookupUsesTheLatestAnnouncementOnly(t *testing.T) {
	w := b1Witnesses(t)
	b1Announce(t, w, "mover", "peer-old", 10, false)
	b1Announce(t, w, "mover", "peer-new", 20, false)

	old, err := w.GetCurrentWitnessesByPeerId([]string{"peer-old"})
	require.NoError(t, err)
	assert.Empty(t, old, "the account moved on: its old peer id no longer resolves to it")
	cur, err := w.GetCurrentWitnessesByPeerId([]string{"peer-new"})
	require.NoError(t, err)
	assert.Equal(t, []string{"mover"}, accountsOf(cur))
}

// A squatter's newer, unproven row never outranks the member's proven one.
func TestB1_PeerIdLookupRanksProvenRowsFirst(t *testing.T) {
	w := b1Witnesses(t)
	b1Announce(t, w, "member", "peer-x", 30, true)
	b1Announce(t, w, "squatter", "peer-x", 40, false)
	got, err := w.GetCurrentWitnessesByPeerId([]string{"peer-x"})
	require.NoError(t, err)
	assert.Equal(t, []string{"member", "squatter"}, accountsOf(got))
}

// Without proofs the order is still the same on every node: newest, then account.
func TestB1_PeerIdLookupIsDeterministic(t *testing.T) {
	w := b1Witnesses(t)
	b1Announce(t, w, "zed", "peer-y", 50, false)
	b1Announce(t, w, "amy", "peer-y", 50, false)
	b1Announce(t, w, "old", "peer-y", 45, false)
	for i := 0; i < 5; i++ {
		got, err := w.GetCurrentWitnessesByPeerId([]string{"peer-y"})
		require.NoError(t, err)
		assert.Equal(t, []string{"amy", "zed", "old"}, accountsOf(got))
	}
}

// The old lookup (the election proposer's presence check) keeps its meaning: an
// account that ever announced the peer id is still returned.
func TestB1_OldPeerIdLookupIsUnchanged(t *testing.T) {
	w := b1Witnesses(t)
	b1Announce(t, w, "mover", "peer-old", 10, false)
	b1Announce(t, w, "mover", "peer-new", 20, false)
	old, err := w.GetWitnessesByPeerId([]string{"peer-old"})
	require.NoError(t, err)
	assert.Equal(t, []string{"mover"}, accountsOf(old), "old lookup unchanged")
}
