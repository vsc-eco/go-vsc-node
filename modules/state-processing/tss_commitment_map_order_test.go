package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	stateEngine "vsc-node/modules/state-processing"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/multiformats/go-multicodec"
	ethBls "github.com/protolambda/bls12-381-util"
)

// A vsc.tss_commitment in the legacy map format was read in Go map order,
// which is random per node. Two valid keygen commitments for one key that is
// still "created" (two keygens can finish before either lands) in one such
// transaction then activated a different pubkey on different nodes. Entries
// are now taken in key order, so every node activates the same one.
func TestTssCommitmentMapFormatIsReadInKeyOrder(t *testing.T) {
	const (
		pa = "037c8b7caf4cbfd635e2364d7761a69ff63686e9d521608ddf4594d2e21c135a04"
		pb = "03f568536863817c2a268f7b4e7d3631595e2ed08d721fdd9dc6042154f15f1c73"
	)
	ms := mapOrderMembers(t, 4)
	ca := mapOrderKeygen(t, ms, "keygen-500-0-vsc1k-primary", pa, 990, 5)
	cb := mapOrderKeygen(t, ms, "keygen-540-0-vsc1k-primary", pb, 995, 5)
	payload, _ := json.Marshal(map[string]tss_helpers.SignedCommitment{"a": ca, "b": cb})

	got := map[string]int{}
	for run := 0; run < 30; run++ {
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
		te.Reader.LastBlock = 999
		te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"testwitness"}, Id: "vsc.tss_commitment", Json: string(payload)})
		te.Reader.CreateBlock()
		time.Sleep(50 * time.Millisecond)
		key := te.TssKeys.Keys["vsc1k-primary"]
		if key.Status != tss_db.TssKeyActive {
			t.Fatalf("run %d: the key was not activated: %+v", run, key)
		}
		got[key.PublicKey]++
	}
	if len(got) != 1 || got[pa] != 30 {
		t.Fatalf("activated pubkeys over 30 runs: %v; want the entry under key \"a\" every time", got)
	}
}

type mapOrderMember struct {
	did  dids.BlsDID
	priv *dids.BlsPrivKey
}

func mapOrderMembers(t *testing.T, n int) []mapOrderMember {
	t.Helper()
	out := make([]mapOrderMember, n)
	for i := range out {
		var seed [32]byte
		copy(seed[:], []byte{byte(i + 1), 'm', 'o'})
		pk := dids.BlsPrivKey{}
		pk.Deserialize(&seed)
		pub, err := ethBls.SkToPk(&pk)
		if err != nil {
			t.Fatal(err)
		}
		did, err := dids.NewBlsDID(pub)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = mapOrderMember{did: did, priv: &pk}
	}
	return out
}

// mapOrderKeygen is a keygen commitment signed by every member.
func mapOrderKeygen(t *testing.T, ms []mapOrderMember, session, pub string, height, epoch uint64) tss_helpers.SignedCommitment {
	t.Helper()
	base := tss_helpers.BaseCommitment{Type: "keygen", SessionId: session, KeyId: "vsc1k-primary",
		Commitment: "Dw", PublicKey: &pub, BlockHeight: height, Epoch: epoch}
	data, _ := common.EncodeDagCbor(base)
	c, err := common.HashBytes(data, multicodec.DagCbor)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]dids.BlsDID, len(ms))
	for i, m := range ms {
		keys[i] = m.did
	}
	circ, err := dids.NewBlsCircuitGenerator(keys).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		prov, _ := dids.NewBlsProvider(m.priv)
		sig, err := prov.Sign(c)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := circ.AddAndVerify(m.did, sig); !ok || err != nil {
			t.Fatalf("member signature: %v %v", ok, err)
		}
	}
	final, err := circ.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	ser, err := final.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tss_helpers.SignedCommitment{BaseCommitment: base, Signature: ser.Signature, BitSet: ser.BitVector}
}
