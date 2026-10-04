package devnet

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	transactionpool "vsc-node/modules/transaction-pool"

	blsu "github.com/protolambda/bls12-381-util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestVscDidQuorumIsTwoThirds (VSC-1): a did:vsc transaction (the oracle relay's
// identity) is admitted on the committee's aggregate BLS signature. Every other
// quorum needs ceil(2/3) of the election weight; this one accepted floor(2/3), so
// under equal POA seat weights one seat fewer could sign it (3 of 5 here).
//
// Once the POA floor (0.9) gives every seat weight 1, the test signs one did:vsc
// call with exactly floor(2/3) of the seats and one with ceil(2/3), using the
// devnet's deterministic BLS keys, and submits both to magi-1's mempool.
// Expected: the ceil(2/3) transaction is admitted on every build (positive
// control); the floor(2/3) one is refused with the fix and admitted without it.
func TestVscDidQuorumIsTwoThirds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	const floorEpoch = 2
	cfg := tssTestConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 1
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = 9
	cp.ConsensusVersionFloorEpoch = floorEpoch
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["DEVNET_DETERMINISTIC_BLS"] = "1"
	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	// An election with equal seat weights, where floor(2W/3) < ceil(2W/3).
	// Decoded into explicit fields: ElectionResult nests its fields in embedded
	// structs without inline tags, so a direct decode reads nothing.
	var row struct {
		Epoch   uint64 `bson:"epoch"`
		Members []struct {
			Key     string `bson:"key"`
			Account string `bson:"account"`
		} `bson:"members"`
		Weights []uint64 `bson:"weights"`
	}
	var elec elections.ElectionResult
	for ep := uint64(floorEpoch + 1); ; ep++ {
		if err := d.waitForElectionEpoch(ctx, 1, ep, 15*time.Minute); err != nil {
			t.Fatalf("PRECONDITION FAILED: no epoch %d: %v", ep, err)
		}
		client, err := d.mongoClient(ctx)
		if err != nil {
			t.Fatalf("mongo: %v", err)
		}
		err = client.Database(d.nodeDbName(1)).Collection("elections").FindOne(ctx, bson.M{},
			options.FindOne().SetSort(bson.M{"epoch": -1})).Decode(&row)
		client.Disconnect(ctx)
		if err != nil {
			t.Fatalf("reading the latest election: %v", err)
		}
		elec = elections.ElectionResult{}
		elec.Epoch = row.Epoch
		elec.Weights = row.Weights
		elec.Members = nil
		for _, m := range row.Members {
			elec.Members = append(elec.Members, elections.ElectionMember{Key: m.Key, Account: m.Account})
		}
		equal := len(elec.Weights) > 0
		for _, w := range elec.Weights {
			equal = equal && w == elec.Weights[0]
		}
		var total uint64
		for _, w := range elec.Weights {
			total += w
		}
		t.Logf("epoch %d: %d members, weights %v", elec.Epoch, len(elec.Members), elec.Weights)
		if equal && elec.Weights[0] == 1 && total%3 != 0 {
			break
		}
		if ep > floorEpoch+6 {
			t.Fatalf("PRECONDITION FAILED: no election with equal seat weight 1 and a total not divisible by 3 by epoch %d", ep)
		}
	}
	n := len(elec.Members)
	floorK := (2 * n) / 3
	ceilK := n - n/3
	t.Logf("committee of %d equal seats: floor(2/3) = %d, ceil(2/3) = %d", n, floorK, ceilK)

	// Each member's key from the deterministic devnet seed; it must match the
	// key the election carries, or the test proves nothing.
	privs := make([]*dids.BlsPrivKey, n)
	for i, m := range elec.Members {
		seed := sha256.Sum256([]byte("devnet-bls-" + strings.TrimPrefix(m.Account, "hive:")))
		priv := &dids.BlsPrivKey{}
		if err := priv.Deserialize(&seed); err != nil {
			t.Fatalf("deriving %s's key: %v", m.Account, err)
		}
		pub, err := blsu.SkToPk(priv)
		if err != nil {
			t.Fatal(err)
		}
		did, err := dids.NewBlsDID(pub)
		if err != nil {
			t.Fatal(err)
		}
		if string(did) != m.Key {
			t.Fatalf("PRECONDITION FAILED: derived key for %s does not match the election (%s vs %s)", m.Account, did, m.Key)
		}
		privs[i] = priv
	}

	submit := func(kid string, signers int) error {
		op := transactionpool.VscContractCall{
			ContractId: "vsc1VSC1QuorumProbeNoSuchContract",
			Action:     "probe",
			Payload:    "{}",
			Intents:    []contracts.Intent{},
			RcLimit:    1000,
			Caller:     kid,
			NetId:      d.netId(),
		}
		vOp, err := op.SerializeVSC()
		if err != nil {
			return fmt.Errorf("serialize op: %w", err)
		}
		tx := transactionpool.VSCTransaction{Ops: []transactionpool.VSCTransactionOp{vOp}, Nonce: 0, NetId: d.netId(), RcLimit: op.RcLimit}
		sTx, err := tx.Serialize()
		if err != nil {
			return fmt.Errorf("serialize tx: %w", err)
		}
		blk, err := tx.ToSignableBlock()
		if err != nil {
			return fmt.Errorf("signable block: %w", err)
		}
		circuit, err := dids.NewBlsCircuitGenerator(elec.MemberKeys()).Generate(blk.Cid())
		if err != nil {
			return fmt.Errorf("circuit: %w", err)
		}
		for i := 0; i < signers; i++ {
			prov, err := dids.NewBlsProvider(privs[i])
			if err != nil {
				return err
			}
			sig, err := prov.Sign(blk.Cid())
			if err != nil {
				return err
			}
			if ok, err := circuit.AddAndVerify(dids.BlsDID(elec.Members[i].Key), sig); err != nil || !ok {
				return fmt.Errorf("adding %s's signature: ok=%v err=%v", elec.Members[i].Account, ok, err)
			}
		}
		final, err := circuit.Finalize()
		if err != nil {
			return fmt.Errorf("finalize: %w", err)
		}
		ser, err := final.Serialize()
		if err != nil {
			return fmt.Errorf("serialize circuit: %w", err)
		}
		sigBytes, err := common.EncodeDagCbor(transactionpool.SignaturePackage{
			Type: "vsc-sig",
			Sigs: []common.Sig{{Algo: "BLS12-381", Kid: kid, Sig: ser.Signature, Bv: ser.BitVector}},
		})
		if err != nil {
			return err
		}
		const q = `query($tx:String!,$sig:String!){submitTransactionV1(tx:$tx,sig:$sig){id}}`
		var out struct {
			SubmitTransactionV1 *struct {
				Id *string `json:"id"`
			} `json:"submitTransactionV1"`
		}
		return d.gqlQuery(ctx, 1, q, map[string]any{
			"tx":  base64.StdEncoding.EncodeToString(sTx.Tx),
			"sig": base64.StdEncoding.EncodeToString(sigBytes),
		}, &out)
	}

	// Positive control: ceil(2/3) of the seats is admitted on every build.
	if err := submit("did:vsc:oracle:vsc1ceil", ceilK); err != nil {
		t.Fatalf("PRECONDITION FAILED: a did:vsc tx signed by %d of %d seats (ceil 2/3) was refused: %v", ceilK, n, err)
	}
	t.Logf("RESULT ceil(2/3) = %d of %d seats: admitted", ceilK, n)

	err := submit("did:vsc:oracle:vsc1floor", floorK)
	if err == nil {
		t.Errorf("RESULT floor(2/3) = %d of %d seats: ADMITTED (VSC-1: one seat fewer than every other 2/3 rule)", floorK, n)
	} else {
		t.Logf("RESULT floor(2/3) = %d of %d seats: refused: %v", floorK, n, err)
	}
}
