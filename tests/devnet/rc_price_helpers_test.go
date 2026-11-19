package devnet

// Helpers for the storage write price devnet tests (rc_price_*_devnet_test.go):
// consensus 0.15.0 charges 1 RC for each new stored byte instead of 19.
//
// What a call was charged is read from each node's own RC records, not from a
// dry run: the record a node writes for a slot is the payer's frozen RC, so
// (record after) - (record before, decayed to the same height) is what that
// node charged for the calls the payer made in the slot.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rcSystem "vsc-node/modules/rc-system"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// buildRcProbeContract builds the probe contract: the wasm e2e test contract
// (modules/wasm/e2e/go_wasm), which stores a value of a chosen size (setBlob),
// stores it through a nested call (relayBlob), logs (logBlob), returns
// (retBlob), stores many keys (manyKeys), draws HBD (drawHbd) and does nothing
// (noop). A local tinygo is used when there is one, else the image the other
// devnet contracts build with.
func buildRcProbeContract(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // wall: a local compiler run, before any devnet exists
	defer cancel()
	root := findSourceRoot()
	src := filepath.Join(root, "modules", "wasm", "e2e", "go_wasm")
	outDir := filepath.Join(root, "modules", "wasm", "e2e", "tmp")
	out := filepath.Join(outDir, "rcprobe.wasm")
	flags := []string{"build", "-no-debug", "-gc=custom", "-scheduler=none", "-panic=trap", "-target=wasm-unknown"}

	var cmd *exec.Cmd
	if _, err := exec.LookPath("tinygo"); err == nil {
		cmd = exec.CommandContext(ctx, "tinygo", append(flags, "-o", out, ".")...)
		cmd.Dir = src
	} else {
		u, uerr := user.Current()
		if uerr != nil {
			t.Fatalf("building the probe contract: %v", uerr)
		}
		args := []string{"run", "--rm", "-e", "HOME=/tmp", "-u", u.Uid + ":" + u.Gid,
			"-v", src + ":/work", "-v", outDir + ":/out", "-w", "/work", "tinygo/tinygo:0.39.0", "tinygo"}
		cmd = exec.CommandContext(ctx, "docker", append(append(args, flags...), "-o", "/out/rcprobe.wasm", ".")...)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the probe contract: %v: %s", err, strings.TrimSpace(string(b)))
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("built probe contract not found at %s: %v", out, err)
	}
	return out
}

// rcpCall broadcasts a contract call signed by a Hive account (every devnet
// account signs with the initminer key) with an explicit rc_limit. The account
// is the RC payer.
func rcpCall(d *Devnet, account, cid, action, payload string, rcLimit int, intents []map[string]any) (string, error) {
	in := make([]any, 0, len(intents))
	for _, it := range intents {
		in = append(in, it)
	}
	return d.BroadcastCustomJSON("vsc.call", []string{account}, map[string]any{
		"net_id":      d.netId(),
		"contract_id": cid,
		"action":      action,
		"payload":     payload,
		"rc_limit":    rcLimit,
		"intents":     in,
	}, d.cfg.InitminerWIF)
}

// rcpWaitFinal waits for a transaction to reach CONFIRMED or FAILED on a node
// and returns the status ("" when it never did).
func rcpWaitFinal(d *Devnet, ctx context.Context, node int, txId string, timeout time.Duration) string {
	deadline := devnetNow().Add(timeout)
	status := ""
	for devnetNow().Before(deadline) {
		status, _ = d.FindTransactionStatus(ctx, node, txId)
		if strings.EqualFold(status, "CONFIRMED") || strings.EqualFold(status, "FAILED") {
			return strings.ToUpper(status)
		}
		devnetSleep(2 * time.Second)
	}
	return strings.ToUpper(status)
}

// rcpRec is one RC record of an account on a node: the RC frozen at a height.
type rcpRec struct {
	Amount int64  `bson:"amount"`
	Height uint64 `bson:"block_height"`
}

// rcpLatest is the newest RC record a node holds for an account ("hive:name").
// ok is false when the account has none yet.
func rcpLatest(ctx context.Context, d *Devnet, node int, account string) (rec rcpRec, ok bool, err error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return rcpRec{}, false, err
	}
	defer client.Disconnect(ctx)
	cur, err := client.Database(d.nodeDbName(node)).Collection("rcs").
		Find(ctx, bson.M{"account": account}, options.Find().SetSort(bson.M{"block_height": -1}).SetLimit(1))
	if err != nil {
		return rcpRec{}, false, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		return rcpRec{}, false, cur.Err()
	}
	if err := cur.Decode(&rec); err != nil {
		return rcpRec{}, false, err
	}
	return rec, true, nil
}

