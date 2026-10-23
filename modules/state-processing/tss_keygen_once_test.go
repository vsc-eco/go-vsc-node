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

// TSS-KG-1: a key's keygen re-runs every rotation until a keygen commitment
// is indexed, so several keygens can succeed, each with its own pubkey,
// before the first commitment lands. Every later commitment's row was stored:
// devnet W3's keygen-500 activated the key (pubkey P500, epoch 5), then
// keygen-540's commitment (epoch 6) bumped the key to epoch 6, whose shares
// are keygen-540's, so the network signed with a key the chain does not hold.
// From 0.13.0 a keygen for an already activated key is dropped before it is
// stored.

type kgMember struct {
	did  dids.BlsDID
	priv *dids.BlsPrivKey
}

func kgMembers(t *testing.T, n int) []kgMember {
	t.Helper()
	out := make([]kgMember, n)
	for i := range out {
		var seed [32]byte
		copy(seed[:], []byte{byte(i + 1), 'k', 'g'})
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
		out[i] = kgMember{did: did, priv: &pk}
	}
	return out
}

// kgCommitment is a keygen commitment signed by every member, as the leader
// lands it.
func kgCommitment(t *testing.T, ms []kgMember, session, pub string, height, epoch uint64) tss_helpers.SignedCommitment {
	t.Helper()
	base := tss_helpers.BaseCommitment{Type: "keygen", SessionId: session, KeyId: "vsc1bridge-primary",
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

func kgLand(te *testEnv, c tss_helpers.SignedCommitment) {
	b, _ := json.Marshal([]tss_helpers.SignedCommitment{c})
	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"testwitness"}, Id: "vsc.tss_commitment", Json: string(b)})
	te.Reader.CreateBlock()
	time.Sleep(200 * time.Millisecond)
}

func TestTSSKG1_LateKeygenDoesNotTakeOverAnActiveKey(t *testing.T) {
	const (
		p500 = "037c8b7caf4cbfd635e2364d7761a69ff63686e9d521608ddf4594d2e21c135a04"
		p540 = "03f568536863817c2a268f7b4e7d3631595e2ed08d721fdd9dc6042154f15f1c73"
	)
	for _, tc := range []struct {
		consensus   uint64
		findKeyErrs int // transient key-store errors before the late keygen's status read
		wantRows    int
		wantNewest  string // pubkey of the newest keygen row, the one a reshare reads as the old commitment
	}{
		// Before the line the late keygen's row is stored and becomes the newest:
		// the next reshare loads keygen-540's shares for a key whose pubkey is
		// keygen-500's. (With rotation-v2 in force, as here, the key's epoch is not
		// bumped; without it, as on the devnet, it is, and the split is immediate.)
		{12, 0, 2, p540},
		{13, 0, 1, p500},
		// A Mongo blip on one node must not make it store the row its peers
		// drop: the read blocks and retries, then decides as everyone else.
		{13, 1, 1, p500},
	} {
		te := newTestEnv()
		ms := kgMembers(t, 4)
		members := make([]elections.ElectionMember, len(ms))
		for i, m := range ms {
			members[i] = elections.ElectionMember{Account: "w" + string(rune('1'+i)), Key: m.did.String()}
		}
		el := elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 5},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: []uint64{1, 1, 1, 1}, ProtocolVersion: tc.consensus},
		}
		te.ElectionDb.ElectionsByHeight[1] = el
		te.ElectionDb.Elections[5] = &el
		te.TssKeys.Keys["vsc1bridge-primary"] = tss_db.TssKey{Id: "vsc1bridge-primary", Status: tss_db.TssKeyCreated, Algo: tss_db.EcdsaType, Epochs: 365}
		te.Reader.LastBlock = 999

		kgLand(te, kgCommitment(t, ms, "keygen-500-0-vsc1bridge-primary", p500, 990, 5))
		key := te.TssKeys.Keys["vsc1bridge-primary"]
		if key.Status != tss_db.TssKeyActive || key.PublicKey != p500 || key.Epoch != 5 {
			t.Fatalf("0.%d: the first keygen did not activate the key: %+v", tc.consensus, key)
		}
		te.TssKeys.FindKeyErrs = tc.findKeyErrs
		kgLand(te, kgCommitment(t, ms, "keygen-540-0-vsc1bridge-primary", p540, 995, 6))
		if te.TssKeys.FindKeyErrs != 0 {
			t.Fatalf("0.%d: the injected key-store error was never read", tc.consensus)
		}

		key = te.TssKeys.Keys["vsc1bridge-primary"]
		rows, newest := 0, tss_db.TssCommitment{}
		for _, c := range te.TssCommitments.Commitments {
			if c.KeyId == "vsc1bridge-primary" && c.Type == "keygen" {
				rows++
				if c.BlockHeight > newest.BlockHeight { // production reads block_height desc
					newest = c
				}
			}
		}
		if key.PublicKey != p500 || key.Epoch != 5 || rows != tc.wantRows {
			t.Fatalf("0.%d after the late keygen: pubkey %s epoch %d, %d keygen rows; want %s, epoch 5, %d rows",
				tc.consensus, key.PublicKey, key.Epoch, rows, p500, tc.wantRows)
		}
		if newest.PublicKey == nil || *newest.PublicKey != tc.wantNewest {
			t.Fatalf("0.%d: the newest keygen row (a reshare's old commitment) is not %s's: %+v", tc.consensus, tc.wantNewest, newest)
		}
	}
}
