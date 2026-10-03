package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
)

// TestPoaGatewayRoundingDecidesWhichTwoThirdsCanSign (item 10, gateway under flat
// weight): the gateway splits 10000 weight units over the committee keys, so with 9
// flat seats one key gets 1112 and eight get 1111, at threshold 6667. Six seats are
// exactly 2/3, yet they can sign only if the 1112 key is among them (1112+5*1111 =
// 6667); six without it reach 6666 and cannot. Which 2/3 of seats can move the
// gateway funds or rotate its keys therefore depends on an alphabetical tiebreak.
// On mainnet (18 seats: 10 keys at 556, 8 at 555) 12 seats pass only with at least
// 7 of the 556 keys.
//
// Phase G (control): stop 3 seats that do NOT hold the 1112 key -> a rotation still
// lands. Phase R: stop 3 seats including it -> no rotation lands.
// Expected RED while the gap exists: R blocks rotation while G does not.
func TestPoaGatewayRoundingDecidesWhichTwoThirdsCanSign(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	requireDocker(t)
	cfg := regressionConfig()
	if pj := os.Getenv("DEVNET_PROJECT"); pj != "" {
		cfg.ProjectName = pj // so the runner can tear it down by project
	}
	cfg.Nodes = 9 // flat 10000/9 = 1111 r 1, and >= 8 gateway keys so rotation runs
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ElectionInterval = 20
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	cp.BondInclusionActivationHeight = 0
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(80*time.Minute))
	t.Cleanup(cancel)
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	hc := hivego.NewHiveRpc([]string{d.DroneEndpoint()})

	account := func() hivego.AccountData {
		for {
			accs, err := hc.GetAccount([]string{"vsc.gateway"})
			if err == nil && len(accs) == 1 {
				return accs[0]
			}
			select {
			case <-ctx.Done():
				t.Fatalf("ctx done reading vsc.gateway: %v", ctx.Err())
			case <-time.After(3 * time.Second):
			}
		}
	}
	// lastRotation identifies the latest rotation: last_block_rotation from the
	// metadata when the RPC returns it, else the account's last_account_update
	// time (every rotation is an account_update; on this devnet the metadata
	// reads back empty through hivego).
	lastRotation := func(a hivego.AccountData) uint64 {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(a.JSONMetadata), &m)
		if v, ok := m["last_block_rotation"].(float64); ok && v > 0 {
			return uint64(v)
		}
		return uint64(time.Time(a.LastAccountUpdate).Unix())
	}
	// waitRotation waits for last_block_rotation to move past after.
	waitRotation := func(after uint64, timeout time.Duration) (hivego.AccountData, bool) {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			a := account()
			if lastRotation(a) > after {
				return a, true
			}
			time.Sleep(5 * time.Second)
		}
		return account(), false
	}
	// gateway key -> account, from magi-1's witness records.
	keyOwner := func() map[string]string {
		out := map[string]string{}
		client, err := d.mongoClient(ctx)
		if err != nil {
			t.Fatalf("mongo: %v", err)
		}
		defer client.Disconnect(ctx)
		cur, err := client.Database(d.nodeDbName(1)).Collection("witnesses").Find(ctx, bson.M{})
		if err != nil {
			t.Fatalf("witnesses: %v", err)
		}
		var rows []struct {
			Account    string `bson:"account"`
			GatewayKey string `bson:"gateway_key"`
		}
		_ = cur.All(ctx, &rows)
		for _, r := range rows {
			if r.GatewayKey != "" {
				out[r.GatewayKey] = r.Account
			}
		}
		return out
	}
	describe := func(a hivego.AccountData) (heavy string, summary string) {
		owners := keyOwner()
		var parts []string
		for _, ka := range a.Active.KeyAuths {
			if len(ka) != 2 {
				continue
			}
			key, _ := ka[0].(string)
			w, _ := ka[1].(float64)
			who := owners[key]
			if who == "" {
				who = key[:12] + "…"
			}
			parts = append(parts, fmt.Sprintf("%s=%d", who, int(w)))
			if int(w) == 1112 {
				heavy = who
			}
		}
		var ownerAA []string
		for _, aa := range a.Owner.AccountAuths {
			ownerAA = append(ownerAA, fmt.Sprint(aa))
		}
		var activeAA []string
		for _, aa := range a.Active.AccountAuths {
			activeAA = append(activeAA, fmt.Sprint(aa))
		}
		return heavy, fmt.Sprintf("active threshold=%d keys=[%s] active account_auths=%v owner account_auths=%v owner threshold=%d",
			a.Active.WeightThreshold, strings.Join(parts, " "), activeAA, ownerAA, a.Owner.WeightThreshold)
	}
	nodeOf := func(acct string) int {
		var n int
		fmt.Sscanf(strings.TrimPrefix(strings.TrimPrefix(acct, "hive:"), cfg.WitnessPrefix), "%d", &n)
		return n
	}

	// P0: a POA rotation with all 9 keys.
	var a hivego.AccountData
	var heavy, summary string
	flat := false
	for tries := 0; ; tries++ {
		var ok bool
		a, ok = waitRotation(lastRotation(account()), 12*time.Minute)
		heavy, summary = describe(a)
		t.Logf("P0. rotation at %d: %s", lastRotation(a), summary)
		// 0.9.0 (GatewayEqualWeightsActive): every key has weight 1 and there is
		// no heavy key; stand magi-1 in for it, so R stops the same nodes.
		if ok && len(a.Active.KeyAuths) == cfg.Nodes && heavy == "" && a.Active.WeightThreshold == 6 {
			flat = true
			heavy = fmt.Sprintf("%s1", cfg.WitnessPrefix)
		}
		if ok && len(a.Active.KeyAuths) == cfg.Nodes && heavy != "" {
			break
		}
		if tries == 6 {
			t.Fatalf("PRECONDITION FAILED: no rotation with %d flat keys (last: %s)", cfg.Nodes, summary)
		}
	}
	h := nodeOf(heavy)
	if h == 0 {
		t.Fatalf("PRECONDITION FAILED: cannot map heavy key owner %q to a node", heavy)
	}
	t.Logf("P0. heavy key (1112, or magi-1 when every key is weight 1: flat=%v) held by %s = magi-%d", flat, heavy, h)

	others := []int{}
	for n := cfg.Nodes; n >= 1 && len(others) < 3; n-- {
		if n != h {
			others = append(others, n)
		}
	}
	stop := func(ns []int) {
		for _, n := range ns {
			if err := d.StopNode(ctx, n); err != nil {
				t.Fatalf("stop magi-%d: %v", n, err)
			}
		}
	}
	start := func(ns []int) {
		for _, n := range ns {
			if err := d.StartNode(ctx, n); err != nil {
				t.Logf("start magi-%d: %v", n, err)
			}
		}
	}

	// G: three stopped, heavy key still signing.
	stop(others)
	before := lastRotation(account())
	aG, okG := waitRotation(before, 8*time.Minute)
	_, sG := describe(aG)
	t.Logf("G. stopped %v (heavy magi-%d up): rotation landed=%v (%d -> %d) %s", others, h, okG, before, lastRotation(aG), sG)
	start(others)
	aBack, _ := waitRotation(lastRotation(account()), 10*time.Minute)
	t.Logf("G'. all back: rotation at %d", lastRotation(aBack))

	// R: three stopped including the heavy key.
	red := []int{h, others[0], others[1]}
	stop(red)
	before = lastRotation(account())
	aR, okR := waitRotation(before, 8*time.Minute)
	t.Logf("R. stopped %v (incl. heavy magi-%d): rotation landed=%v (%d -> %d)", red, h, okR, before, lastRotation(aR))
	start(red)

	switch {
	case flat && okG && okR:
		t.Logf("GREEN: every key weight 1, threshold 6 of 9; both 6-key sets rotated (G=%v R=%v)", okG, okR)
	case flat && okG && !okR:
		t.Errorf("weight-1 keys at threshold 6, yet the 6 seats without magi-%d did not rotate", h)
	case okG && !okR:
		t.Errorf("GATEWAY-ROUNDING: 6 of 9 seats rotate the gateway only if the 1112-weight key (%s) is among them: without it they hold 6666 < 6667", heavy)
	case !okG:
		t.Logf("INCONCLUSIVE: no rotation even with the heavy key up (3 of 9 stopped also halts L2 blocks; rotation may depend on them)")
	default:
		t.Logf("no rounding effect observed (G=%v R=%v)", okG, okR)
	}
}
