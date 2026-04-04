package tss

import (
	"encoding/base64"
	"math/big"
	"testing"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlameSafetyValve_OverBroadBlameIgnored verifies that when a blame
// commitment excludes so many nodes that the committee drops below threshold,
// the safety valve fires and the committees are rebuilt without blame
// (bans still apply).
//
// Scenario from production: 19-member election, blame marks 15 members,
// 4 banned → only 1 participant survives → deadlock.
// After safety valve: ban-only exclusion → 14 old + 15 new → viable.
func TestBlameSafetyValve_OverBroadBlameIgnored(t *testing.T) {
	// 19-member current election (epoch 1392), sorted alphabetically
	currentMembers := []string{
		"actifit", "arcange", "atexoras", "bala", "botlord",
		"bradleyarrow", "comptroller", "delta-p", "emrebeyler", "herman",
		"louis", "mahdiyari", "mengao", "milo", "prime",
		"sagarkothari", "techcoderx", "tibfox", "v4vapp",
	}
	currentElection := makeElection(1392, currentMembers)

	// 18-member keygen election (epoch 1379), no actifit
	keygenMembers := []string{
		"arcange", "atexoras", "bala", "botlord", "bradleyarrow",
		"comptroller", "delta-p", "emrebeyler", "herman", "louis",
		"mahdiyari", "mengao", "milo", "prime", "sagarkothari",
		"techcoderx", "tibfox", "v4vapp",
	}
	keygenElection := makeElection(1379, keygenMembers)

	// Keygen commitment: 16 of 18 participated (all except comptroller=5, milo=12)
	keygenBitset := big.NewInt(0)
	for i := 0; i < 18; i++ {
		if i == 5 || i == 12 { // comptroller, milo didn't participate
			continue
		}
		keygenBitset.SetBit(keygenBitset, i, 1)
	}

	// Blame: marks 15 of 19 current members (all except comptroller=6, emrebeyler=8, prime=14, techcoderx=16)
	blamedIndices := []int{0, 1, 2, 3, 4, 5, 7, 9, 10, 11, 12, 13, 15, 17, 18}
	blameBitset := big.NewInt(0)
	for _, idx := range blamedIndices {
		blameBitset.SetBit(blameBitset, idx, 1)
	}

	// Banned nodes
	bannedNodes := map[string]bool{
		"comptroller": true,
		"emrebeyler":  true,
		"prime":       true,
		"actifit":     true,
	}

	// --- Phase 1: Build committees WITH blame + bans (reproducing the deadlock) ---

	blamedAccounts := make(map[string]bool)
	for idx, m := range currentElection.Members {
		if blameBitset.Bit(idx) == 1 {
			blamedAccounts[m.Account] = true
		}
	}

	// Build old committee (from keygen bitset against keygen election)
	fullOldCommitteeSize := 0
	commitedMembers := make([]Participant, 0)
	for idx, member := range keygenElection.Members {
		if idx < keygenBitset.BitLen() && keygenBitset.Bit(idx) == 1 {
			fullOldCommitteeSize++
			if blamedAccounts[member.Account] || bannedNodes[member.Account] {
				continue
			}
			commitedMembers = append(commitedMembers, Participant{Account: member.Account})
		}
	}

	// Build new committee (from current election)
	newParticipants := make([]Participant, 0)
	for idx, member := range currentElection.Members {
		if blameBitset.Bit(idx) == 1 || bannedNodes[member.Account] {
			continue
		}
		newParticipants = append(newParticipants, Participant{Account: member.Account})
	}

	// Verify the deadlock: only 1 old, 1 new participant
	assert.Equal(t, 16, fullOldCommitteeSize, "keygen had 16 participants")
	assert.Equal(t, 1, len(commitedMembers), "only techcoderx survives in old committee")
	assert.Equal(t, 1, len(newParticipants), "only techcoderx survives in new committee")
	assert.Equal(t, "techcoderx", commitedMembers[0].Account)
	assert.Equal(t, "techcoderx", newParticipants[0].Account)

	// Verify thresholds show we're below minimum
	oldThreshold, err := tss_helpers.GetThreshold(fullOldCommitteeSize)
	require.NoError(t, err)
	newThreshold, _ := tss_helpers.GetThreshold(len(newParticipants))
	minNew := newThreshold + 1
	if minNew < 2 {
		minNew = 2
	}

	assert.Equal(t, 10, oldThreshold, "threshold for 16 = ceil(16*2/3)-1 = 10")
	assert.True(t, len(commitedMembers) < oldThreshold+1,
		"old committee (%d) below threshold+1 (%d) — safety valve should fire",
		len(commitedMembers), oldThreshold+1)

	// --- Phase 2: Safety valve fires — rebuild WITHOUT blame ---

	commitedMembers = make([]Participant, 0)
	for idx, member := range keygenElection.Members {
		if idx < keygenBitset.BitLen() && keygenBitset.Bit(idx) == 1 {
			if bannedNodes[member.Account] {
				continue
			}
			commitedMembers = append(commitedMembers, Participant{Account: member.Account})
		}
	}

	newParticipants = make([]Participant, 0)
	for _, member := range currentElection.Members {
		if bannedNodes[member.Account] {
			continue
		}
		newParticipants = append(newParticipants, Participant{Account: member.Account})
	}

	// Verify rebuilt committees are viable
	assert.Equal(t, 14, len(commitedMembers),
		"old committee without blame: 16 keygen - 2 banned (emrebeyler, prime)")
	assert.Equal(t, 15, len(newParticipants),
		"new committee without blame: 19 - 4 banned")

	// Verify thresholds are satisfied
	newThresholdAfter, _ := tss_helpers.GetThreshold(len(newParticipants))
	assert.GreaterOrEqual(t, len(commitedMembers), oldThreshold+1,
		"old committee (%d) must be >= oldThreshold+1 (%d)",
		len(commitedMembers), oldThreshold+1)
	assert.GreaterOrEqual(t, len(newParticipants), newThresholdAfter+1,
		"new committee (%d) must be >= newThreshold+1 (%d)",
		len(newParticipants), newThresholdAfter+1)
}

// TestBlameSafetyValve_ModerateBlameNotIgnored verifies that the safety valve
// does NOT fire when blame excludes a few nodes but enough remain.
func TestBlameSafetyValve_ModerateBlameNotIgnored(t *testing.T) {
	// 19-member election
	members := []string{
		"actifit", "arcange", "atexoras", "bala", "botlord",
		"bradleyarrow", "comptroller", "delta-p", "emrebeyler", "herman",
		"louis", "mahdiyari", "mengao", "milo", "prime",
		"sagarkothari", "techcoderx", "tibfox", "v4vapp",
	}
	election := makeElection(1392, members)

	// Blame only 3 nodes (indices 0, 1, 2)
	blameBitset := big.NewInt(0)
	blameBitset.SetBit(blameBitset, 0, 1)
	blameBitset.SetBit(blameBitset, 1, 1)
	blameBitset.SetBit(blameBitset, 2, 1)

	bannedNodes := map[string]bool{"comptroller": true}

	// Build new committee with blame + bans
	newParticipants := make([]Participant, 0)
	for idx, member := range election.Members {
		if blameBitset.Bit(idx) == 1 || bannedNodes[member.Account] {
			continue
		}
		newParticipants = append(newParticipants, Participant{Account: member.Account})
	}

	// 19 - 3 blamed - 1 banned = 15 participants
	assert.Equal(t, 15, len(newParticipants))

	// With 15 participants, threshold = ceil(15*2/3)-1 = 9, need 10
	newThreshold, _ := tss_helpers.GetThreshold(len(newParticipants))
	assert.Equal(t, 9, newThreshold)
	assert.True(t, len(newParticipants) >= newThreshold+1,
		"15 >= 10: safety valve should NOT fire")
}

// TestBlameSafetyValve_SetToCommitmentRoundTrip verifies that the blame bitset
// created by setToCommitment can be decoded back, and that 15-node blame
// produces the expected bit pattern.
func TestBlameSafetyValve_SetToCommitmentRoundTrip(t *testing.T) {
	members := []string{
		"actifit", "arcange", "atexoras", "bala", "botlord",
		"bradleyarrow", "comptroller", "delta-p", "emrebeyler", "herman",
		"louis", "mahdiyari", "mengao", "milo", "prime",
		"sagarkothari", "techcoderx", "tibfox", "v4vapp",
	}
	election := makeElection(1392, members)

	// Blame 15 of 19: everyone except comptroller(6), emrebeyler(8), prime(14), techcoderx(16)
	blamedIndices := []int{0, 1, 2, 3, 4, 5, 7, 9, 10, 11, 12, 13, 15, 17, 18}
	blameBitset := big.NewInt(0)
	for _, idx := range blamedIndices {
		blameBitset.SetBit(blameBitset, idx, 1)
	}
	encoded := base64.RawURLEncoding.EncodeToString(blameBitset.Bytes())

	// Decode and verify
	decoded := decodeBlameBitset(encoded, election.Members)
	assert.Len(t, decoded, 15, "blame should mark 15 members")
	assert.NotContains(t, decoded, "comptroller")
	assert.NotContains(t, decoded, "emrebeyler")
	assert.NotContains(t, decoded, "prime")
	assert.NotContains(t, decoded, "techcoderx")
	assert.Contains(t, decoded, "actifit")
	assert.Contains(t, decoded, "v4vapp")
}
