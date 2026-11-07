package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	stateEngine "vsc-node/modules/state-processing"
	tss_helpers "vsc-node/modules/tss/helpers"
)

// C-R6: the vsc.tss_commitment handler read the key with its errors dropped,
// so a Mongo blip on one node read a "created" key as the zero key: that node
// did not activate it (it bumped the epoch of a key with no public key) while
// its peers did. The read now blocks and retries.
func TestTssCommitmentKeyReadBlocksOnATransientError(t *testing.T) {
	const pa = "037c8b7caf4cbfd635e2364d7761a69ff63686e9d521608ddf4594d2e21c135a04"
	ms := mapOrderMembers(t, 4)
	c := mapOrderKeygen(t, ms, "keygen-500-0-vsc1k-primary", pa, 990, 5)
	payload, _ := json.Marshal([]tss_helpers.SignedCommitment{c})

	te := newTestEnv()
	members := make([]elections.ElectionMember, len(ms))
	for i, m := range ms {
		members[i] = elections.ElectionMember{Account: "w" + string(rune('1'+i)), Key: m.did.String()}
	}
	el := elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 5},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: []uint64{1, 1, 1, 1}},
	}
	te.ElectionDb.ElectionsByHeight[1] = el
	te.ElectionDb.Elections[5] = &el
	te.TssKeys.Keys["vsc1k-primary"] = tss_db.TssKey{Id: "vsc1k-primary", Status: tss_db.TssKeyCreated, Algo: tss_db.EcdsaType, Epochs: 365}
	te.TssKeys.FindKeyErrs = 1 // the handler's key read fails once on this node
	te.Reader.LastBlock = 999
	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"testwitness"}, Id: "vsc.tss_commitment", Json: string(payload)})
	te.Reader.CreateBlock()
	time.Sleep(500 * time.Millisecond) // the blocking retry waits 100ms before its second read

	key := te.TssKeys.Keys["vsc1k-primary"]
	if key.Status != tss_db.TssKeyActive || key.PublicKey != pa {
		t.Fatalf("after a transient key read error: %+v; want the key activated with %s, as on every other node", key, pa)
	}
}
