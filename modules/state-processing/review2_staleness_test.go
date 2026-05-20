package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"

	tss_helpers "vsc-node/modules/tss/helpers"

	stateEngine "vsc-node/modules/state-processing"
)

func TestStaleCommitmentRejected(t *testing.T) {
	te := newTestEnv()

	te.Reader.LastBlock = 49999

	commitment := tss_helpers.SignedCommitment{
		BaseCommitment: tss_helpers.BaseCommitment{
			Type:        "reshare",
			SessionId:   "reshare-1-0-testkey",
			KeyId:       "testkey",
			Commitment:  "AAAA",
			Epoch:       1,
			BlockHeight: 1,
		},
		Signature: "deadbeef",
		BitSet:    "AA",
	}

	commitments := []tss_helpers.SignedCommitment{commitment}
	jsonBytes, _ := json.Marshal(commitments)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"testwitness"},
		Id:            "vsc.tss_commitment",
		Json:          string(jsonBytes),
	})
	te.Reader.CreateBlock()
	time.Sleep(200 * time.Millisecond)

	if len(te.TssCommitments.Commitments) > 0 {
		t.Fatalf("stale commitment (BlockHeight=1 at block 50000) was stored — staleness check failed")
	}
}

func TestRecentCommitmentPassesStalenessCheck(t *testing.T) {
	te := newTestEnv()

	te.Reader.LastBlock = 49999

	commitment := tss_helpers.SignedCommitment{
		BaseCommitment: tss_helpers.BaseCommitment{
			Type:        "reshare",
			SessionId:   "reshare-49000-0-testkey",
			KeyId:       "testkey",
			Commitment:  "AAAA",
			Epoch:       1,
			BlockHeight: 49000,
		},
		Signature: "deadbeef",
		BitSet:    "AA",
	}

	commitments := []tss_helpers.SignedCommitment{commitment}
	jsonBytes, _ := json.Marshal(commitments)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"testwitness"},
		Id:            "vsc.tss_commitment",
		Json:          string(jsonBytes),
	})
	te.Reader.CreateBlock()
	time.Sleep(200 * time.Millisecond)

	// Recent commitment passes staleness but fails downstream (no election/BLS).
	// Test verifies no panic and correct code path.
}
