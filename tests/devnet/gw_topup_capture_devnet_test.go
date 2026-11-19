package devnet

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"

	libhive "vsc-node/lib/hive"
)

// TestGatewayTopUpCapture: the worst case of the POA starvation top-up (owner
// 10-31: "try to create failure states where the worst could happen").
// election-proposer.go:1178-1230: when fewer than MinMembers (8) SEATED
// candidates are eligible, UNSEATED bonded candidates fill the committee up to 8
// (previous members first, then by bond maturity). Weights are equal from 0.9,
// and vsc.gateway follows the committee (multisig.go keyRotation), threshold
// ceil(2N/3) = 6 of 8. So with 2 seated left, 6 unseated members hold the
// gateway threshold on their own.
//
// Here the seats of magi-3..10 are removed from every node's registry (the
// state a seat collapse leaves: the same edit on every node, deterministic),
// so magi-3..10 are unseated incumbents. A real capture would be bonded
// strangers after the incumbents are gone; the gateway arithmetic is the same.
//
//	C0  10 seated members, the gateway on their 10 keys.
//	C1  seats of magi-3..10 removed: the next election tops up to 8 with 6 unseated.
//	C2  the gateway rotates to those 8 keys.
//	C3  the 6 unseated sign a 1.000 TESTS transfer OUT of vsc.gateway on their
//	    own, outside the protocol (their devnet gateway keys are derivable,
//	    DEVNET_DETERMINISTIC_BLS). Expect: Hive accepts it.
//	C4  control: the 2 seated alone cannot.
//
//	go test -v -run TestGatewayTopUpCapture -timeout 120m ./tests/devnet/
func TestGatewayTopUpCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 10
	cfg.SkipFunding = false
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 12, 1
	cp.MinMembers = 8 // mainnet
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["DEVNET_DETERMINISTIC_BLS"] = "1"
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"
	d, ctx := startDevnetNoKey(t, cfg, 100*time.Minute)

	if err := d.waitForElectionEpoch(ctx, 1, 2, 20*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: no epoch 2: %v", err)
	}
	keyCount := func() int { _, w := gwActiveWeights(t, d); return len(w) }
	for end := devnetNow().Add(20 * time.Minute); keyCount() < 10; devnetSleep(15 * time.Second) {
		if devnetNow().After(end) {
			t.Fatalf("PRECONDITION FAILED: the gateway never got the 10 committee keys (authority %s)", gwAuthority(t, d))
		}
	}
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	seatCount := func(node int) int64 {
		n, _ := client.Database(d.nodeDbName(node)).Collection("poa_seats").CountDocuments(ctx, bson.M{})
		return n
	}
	t.Logf("RESULT GW-C0: line 0.%d, seats on magi-1: %d, authority %s", vfActiveConsensus(d, ctx), seatCount(1), gwAuthority(t, d))
	if seatCount(1) < 10 {
		t.Fatalf("PRECONDITION FAILED: expected the 10 founders seated, magi-1 has %d seat rows", seatCount(1))
	}

	// C1: the seat collapse.
	var gone []string
	for n := 3; n <= 10; n++ {
		a := d.witnessAccount(n)
		gone = append(gone, a, "hive:"+a)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if _, err := client.Database(d.nodeDbName(n)).Collection("poa_seats").DeleteMany(ctx, bson.M{"account": bson.M{"$in": gone}}); err != nil {
			t.Fatalf("removing seats on magi-%d: %v", n, err)
		}
	}
	e0 := custodyCurrentEpoch(d, ctx, 1)
	t.Logf("seats of magi-3..10 removed on every node at epoch %d (magi-1 now %d rows)", e0, seatCount(1))
	var members []string
	for end := devnetNow().Add(15 * time.Minute); devnetNow().Before(end); devnetSleep(15 * time.Second) {
		if e := custodyCurrentEpoch(d, ctx, 1); e > e0+1 {
			members, _ = d.GetElectionMembers(ctx, 1, e)
			break
		}
	}
	unseated := map[string]bool{}
	for _, m := range members {
		a := strings.TrimPrefix(m, "hive:")
		if a != d.witnessAccount(1) && a != d.witnessAccount(2) {
			unseated[a] = true
		}
	}
	t.Logf("RESULT GW-C1: election after the collapse: %d members %v, unseated %d", len(members), members, len(unseated))

	// C2: the gateway follows.
	for end := devnetNow().Add(15 * time.Minute); keyCount() != len(members) && devnetNow().Before(end); devnetSleep(15 * time.Second) {
	}
	thr, weights := gwActiveWeights(t, d)
	owners := gwKeyOwners(t, d, ctx)
	unseatedW, seatedW := 0, 0
	var unseatedKeys, seatedKeys []*hivego.KeyPair
	for k, w := range weights {
		who := owners[k]
		kp := devnetGatewayKeypair(t, who)
		if *kp.GetPublicKeyString() != k {
			continue // not derivable here
		}
		if unseated[who] {
			unseatedW += w
			if len(unseatedKeys) < thr { // exactly the threshold: Hive refuses extra signatures
				unseatedKeys = append(unseatedKeys, kp)
			}
		} else {
			seatedW += w
			seatedKeys = append(seatedKeys, kp)
		}
	}
	t.Logf("RESULT GW-C2: authority %s: unseated weight %d, seated weight %d, threshold %d", gwAuthority(t, d), unseatedW, seatedW, thr)

	// C3: the unseated alone move funds out of vsc.gateway.
	thief := d.witnessAccount(10)
	steal := func(label string, keys []*hivego.KeyPair) bool {
		before := gwL1Hive(t, d, thief)
		_, err := gwTransferSigned(d, thief, "1.000", label, keys)
		if err != nil {
			t.Logf("RESULT %s: refused by Hive: %s", label, firstLine(err.Error()))
			return false
		}
		for end := devnetNow().Add(2 * time.Minute); devnetNow().Before(end); devnetSleep(5 * time.Second) {
			if gwL1Hive(t, d, thief) >= before+1000 {
				t.Logf("RESULT %s: 1.000 TESTS left vsc.gateway for %s", label, thief)
				return true
			}
		}
		t.Logf("RESULT %s: broadcast accepted but no balance change within 2 min", label)
		return false
	}
	stolen := steal(fmt.Sprintf("GW-C3 %d unseated keys sign alone", len(unseatedKeys)), unseatedKeys)
	controlled := steal(fmt.Sprintf("GW-C4 control: the %d seated keys alone", len(seatedKeys)), seatedKeys)
	t.Logf("SUMMARY: after a seat collapse to 2, the top-up put %d unseated members on the committee holding %d of %d gateway weight (threshold %d); they moved funds alone=%v; the seated alone=%v",
		len(unseated), unseatedW, unseatedW+seatedW, thr, stolen, controlled)
	if controlled {
		t.Errorf("the 2 seated members alone moved gateway funds: the authority is not what the model expects")
	}
}