// rcpAll is every RC record a node holds for an account, oldest first, as
// "height:amount" strings, for comparing nodes.
func rcpAll(ctx context.Context, d *Devnet, node int, account string) ([]string, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)
	cur, err := client.Database(d.nodeDbName(node)).Collection("rcs").
		Find(ctx, bson.M{"account": account}, options.Find().SetSort(bson.M{"block_height": 1}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []string
	for cur.Next(ctx) {
		var r rcpRec
		if err := cur.Decode(&r); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%d:%d", r.Height, r.Amount))
	}
	return out, cur.Err()
}

// rcpCharge is what a node charged between two of its records for an account:
// the new record less the old one decayed to the new height (the node's own
// formula, rc-system CalculateFrozenBal).
func rcpCharge(prev rcpRec, havePrev bool, cur rcpRec) int64 {
	if !havePrev {
		return cur.Amount
	}
	return cur.Amount - rcSystem.CalculateFrozenBal(prev.Height, cur.Height, prev.Amount)
}

// rcpRefNode is the node whose RC records the measurements are read from. Not
// node 1: the harness restarts node 1 to deploy a contract, and a node
// restarted in the middle of a slot files the RC of that slot's later calls
// under the next slot (it forgets that the slot's block was already applied),
// so its record heights can differ from the others' for that one slot.
const rcpRefNode = 2

// rcpOutcome is one measured call.
type rcpOutcome struct {
	TxId   string
	Status string // CONFIRMED, FAILED, or "" (never final)
	Used   int64  // RC the chain charged the payer, read from rcpRefNode's records
	Height uint64 // the height of the RC record
}

// rcpMeasure sends one call as `account` and returns its status and what the
// chain charged for it. The payer must not have another call in flight.
func rcpMeasure(t *testing.T, d *Devnet, ctx context.Context, account, cid, action, payload string, rcLimit int, intents []map[string]any) rcpOutcome {
	t.Helper()
	payer := "hive:" + account
	prev, havePrev, err := rcpLatest(ctx, d, rcpRefNode, payer)
	if err != nil {
		t.Fatalf("reading %s RC record: %v", payer, err)
	}
	txId, err := rcpCall(d, account, cid, action, payload, rcLimit, intents)
	if err != nil {
		t.Fatalf("%s(%s) as %s: %v", action, rcpShort(payload), account, err)
	}
	out := rcpOutcome{TxId: txId}
	out.Status = rcpWaitFinal(d, ctx, rcpRefNode, txId, 6*time.Minute)
	if out.Status == "" {
		t.Fatalf("%s(%s) as %s never became final (tx %s)", action, rcpShort(payload), account, txId)
	}
	// The RC record for the slot is written when the slot is processed.
	deadline := devnetNow().Add(4 * time.Minute)
	for devnetNow().Before(deadline) {
		cur, ok, err := rcpLatest(ctx, d, rcpRefNode, payer)
		if err == nil && ok && (!havePrev || cur.Height > prev.Height) {
			out.Used = rcpCharge(prev, havePrev, cur)
			out.Height = cur.Height
			t.Logf("%s(%s) as %s: %s, charged %d RC (rc_limit %d, record at %d)", action, rcpShort(payload), account, out.Status, out.Used, rcLimit, cur.Height)
			return out
		}
		devnetSleep(2 * time.Second)
	}
	t.Fatalf("%s(%s) as %s is %s but no new RC record appeared for %s", action, rcpShort(payload), account, out.Status, payer)
	return out
}

func rcpShort(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// rcpSim dry-runs one call on a node and returns the RC it reports.
func rcpSim(ctx context.Context, d *Devnet, node int, account, cid, action, payload string, rcLimit int) (rc int64, success bool, msg string, err error) {
	var out struct {
		Sim []struct {
			Success bool    `json:"success"`
			Err     *string `json:"err"`
			ErrMsg  *string `json:"err_msg"`
			RcUsed  int64   `json:"rc_used"`
		} `json:"simulateContractCalls"`
	}
	err = d.gqlQuery(ctx, node, `query($i: SimulateContractCallsInput!){ simulateContractCalls(input: $i){ success err err_msg rc_used } }`,
		map[string]any{"i": map[string]any{
			"tx_id":          fmt.Sprintf("rcp-dryrun-%d", time.Now().UnixNano()), // wall: unique id
			"required_auths": []string{"hive:" + account},
			"calls": []map[string]any{{
				"contract_id": cid, "action": action, "payload": payload, "rc_limit": rcLimit,
			}},
		}}, &out)
	if err != nil {
		return 0, false, "", err
	}
	if len(out.Sim) != 1 {
		return 0, false, "", fmt.Errorf("dry run returned %d results", len(out.Sim))
	}
	if out.Sim[0].ErrMsg != nil {
		msg = *out.Sim[0].ErrMsg
	} else if out.Sim[0].Err != nil {
		msg = *out.Sim[0].Err
	}
	return out.Sim[0].RcUsed, out.Sim[0].Success, msg, nil
}

// rcpNodesAgreeOnRc checks that every listed node holds the same RC records
// for every account. Nodes are first brought to the same processed height.
func rcpNodesAgreeOnRc(t *testing.T, d *Devnet, ctx context.Context, nodes []int, accounts []string) {
	t.Helper()
	head := nodeBlock(t, d, ctx, nodes[0])
	for _, n := range nodes {
		if err := d.WaitForBlockProcessing(ctx, n, head, 5*time.Minute); err != nil {
			t.Fatalf("magi-%d did not reach block %d: %v", n, head, err)
		}
	}
	for _, acct := range accounts {
		// Records above `head` may exist on a node that ran ahead; compare up to it.
		var want []string
		for i, n := range nodes {
			all, err := rcpAll(ctx, d, n, acct)
			if err != nil {
				t.Fatalf("reading %s RC records on magi-%d: %v", acct, n, err)
			}
			got := rcpUpTo(all, head)
			if i == 0 {
				want = got
				continue
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("%s: magi-%d holds RC records %v, magi-%d holds %v", acct, n, got, nodes[0], want)
			}
		}
		t.Logf("%s: %d RC records, the same on nodes %v", acct, len(want), nodes)
	}
}

// rcpUpTo keeps the "height:amount" records at or below a height.
func rcpUpTo(recs []string, height uint64) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		var h uint64
		var a int64
		if _, err := fmt.Sscanf(r, "%d:%d", &h, &a); err == nil && h <= height {
			out = append(out, r)
		}
	}
	return out
}

