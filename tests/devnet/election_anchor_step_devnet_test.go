package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestElectionAnchorStepsPastDisabledWitnesses reproduces the mainnet epoch-2210
// halt (2026-10-30): a fleet-wide restart right before an election anchor left
// most witnesses announced enabled=false at that anchor (a node announces at
// start, before AutoNAT reports it public, and only re-announces 24 h later),
// so the election built there could not seat MinMembers, and every retry
// rebuilt the same election at the same canonical anchor. Re-announcing does
// not help: the anchor is in the past.
//
// The test disables 3 of 5 witnesses (2 left < MinMembers 3) right before an
// anchor, proves the chain stalls there, re-enables them (the mainnet daily
// re-announce), then requires the stalled epoch to land on every node with all
// 5 members and identical election data, and the next epoch to follow on the
// normal cadence. On a tree without the anchor step it fails at "never
// recovered".
//
// ELECTFIX_OLD_DIR=<unfixed go-vsc-node tree> runs magi-5 on that code (a
// staggered watchtower rollout): magi-5 must accept the stepped election and
// keep processing blocks.
func TestElectionAnchorStepsPastDisabledWitnesses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.LogLevel = "warn,ep=info"
	oldNodes := []int{}
	if old := os.Getenv("ELECTFIX_OLD_DIR"); old != "" {
		cfg.OldCodeSourceDir = old
		cfg.OldCodeNodes = []int{5}
		cfg.OldCodeSysconfig = true
		oldNodes = cfg.OldCodeNodes
		t.Logf("mixed fleet: magi-5 runs the unfixed tree %s", old)
	}
	interval := cfg.SysConfigOverrides.ConsensusParams.ElectionInterval
	cadence := 5 * common.CONSENSUS_SPECS.SlotLength
	canonical := func(prevBh uint64) uint64 {
		target := prevBh + interval + 1
		return ((target + cadence - 1) / cadence) * cadence
	}

	d, ctx := startDevnetNoKey(t, cfg, 75*time.Minute)

	// Healthy baseline: elections land before we touch anything.
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 15*time.Minute); err != nil {
			t.Fatalf("PRECONDITION: magi-%d never reached epoch 2: %v", n, err)
		}
	}

	disabled := []string{"magi.test3", "magi.test4", "magi.test5"}
	original := map[string]string{}
	for _, acct := range disabled {
		original[acct] = announcementMetadata(t, d, ctx, acct)
	}

	// Disable: each witness's own announcement, unchanged except enabled=false.
	for _, acct := range disabled {
		announceWithEnabled(t, d, acct, original[acct], false)
	}
	disabledAt := waitWitnessEnabled(t, d, ctx, disabled, false, 3*time.Minute)
	t.Logf("disabled %v, ingested by magi-1 at height %d", disabled, disabledAt)

	// Find the stalled epoch: the first whose canonical anchor is past the
	// disables, and prove no election lands there for 3 full intervals.
	var stalledEpoch, prevBh, anchor uint64
	findDeadline := devnetNow().Add(15 * time.Minute)
	for {
		if devnetNow().After(findDeadline) {
			t.Fatalf("PRECONDITION: no stalled epoch found after the disables at %d", disabledAt)
		}
		ep, bh := latestElectionEpochHeight(t, d, ctx, 1)
		anchor = canonical(bh)
		if anchor <= disabledAt {
			devnetSleep(10 * time.Second) // this epoch's anchor predates the disables; let it land
			continue
		}
		waitHiveHead(t, d, anchor+3*interval+2*cadence, 15*time.Minute)
		if ep2, _ := latestElectionEpochHeight(t, d, ctx, 1); ep2 != ep {
			continue // landed after all (anchor raced the ingestion); look again
		}
		stalledEpoch, prevBh = ep+1, bh
		break
	}
	t.Logf("STALLED: epoch %d (previous election at %d) did not land at canonical anchor %d; Hive head is past %d",
		stalledEpoch, prevBh, anchor, anchor+3*interval+2*cadence)
	why := 0
	for n := 1; n <= cfg.Nodes; n++ {
		why += countLogLines(d, ctx, n, minMembersLine, stalledEpoch)
	}
	if why == 0 {
		t.Fatalf("PRECONDITION: no node logged 'election minimum member count not met' for epoch %d; the stall is not the mainnet one", stalledEpoch)
	}
	t.Logf("cause: %d 'election minimum member count not met' lines for epoch %d across the fleet", why, stalledEpoch)

	// Re-enable: the mainnet daily re-announce, landing after the anchor.
	for _, acct := range disabled {
		announceWithEnabled(t, d, acct, original[acct], true)
	}
	enabledAt := waitWitnessEnabled(t, d, ctx, disabled, true, 3*time.Minute)
	t.Logf("re-enabled %v at height %d (canonical anchor %d stays in the past)", disabled, enabledAt, anchor)

	// The stalled epoch must now land on every node, identically.
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, stalledEpoch, 12*time.Minute); err != nil {
			t.Fatalf("never recovered: magi-%d has no epoch %d after the witnesses re-enabled at %d (canonical anchor %d): %v",
				n, stalledEpoch, enabledAt, anchor, err)
		}
	}
	base, err := readCanonicalElection(ctx, d, 1, stalledEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.MemberAccounts) != cfg.Nodes {
		t.Errorf("recovered epoch %d seated %v, want all %d witnesses", stalledEpoch, base.MemberAccounts, cfg.Nodes)
	}
	for n := 2; n <= cfg.Nodes; n++ {
		rec, err := readCanonicalElection(ctx, d, n, stalledEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Data != base.Data || !equalStrings(rec.MemberAccounts, base.MemberAccounts) || !equalUint64s(rec.Weights, base.Weights) {
			t.Errorf("magi-%d stored a different epoch %d than magi-1:\n  magi-1: %s %v %v\n  magi-%d: %s %v %v",
				n, stalledEpoch, base.Data, base.MemberAccounts, base.Weights, n, rec.Data, rec.MemberAccounts, rec.Weights)
		}
	}
	t.Logf("RECOVERED: epoch %d landed on all %d nodes with %d members", stalledEpoch, cfg.Nodes, len(base.MemberAccounts))
	stepped := 0
	for n := 1; n <= cfg.Nodes; n++ {
		stepped += countLogLines(d, ctx, n, steppedLine, stalledEpoch)
	}
	if stepped == 0 {
		t.Errorf("epoch %d landed but no node logged a stepped anchor; the recovery did not come from the anchor step", stalledEpoch)
	}
	t.Logf("path: %d 'anchor stepped past the canonical anchor' lines for epoch %d", stepped, stalledEpoch)

	// Normal cadence resumes: the next epoch lands at its canonical anchor.
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, stalledEpoch+1, 8*time.Minute); err != nil {
			t.Fatalf("after recovery magi-%d never reached epoch %d: %v", n, stalledEpoch+1, err)
		}
	}
	t.Logf("epoch %d followed on all nodes", stalledEpoch+1)

	// Every node, fixed or not, keeps following the chain.
	head, err := getHeadBlock(d.HiveRPCEndpoint())
	if err != nil {
		t.Fatalf("hive head: %v", err)
	}
	target := uint64(head) + 2*cadence
	waitHiveHead(t, d, target, 10*time.Minute)
	lagging := []int{}
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, target, 3*time.Minute); err != nil {
			lagging = append(lagging, n)
		}
	}
	if len(lagging) > 0 {
		now, _ := getHeadBlock(d.HiveRPCEndpoint())
		state := []string{}
		for n := 1; n <= cfg.Nodes; n++ {
			bh, _ := d.getLastProcessedBlock(ctx, n)
			state = append(state, fmt.Sprintf("magi-%d=%d", n, bh))
		}
		for _, n := range lagging {
			logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
			t.Logf("magi-%d log tail:\n%s", n, truncateLogs(logs, 60))
		}
		t.Errorf("nodes %v stopped short of Hive %d (head now %d; unfixed: %v): %s",
			lagging, target, now, oldNodes, strings.Join(state, " "))
	} else {
		t.Logf("all %d nodes processed past Hive %d (unfixed: %v)", cfg.Nodes, target, oldNodes)
	}
}