// gwTransferSigned broadcasts a transfer out of vsc.gateway signed only by keys.
// memo makes each attempt a distinct transaction (the control first ran with
// the capture's exact op and was refused as a duplicate, proving nothing).
func gwTransferSigned(d *Devnet, to, amount, memo string, keys []*hivego.KeyPair) (string, error) {
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = devnetChainID
	creator := libhive.LiveTransactionCreator{
		TransactionBroadcaster: libhive.TransactionBroadcaster{Client: client},
		TransactionCrafter:     libhive.TransactionCrafter{},
	}
	op := creator.Transfer("vsc.gateway", to, amount, "TESTS", memo)
	tx := creator.MakeTransaction([]hivego.HiveOperation{op})
	if err := creator.PopulateSigningProps(&tx, nil); err != nil {
		return "", err
	}
	for _, kp := range keys {
		sig, err := tx.Sign(*kp, devnetChainID)
		if err != nil {
			return "", err
		}
		tx.AddSig(sig)
	}
	return client.BroadcastRaw(tx)
}

// gwL1Hive is an account's liquid L1 HIVE (TESTS) in thousandths.
func gwL1Hive(t *testing.T, d *Devnet, account string) int64 {
	var accts []struct {
		Balance string `json:"balance"`
	}
	gwHiveRPC(t, d, "condenser_api.get_accounts", []any{[]string{account}}, &accts)
	if len(accts) == 0 {
		return 0
	}
	f := strings.Fields(accts[0].Balance)
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v*1000 + 0.5)
}
