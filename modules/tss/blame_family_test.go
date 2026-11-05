package tss

import (
	"testing"

	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/require"
)

// 18an regression: a fault on ONE bridge's keys must not evict a witness from
// another bridge's committee.
//
// BlameScore summed the blames of every key in an epoch and the resulting ban
// excluded the node from keygen, signing AND reshare of EVERY key. Once the EVM
// bridge holds keys, a witness with an EVM-only fault — a lost EVM share, an
// EVM-specific bug — is blamed in every EVM session, crosses the 60% threshold,
// and is then removed from the BTC vault's committee too, though its BTC shares
// are perfectly good. The reverse holds as well.
//
// This is the property the devnet proof would demonstrate under real faults.
// It is deterministic, so it can be proven here: BlameScore is a pure function
// of (election, blame commitments).

const (
	btcFamily = "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d"
	evmFamily = "vsc1EvmBridgeContractIdForTheseTests00"
)

// blameAllEpochs puts the same blame against `indices` in every epoch from
// `from` to `to`, for one key — enough volume to clear both the 60% rate and
// the per-family minimum blame count.
func blameAllEpochs(keyId string, from, to uint64, indices ...int) map[uint64][]tss_db.TssCommitment {
	out := map[uint64][]tss_db.TssCommitment{}
	for e := from; e <= to; e++ {
		out[e] = append(out[e], tss_db.TssCommitment{
			Type: "blame", Epoch: e, KeyId: keyId, Commitment: makeBlameBitset(indices...),
		})
	}
	return out
}

func TestBlame18an_EvmOnlyFaultDoesNotBanFromBtc(t *testing.T) {
	members := []string{"alice", "bob", "carol", "dave", "eve"}
	const current = uint64(20)

	// Deep enough history that nobody is inside the 3-epoch ban grace period.
	var prev []elections.ElectionResult
	for e := uint64(10); e < current; e++ {
		prev = append(prev, makeElectionResult(e, members))
	}
	cur := makeElectionResult(current, members)

	// "carol" (index 2) is blamed in EVERY EVM session and never in a BTC one.
	//
	// The BTC key is given a clean session in only SOME epochs, so the
	// NETWORK-WIDE rate is 11/16 = 68% and clears the 60% threshold. That
	// matters: if the global ban does not fire, this test passes trivially and
	// proves nothing about the split.
	//
	// One commit per (key, epoch) deliberately: MockTssCommitmentsDb runs
	// DedupCommitmentsBySemanticKey, so two identical commits in one epoch
	// collapse into one. An earlier fixture used two per epoch to reach 66% and
	// silently got 50% instead, which is how this was caught.
	blames := map[uint64][]tss_db.TssCommitment{}
	for e := uint64(10); e <= current; e++ {
		blames[e] = []tss_db.TssCommitment{
			{Type: "blame", Epoch: e, KeyId: evmFamily + "-ethmain", Commitment: makeBlameBitset(2)},
		}
		if e%2 == 0 {
			// The BTC key has sessions (a denominator) but no blame against carol.
			blames[e] = append(blames[e], tss_db.TssCommitment{
				Type: "blame", Epoch: e, KeyId: btcFamily + "-mainv2", Commitment: makeBlameBitset(),
			})
		}
	}

	mgr := newBlameScoreMgr(cur, prev, blames)
	score := mgr.BlameScore(cur)
	score.PerFamilyActive = true

	// The global score still bans carol — that is the old behaviour, unchanged.
	require.True(t, score.BannedNodes["carol"],
		"fixture precondition: the network-wide score must still ban carol, or this test proves nothing")

	// THE POINT. Banned for the EVM key...
	require.True(t, score.BannedFor(evmFamily + "-ethmain")["carol"],
		"carol faults every EVM session and must be banned from the EVM key")

	// ...and NOT for the BTC key.
	require.False(t, score.BannedFor(btcFamily + "-mainv2")["carol"],
		"an EVM-only fault must NOT evict carol from the BTC vault's committee — this is 18an")

	// And nobody else is banned anywhere.
	for _, m := range members {
		if m == "carol" {
			continue
		}
		require.False(t, score.BannedFor(evmFamily + "-ethmain")[m], "%s should not be banned", m)
		require.False(t, score.BannedFor(btcFamily + "-mainv2")[m], "%s should not be banned", m)
	}
}

// The gate must be a true no-op: with it off, every lookup returns the global
// set, so a rolling deploy cannot have two nodes building different party lists.
func TestBlame18an_GateOffIsTheGlobalSet(t *testing.T) {
	members := []string{"alice", "bob", "carol", "dave", "eve"}
	const current = uint64(20)
	var prev []elections.ElectionResult
	for e := uint64(10); e < current; e++ {
		prev = append(prev, makeElectionResult(e, members))
	}
	cur := makeElectionResult(current, members)

	mgr := newBlameScoreMgr(cur, prev, blameAllEpochs(evmFamily+"-ethmain", 10, current, 2))
	score := mgr.BlameScore(cur)
	score.PerFamilyActive = false

	require.True(t, score.BannedNodes["carol"])
	require.True(t, score.BannedFor(btcFamily + "-mainv2")["carol"],
		"with the gate off the BTC lookup must return the GLOBAL set, i.e. still banned")
	require.Equal(t, score.BanOrder, score.BanOrderFor(btcFamily+"-mainv2"))
}

// A key id with no resolvable owner falls back to the global set: it was not
// created through the contract path, so it has no family, and the conservative
// answer is the network-wide decision rather than "nobody is banned".
func TestBlame18an_UnattributableKeyFallsBackToGlobal(t *testing.T) {
	members := []string{"alice", "bob", "carol", "dave", "eve"}
	const current = uint64(20)
	var prev []elections.ElectionResult
	for e := uint64(10); e < current; e++ {
		prev = append(prev, makeElectionResult(e, members))
	}
	cur := makeElectionResult(current, members)

	mgr := newBlameScoreMgr(cur, prev, blameAllEpochs(evmFamily+"-ethmain", 10, current, 2))
	score := mgr.BlameScore(cur)
	score.PerFamilyActive = true

	require.True(t, score.BannedFor("legacyKeyWithNoOwner")["carol"],
		"an unattributable key must use the global set, not an empty one")
}

// The per-family minimum blame count. A single blame in a family is a 100%
// failure rate against a denominator of one, which would ban on noise — the
// whole reason the notes asked for a floor alongside the split.
func TestBlame18an_MinimumFamilyBlameCountPreventsNoiseBans(t *testing.T) {
	members := []string{"alice", "bob", "carol", "dave", "eve"}
	const current = uint64(20)
	var prev []elections.ElectionResult
	for e := uint64(10); e < current; e++ {
		prev = append(prev, makeElectionResult(e, members))
	}
	cur := makeElectionResult(current, members)

	// Exactly ONE blame against carol in the whole window for this family.
	blames := map[uint64][]tss_db.TssCommitment{
		current: {{Type: "blame", Epoch: current, KeyId: evmFamily + "-ethmain", Commitment: makeBlameBitset(2)}},
	}
	mgr := newBlameScoreMgr(cur, prev, blames)
	score := mgr.BlameScore(cur)
	score.PerFamilyActive = true

	require.Less(t, 1, TSS_BAN_MIN_FAMILY_BLAMES, "fixture assumes the floor is above 1")
	require.False(t, score.BannedFor(evmFamily + "-ethmain")["carol"],
		"one blame is a 100 percent rate over a denominator of 1; the floor must stop it banning")
}