func latestElectionEpochHeight(t *testing.T, d *Devnet, ctx context.Context, node int) (epoch, blockHeight uint64) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	var e struct {
		Epoch       uint64 `bson:"epoch"`
		BlockHeight uint64 `bson:"block_height"`
	}
	if err := client.Database(d.nodeDbName(node)).Collection("elections").FindOne(ctx, bson.M{},
		options.FindOne().SetSort(bson.D{{Key: "epoch", Value: -1}})).Decode(&e); err != nil {
		t.Fatalf("magi-%d latest election: %v", node, err)
	}
	return e.Epoch, e.BlockHeight
}

// announcementMetadata returns the json_metadata of the witness's own latest
// announcement, read from the Hive block its witness row was ingested at (the
// devnet hived does not store account metadata for get_accounts).
func announcementMetadata(t *testing.T, d *Devnet, ctx context.Context, account string) string {
	t.Helper()
	row := latestWitnessRowOrNil(d, ctx, 1, account)
	if row == nil {
		t.Fatalf("no witness row for %s on magi-1", account)
	}
	h := heightOf(row)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"block_api.get_block","params":{"block_num":%d},"id":1}`, h)
	resp, err := http.Post(d.HiveRPCEndpoint(), "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("get_block %d: %v", h, err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Block struct {
				Transactions []struct {
					Operations []struct {
						Type  string `json:"type"`
						Value struct {
							Account      string `json:"account"`
							JsonMetadata string `json:"json_metadata"`
						} `json:"value"`
					} `json:"operations"`
				} `json:"transactions"`
			} `json:"block"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("get_block %d: %v", h, err)
	}
	meta := ""
	for _, tx := range out.Result.Block.Transactions {
		for _, op := range tx.Operations {
			if op.Type == "account_update_operation" && op.Value.Account == account && op.Value.JsonMetadata != "" {
				meta = op.Value.JsonMetadata
			}
		}
	}
	if meta == "" {
		t.Fatalf("block %d (the height of %s's witness row) holds no account_update with json_metadata for it", h, account)
	}
	return meta
}