// rcpActiveVersion is the consensus version of the election in force on a
// node, as a wallet reads it (electionByBlockHeight).
func rcpActiveVersion(ctx context.Context, d *Devnet, node int) (major, consensus uint64, err error) {
	var out struct {
		E *struct {
			Major     uint64 `json:"version_major"`
			Consensus uint64 `json:"protocol_version"`
		} `json:"electionByBlockHeight"`
	}
	if err := d.gqlQuery(ctx, node, `query{ electionByBlockHeight { version_major protocol_version } }`, nil, &out); err != nil {
		return 0, 0, err
	}
	if out.E == nil {
		return 0, 0, fmt.Errorf("magi-%d returned no election", node)
	}
	return out.E.Major, out.E.Consensus, nil
}

// rcpLatestOutput is the id and state root of a contract's newest output on a
// node.
func rcpLatestOutput(ctx context.Context, d *Devnet, node int, cid string) (id, stateMerkle string, height int64, err error) {
	var out struct {
		Outs []struct {
			Id          string `json:"id"`
			StateMerkle string `json:"state_merkle"`
			BlockHeight int64  `json:"block_height"`
		} `json:"findContractOutput"`
	}
	if err := d.gqlQuery(ctx, node, `query($c:String!){ findContractOutput(filterOptions:{byContract:$c, limit: 50}){ id state_merkle block_height } }`,
		map[string]any{"c": cid}, &out); err != nil {
		return "", "", 0, err
	}
	for _, o := range out.Outs {
		if o.BlockHeight >= height {
			id, stateMerkle, height = o.Id, o.StateMerkle, o.BlockHeight
		}
	}
	if id == "" {
		return "", "", 0, fmt.Errorf("magi-%d has no output for %s", node, cid)
	}
	return id, stateMerkle, height, nil
}

// rcpRaiseFloor proposes a consensus version from every witness and waits for
// an election that carries it. It returns that election's epoch and height.
func rcpRaiseFloor(t *testing.T, d *Devnet, ctx context.Context, nodes int, consensus uint64, timeout time.Duration) (epoch, height uint64) {
	t.Helper()
	cur, _ := d.currentEpoch(ctx, 1)
	for n := 1; n <= nodes; n++ {
		// activation_epoch 0: the handler takes the next epoch at processing time.
		if _, err := d.proposeConsensusVersion(n, 0, consensus, 0); err != nil {
			t.Logf("propose from magi-%d failed (continuing): %v", n, err)
		}
	}
	t.Logf("proposed 0.%d.0 from %d witnesses (current epoch %d)", consensus, nodes, cur)
	deadline := devnetNow().Add(timeout)
	for devnetNow().Before(deadline) {
		last, _, _ := rhLatestElection(ctx, d, 1)
		for e := cur; e <= last; e++ {
			if v, bh, err := rhElection(ctx, d, 1, e); err == nil && v >= consensus {
				return e, bh
			}
		}
		devnetSleep(5 * time.Second)
	}
	t.Fatalf("no election carried 0.%d.0 within %s", consensus, timeout)
	return 0, 0
}

// rcpSettleAfterDeploy waits until every node is two slots past the contract
// deploy, which restarts node 1: a node restarted in the middle of a slot
// files that slot's later RC under the next slot, so calls are only measured
// once that slot is behind every node.
func rcpSettleAfterDeploy(t *testing.T, d *Devnet, ctx context.Context, nodes int) {
	t.Helper()
	target := nodeBlock(t, d, ctx, 1) + 25
	for n := 1; n <= nodes; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, target, 8*time.Minute); err != nil {
			t.Fatalf("magi-%d did not reach block %d after the deploy: %v", n, target, err)
		}
	}
}
