package datalayer_test

import (
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

// TestLocalOnlyServesLocalBlocks: a block already in the blockstore is served by
// the LocalOnly view exactly as by the full data layer.
func TestLocalOnlyServesLocalBlocks(t *testing.T) {
	da := getDirTestDA(t)
	local := da.LocalOnly()
	require.NotNil(t, local, "LocalOnly must be set after Init")

	c, err := da.PutObject(map[string]interface{}{"hello": "local"})
	require.NoError(t, err)

	node, err := local.GetDag(*c)
	require.NoError(t, err)
	require.Equal(t, c.String(), node.Cid().String())

	raw, err := local.GetRaw(*c)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
}

// TestLocalOnlyMissingCidReturnsAtOnce: a CID the node does not hold comes back
// as an error immediately instead of waiting on the network, and asking for it
// does not store anything.
func TestLocalOnlyMissingCidReturnsAtOnce(t *testing.T) {
	da := getDirTestDA(t)
	local := da.LocalOnly()

	sum, err := mh.Sum([]byte("not stored anywhere "+time.Now().String()), mh.SHA2_256, -1)
	require.NoError(t, err)
	missing := cid.NewCidV1(cid.DagCBOR, sum)

	type res struct{ err error }
	done := make(chan res, 3)
	go func() { _, err := local.GetDag(missing); done <- res{err} }()
	go func() { _, err := local.GetRaw(missing); done <- res{err} }()
	go func() { _, err := local.Get(missing, nil); done <- res{err} }()
	for i := 0; i < 3; i++ {
		select {
		case r := <-done:
			require.Error(t, r.err, "a missing CID must be an error, not data")
		case <-time.After(5 * time.Second):
			t.Fatal("LocalOnly read of a missing CID did not return; it is waiting on the network")
		}
	}

	_, err = local.GetRaw(missing)
	require.Error(t, err, "the failed read must not have stored the CID")
}

// TestLocalOnlyWriteStaysLocal: a write through the view (no network side) is
// stored and does not panic in the announce step.
func TestLocalOnlyWriteStaysLocal(t *testing.T) {
	da := getDirTestDA(t)
	local := da.LocalOnly()

	c, err := local.PutObject(map[string]interface{}{"written": "via local view"})
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond) // PutObject announces on a goroutine
	_, err = da.GetDag(*c)
	require.NoError(t, err)
}