// announceWithEnabled re-broadcasts the witness's own announcement with only
// vsc_node.witness.enabled changed, as the node itself would.
func announceWithEnabled(t *testing.T, d *Devnet, account, metadata string, enabled bool) {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		t.Fatalf("%s json_metadata: %v", account, err)
	}
	node, ok := meta["vsc_node"].(map[string]any)
	if !ok {
		t.Fatalf("%s json_metadata has no vsc_node: %s", account, metadata)
	}
	node["witness"] = map[string]any{"enabled": enabled}
	node["ts"] = time.Now().UTC().Format(time.RFC3339) // wall: announcement payload timestamp, not a wait
	raw, _ := json.Marshal(meta)
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
	wif := d.cfg.InitminerWIF
	if _, err := broadcastRetryingStale(client, []hivego.HiveOperation{hivego.AccountUpdateOperation{
		Account: account, MemoKey: accountMemoKey(t, d, account), JsonMetadata: string(raw),
	}}, &wif, "electfix announce"); err != nil {
		t.Fatalf("announce %s enabled=%v: %v", account, enabled, err)
	}
}

// waitWitnessEnabled waits until magi-1's latest witness row for every account
// has the wanted enabled flag and returns the highest row height.
func waitWitnessEnabled(t *testing.T, d *Devnet, ctx context.Context, accounts []string, want bool, timeout time.Duration) uint64 {
	t.Helper()
	deadline := devnetNow().Add(timeout)
	for {
		top, done := uint64(0), true
		for _, acct := range accounts {
			row := latestWitnessRowOrNil(d, ctx, 1, acct)
			if row == nil || row["enabled"] != want {
				done = false
				break
			}
			if h := heightOf(row); h > top {
				top = h
			}
		}
		if done {
			return top
		}
		if devnetNow().After(deadline) {
			t.Fatalf("witness rows for %v never showed enabled=%v", accounts, want)
		}
		devnetSleep(3 * time.Second)
	}
}

func waitHiveHead(t *testing.T, d *Devnet, target uint64, timeout time.Duration) {
	t.Helper()
	deadline := devnetNow().Add(timeout)
	for {
		if h, err := getHeadBlock(d.HiveRPCEndpoint()); err == nil && uint64(h) >= target {
			return
		}
		if devnetNow().After(deadline) {
			t.Fatalf("Hive head never reached %d", target)
		}
		devnetSleep(3 * time.Second)
	}
}

var (
	minMembersLine = regexp.MustCompile(`election minimum member count not met.*epoch=(\d+)`)
	steppedLine    = regexp.MustCompile(`anchor stepped past the canonical anchor.*epoch=(\d+)`)
)

func countLogLines(d *Devnet, ctx context.Context, node int, re *regexp.Regexp, epoch uint64) int {
	logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			if e, _ := strconv.ParseUint(m[1], 10, 64); e == epoch {
				n++
			}
		}
	}
	return n
}
