package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestGatewayMixedFleet (VR2-13 re-test): during a rolling upgrade, can the
// vsc.gateway multisig still rotate its keys and pay HBD withdrawals out on L1?
// On 09-06 a mixed fleet never got the gateway authority live (no gateway op could
// be broadcast); the cause was traced to elections not converging, since fixed.
//
// GW_OLD_DIR set: mainnet's build (aa112bc1) on magi-6..magi-9, the new build on
// magi-1..magi-5. Unset: an all-new fleet (the control). 9 nodes because
// keyRotation skips below MIN_GATEWAY_KEYS (8): with 5 nodes no rotation ever ran,
// and one spare keeps it rotating if a member is briefly not eligible.
// The floor is pinned to 0.3, mainnet's line during the rollout. Checks: an HBD
// withdrawal lands on L1; two key rotations land (the second is signed under the
// rotated authority); neither build alone holds that authority's threshold, so
// every later signature needed both; a second withdrawal lands under it.
func TestGatewayMixedFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 9
	cfg.SkipFunding = false
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = 3
	cp.ConsensusVersionFloorEpoch = 1
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"
	if old := os.Getenv("GW_OLD_DIR"); old != "" {
		cfg.OldCodeSourceDir = old
		cfg.OldCodeNodes = []int{6, 7, 8, 9}
		cfg.OldCodeSysconfig = true
		t.Logf("mixed fleet: %s on magi-6..magi-9", old)
	} else {
		t.Logf("all-new fleet (control)")
	}
	d, ctx := startDevnetNoKey(t, cfg, 90*time.Minute)

	if err := d.waitForElectionEpoch(ctx, 1, 2, 20*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: no epoch 2: %v", err)
	}
	if v := vfActiveConsensus(d, ctx); v != 3 {
		t.Fatalf("PRECONDITION FAILED: election carries consensus 0.%d, want 0.3 (mainnet's line)", v)
	}
	payer, payee := 1, 2
	payeeAcct := d.witnessAccount(payee)
	auth0 := gwAuthority(t, d)
	rot0 := gwRotationMark(t, d)
	t.Logf("gateway active authority at epoch 2: %s (rotation mark %d)", auth0, rot0)

	// Fund the payer's L2 HBD.
	if _, err := d.Deposit(ctx, payer, "10.000", "hbd"); err != nil {
		t.Fatalf("PRECONDITION FAILED: deposit: %v", err)
	}
	for end := time.Now().Add(5 * time.Minute); ; time.Sleep(5 * time.Second) {
		if b, err := d.GetAccountBalance(ctx, 1, "hive:"+d.witnessAccount(payer)); err == nil && b.Hbd >= 10_000 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("PRECONDITION FAILED: the HBD deposit was never credited on L2")
		}
	}

	withdraw := func(label, amount string, milli int64) bool {
		before := gwL1Hbd(t, d, payeeAcct)
		tx, err := d.Withdraw(payer, payeeAcct, amount, "hbd", label)
		if err != nil {
			t.Fatalf("%s: withdraw: %v", label, err)
		}
		start := time.Now()
		for end := time.Now().Add(15 * time.Minute); time.Now().Before(end); time.Sleep(10 * time.Second) {
			if now := gwL1Hbd(t, d, payeeAcct); now >= before+milli {
				t.Logf("RESULT %s: %s HBD paid out on L1 to %s after %s (withdraw tx %s)", label, amount, payeeAcct, time.Since(start).Round(time.Second), tx)
				return true
			}
		}
		t.Errorf("RESULT %s: withdrawal of %s HBD never reached %s on L1 within 15 min (withdraw tx %s, L1 HBD still %d)", label, amount, payeeAcct, tx, gwL1Hbd(t, d, payeeAcct))
		return false
	}
	withdraw("GW-1", "3.000", 3000)

	// Key rotation runs every 20 blocks. Wait for two: the second one is signed
	// under the authority the first one installed.
	waitRotation := func(label string, after uint64) uint64 {
		for end := time.Now().Add(15 * time.Minute); time.Now().Before(end); time.Sleep(10 * time.Second) {
			if m := gwRotationMark(t, d); m > after {
				t.Logf("RESULT %s: rotation landed (mark %d > %d), authority %s", label, m, after, gwAuthority(t, d))
				return m
			}
		}
		t.Errorf("RESULT %s: no gateway key rotation landed within 15 min (mark still %d, authority %s)", label, after, gwAuthority(t, d))
		return 0
	}
	rot1 := waitRotation("GW-ROTATE-1", rot0)
	if rot1 != 0 {
		waitRotation("GW-ROTATE-2", rot1)
	}

	// Neither build alone may hold the rotated authority's threshold, or the
	// rotations and GW-2 prove nothing about the two builds signing together.
	threshold, weights := gwActiveWeights(t, d)
	owners := gwKeyOwners(t, d, ctx)
	oldAcct := map[string]bool{}
	for _, n := range cfg.OldCodeNodes {
		oldAcct[d.witnessAccount(n)] = true
	}
	var oldW, newW, unknownW int
	for key, w := range weights {
		switch who := owners[key]; {
		case who == "":
			unknownW += w
		case oldAcct[who]:
			oldW += w
		default:
			newW += w
		}
	}
	t.Logf("RESULT GW-SPLIT: threshold %d, keys %d, old build %d, new build %d, unknown %d", threshold, len(weights), oldW, newW, unknownW)
	if len(cfg.OldCodeNodes) > 0 && (oldW >= threshold || newW+unknownW >= threshold) {
		t.Errorf("RESULT GW-SPLIT: one build alone holds the threshold; the layout does not force both builds to sign")
	}
	withdraw("GW-2", "2.000", 2000)

	// L2 must still agree across the fleet.
	_, compared, div, rerr := vfScanBlockHeaders(d, ctx, cfg.Nodes)
	t.Logf("RESULT L2 headers compared %d, divergence=%q readErr=%q", compared, div, rerr)
	if div != "" {
		t.Errorf("L2 block divergence: %s", div)
	}
}

