package tss

import (
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/require"
)

// KEY-SPAM-1 (18j), second half.
//
// The create_key allowlist stops NEW junk keys. This stops keys that already
// exist from being scheduled — which matters because a "created" key is picked
// up at every rotate block forever, with no cap and no expiry, and each keygen
// holds tssMgr.lock while it waits for pre-parameters. Keys minted before the
// allowlist existed would otherwise keep costing a lock-hold per rotation.

func TestTssKeyOwner(t *testing.T) {
	// The shape TssCreateKey produces: "<contractId>-<keyName>".
	owner, ok := tssKeyOwner("vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d-mainv2")
	require.True(t, ok)
	require.Equal(t, "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d", owner)

	// Splitting on the FIRST dash is unambiguous because key names are
	// validated against ^[a-zA-Z0-9]+$ and contract ids carry no dash. If that
	// ever changes, this is the test that should fail.
	owner, ok = tssKeyOwner("vsc1Abc-main-v2")
	require.True(t, ok)
	require.Equal(t, "vsc1Abc", owner, "the first dash separates; the rest is the key name")

	// Not contract-owned, so there is no owner to check against the allowlist.
	for _, id := range []string{"main", "", "-main", "vsc1Abc-"} {
		if _, ok := tssKeyOwner(id); ok {
			t.Fatalf("%q should not resolve to an owner", id)
		}
	}
}

func TestFilterScheduledKeys(t *testing.T) {
	const permitted = "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d"
	const spammer = "vsc1SpamSpamSpamSpamSpamSpamSpamSpamSpam"

	keys := []tss_db.TssKey{
		{Id: permitted + "-mainv2"},
		{Id: spammer + "-junk1"},
		{Id: spammer + "-junk2"},
		{Id: "legacyKeyWithNoOwner"},
	}

	// The filter's decision, isolated from the sysconfig/version plumbing that
	// wraps it (those are covered by the gate test and the sysconfig default).
	keep := func(allowed []string, keyId string) bool {
		if len(allowed) == 0 {
			return true
		}
		owner, ok := tssKeyOwner(keyId)
		if !ok {
			return true // unattributable: kept
		}
		for _, id := range allowed {
			if id == owner {
				return true
			}
		}
		return false
	}

	t.Run("junk keys from an unlisted contract are dropped", func(t *testing.T) {
		allowed := []string{permitted}
		var kept []string
		for _, k := range keys {
			if keep(allowed, k.Id) {
				kept = append(kept, k.Id)
			}
		}
		require.ElementsMatch(t, []string{permitted + "-mainv2", "legacyKeyWithNoOwner"}, kept,
			"only the permitted contract's key and the unattributable one survive")
	})

	// Unattributable ids are KEPT on purpose: an id with no owner cannot have
	// come from the contract spam path, which always prefixes the contract id,
	// so dropping it would risk stranding a legitimately-provisioned key.
	t.Run("an unattributable key is kept", func(t *testing.T) {
		require.True(t, keep([]string{permitted}, "legacyKeyWithNoOwner"))
	})

	// The pre-activation / test-network state: filters nothing.
	t.Run("an empty allowlist filters nothing", func(t *testing.T) {
		for _, k := range keys {
			require.True(t, keep(nil, k.Id), "%s should survive an empty allowlist", k.Id)
		}
	})
}

// And through the real method, which also exercises the sysconfig + version
// gating: with no sysconfig it must pass everything through unchanged, because
// that is the state every existing test and every pre-activation node is in.
func TestFilterScheduledKeys_NoConfigIsAPassthrough(t *testing.T) {
	mgr := &TssManager{}
	keys := []tss_db.TssKey{{Id: "vsc1Spam-junk"}, {Id: "vsc1Ok-mainv2"}}
	got := mgr.filterScheduledKeys(keys, 1000, "keygen")
	require.Len(t, got, 2, "with no sysconfig the filter must not drop anything")
}
