package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	stateEngine "vsc-node/modules/state-processing"
	tss_helpers "vsc-node/modules/tss/helpers"
)

// M-1: one transaction must not be able to make every node in the network do
// unbounded work.
//
// The vsc.tss_commitment ingest loop does a staleness check, a database lookup, a
// CID hash, a BLS circuit deserialisation and a pairing verification PER ELEMENT,
// and the array arrived off an unauthenticated custom_json payload with no length
// check at all. A single cheap transaction carrying a few hundred thousand entries
// therefore imposed all of that on the whole fleet — a denial of service needing
// no privilege and no stake.
func TestM1_OversizedCommitmentBundleIsRejected(t *testing.T) {
	te := newTestEnv()
	te.Reader.LastBlock = 49999

	oversized := make([]tss_helpers.SignedCommitment, 0, params.MAX_TSS_COMMITMENTS_PER_TX+1)
	for i := 0; i <= params.MAX_TSS_COMMITMENTS_PER_TX; i++ {
		oversized = append(oversized, tss_helpers.SignedCommitment{
			BaseCommitment: tss_helpers.BaseCommitment{
				Type:        "reshare",
				SessionId:   "reshare-1-0-floodkey",
				KeyId:       "floodkey",
				Commitment:  "AAAA",
				Epoch:       1,
				BlockHeight: 49999,
			},
			Signature: "deadbeef",
			BitSet:    "AA",
		})
	}
	jsonBytes, err := json.Marshal(oversized)
	if err != nil {
		t.Fatal(err)
	}

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"testwitness"},
		Id:            "vsc.tss_commitment",
		Json:          string(jsonBytes),
	})
	te.Reader.CreateBlock()
	time.Sleep(200 * time.Millisecond)

	if len(te.TssCommitments.Commitments) > 0 {
		t.Fatalf("an oversized bundle stored %d commitments", len(te.TssCommitments.Commitments))
	}
}

// The cap must sit far above anything a real ceremony produces, because an
// oversized bundle is rejected WHOLESALE rather than truncated. A cap set too
// tight would silently drop honest commitments and stall a key ceremony, which is
// a worse failure than the one being prevented.
func TestM1_CapIsFarAboveAnyLegitimateBundle(t *testing.T) {
	// A legitimate bundle carries at most one commitment per committee member per
	// ceremony. Mainnet runs 19 witnesses; even a committee an order of magnitude
	// larger must fit with room to spare.
	const generousCommittee = 100
	if params.MAX_TSS_COMMITMENTS_PER_TX < generousCommittee*2 {
		t.Errorf("MAX_TSS_COMMITMENTS_PER_TX = %d leaves no margin above a %d-member "+
			"committee; an over-tight cap silently drops honest commitments and stalls "+
			"a ceremony", params.MAX_TSS_COMMITMENTS_PER_TX, generousCommittee)
	}
}
