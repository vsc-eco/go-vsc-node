package state_engine

// review7 C9 — a witness announce whose consensus BLS key fails
// proof-of-possession was only warned about and the key was stored anyway,
// leaving a rogue-key aggregate-signature forgery vector. The PoP failure was
// always detectable (verifyAnnouncedBlsPoP returned an error); the bug was that
// the error was ignored. Strict mode now rejects such an announce, while still
// accepting an announce that carries no consensus BLS key (no forgeable key).

import (
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/witnesses"

	ethBls "github.com/protolambda/bls12-381-util"
	"github.com/stretchr/testify/require"
)

func blsDIDFromSeed(t *testing.T, s string) (didStr string, privKey *dids.BlsPrivKey) {
	t.Helper()
	var seed [32]byte
	copy(seed[:], []byte(s))
	pk := dids.BlsPrivKey{}
	pk.Deserialize(&seed)
	pub, err := ethBls.SkToPk(&pk)
	require.NoError(t, err)
	did, err := dids.NewBlsDID(pub)
	require.NoError(t, err)
	return did.String(), &pk
}

func TestAuditReview7_C9_RejectsBlsKeyWithoutValidPoP(t *testing.T) {
	const account = "hive:witness1"
	didStr, privKey := blsDIDFromSeed(t, "review7c9seed1")

	validPoP, err := dids.GenerateBlsPoP(privKey, account)
	require.NoError(t, err)

	consensusKey := func(pop string) witnesses.PostingJsonMetadata {
		return witnesses.PostingJsonMetadata{
			DidKeys: []witnesses.PostingJsonKeys{
				{CryptoType: "DID-BLS", Type: "consensus", Key: didStr, PoP: pop},
			},
		}
	}

	// The PoP failure was always detectable — the pre-fix bug was ignoring it.
	require.Error(t, verifyAnnouncedBlsPoP(consensusKey(""), account),
		"missing PoP must be a detectable error")

	// Valid PoP → accepted (not rogue).
	require.False(t, witnessAnnounceHasRogueBlsKey(consensusKey(validPoP), account),
		"a consensus BLS key with a valid PoP must be accepted")

	// Missing / garbage PoP → rejected (rogue-key vector).
	require.True(t, witnessAnnounceHasRogueBlsKey(consensusKey(""), account),
		"a consensus BLS key with no PoP must be rejected")
	require.True(t, witnessAnnounceHasRogueBlsKey(consensusKey("not-a-valid-pop"), account),
		"a consensus BLS key with a garbage PoP must be rejected")

	// Valid PoP but bound to a DIFFERENT account → rejected (account binding).
	require.True(t, witnessAnnounceHasRogueBlsKey(consensusKey(validPoP), "hive:attacker"),
		"a PoP bound to another account must not pass")

	// No consensus BLS key at all → allowed (carries no forgeable key).
	require.False(t, witnessAnnounceHasRogueBlsKey(witnesses.PostingJsonMetadata{}, account),
		"an announce with no consensus BLS key must be allowed")
	require.False(t, witnessAnnounceHasRogueBlsKey(witnesses.PostingJsonMetadata{
		DidKeys: []witnesses.PostingJsonKeys{{CryptoType: "DID-BLS", Type: "validation", Key: didStr}},
	}, account), "a non-consensus BLS key must not trigger rejection")
}