func gwHiveRPC(t *testing.T, d *Devnet, method string, params any, out any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(d.HiveRPCEndpoint(), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("hive rpc %s: %v", method, err)
	}
	defer resp.Body.Close()
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("hive rpc %s decode: %v", method, err)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		t.Fatalf("hive rpc %s result: %v", method, err)
	}
}

// gwL1Hbd is an account's liquid L1 HBD in thousandths.
func gwL1Hbd(t *testing.T, d *Devnet, account string) int64 {
	var accts []struct {
		HbdBalance string `json:"hbd_balance"`
	}
	gwHiveRPC(t, d, "condenser_api.get_accounts", []any{[]string{account}}, &accts)
	if len(accts) == 0 {
		t.Fatalf("no L1 account %s", account)
	}
	f := strings.Fields(accts[0].HbdBalance)
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", accts[0].HbdBalance, err)
	}
	return int64(v*1000 + 0.5)
}

// gwAuthority is vsc.gateway's active authority as "threshold:key=weight,...".
func gwAuthority(t *testing.T, d *Devnet) string {
	var accts []struct {
		Active struct {
			Threshold int     `json:"weight_threshold"`
			Keys      [][]any `json:"key_auths"`
		} `json:"active"`
	}
	gwHiveRPC(t, d, "condenser_api.get_accounts", []any{[]string{"vsc.gateway"}}, &accts)
	if len(accts) == 0 {
		return "no vsc.gateway account"
	}
	keys := make([]string, 0, len(accts[0].Active.Keys))
	for _, k := range accts[0].Active.Keys {
		if len(k) == 2 {
			s := fmt.Sprint(k[0])
			if len(s) > 12 {
				s = s[len(s)-8:]
			}
			keys = append(keys, fmt.Sprintf("%s=%v", s, k[1]))
		}
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d:%s", accts[0].Active.Threshold, strings.Join(keys, ","))
}

// gwRotationMark is the unix time of vsc.gateway's last account_update; every
// key rotation is one.
func gwRotationMark(t *testing.T, d *Devnet) uint64 {
	var accts []struct {
		LastAccountUpdate string `json:"last_account_update"`
	}
	gwHiveRPC(t, d, "condenser_api.get_accounts", []any{[]string{"vsc.gateway"}}, &accts)
	if len(accts) == 0 {
		return 0
	}
	ts, err := time.Parse("2006-01-02T15:04:05", accts[0].LastAccountUpdate)
	if err != nil {
		return 0
	}
	return uint64(ts.Unix())
}

// gwActiveWeights is vsc.gateway's active threshold and per-key weights.
func gwActiveWeights(t *testing.T, d *Devnet) (int, map[string]int) {
	var accts []struct {
		Active struct {
			Threshold int     `json:"weight_threshold"`
			Keys      [][]any `json:"key_auths"`
		} `json:"active"`
	}
	gwHiveRPC(t, d, "condenser_api.get_accounts", []any{[]string{"vsc.gateway"}}, &accts)
	if len(accts) == 0 {
		t.Fatalf("no vsc.gateway account")
	}
	out := map[string]int{}
	for _, k := range accts[0].Active.Keys {
		if len(k) == 2 {
			w, _ := k[1].(float64)
			out[fmt.Sprint(k[0])] = int(w)
		}
	}
	return accts[0].Active.Threshold, out
}

// gwKeyOwners maps each announced gateway key to its witness, from magi-1's records.
func gwKeyOwners(t *testing.T, d *Devnet, ctx context.Context) map[string]string {
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
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("witnesses: %v", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		if r.GatewayKey != "" {
			out[r.GatewayKey] = r.Account
		}
	}
	return out
}
