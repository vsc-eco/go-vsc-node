package devnet

// What a network must keep whatever a contract stores, on a real devnet, from
// consensus 0.16.0 (independent of the write price): a node that was offline
// for a slot can always catch up to its peers, a large slot output reaches
// every node, and the size bounds hold. hardeningDevnet and rcpCallNoWait are
// in many_keys_devnet_test.go.
//
//	go test -v -run 'TestHardening_' -timeout 150m ./tests/devnet/

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"github.com/vsc-eco/hivego"
)

// hardeningCallsInOneBlock broadcasts one contract call from each of the given
// witnesses in ONE Hive transaction, so they land in one block and one slot and
// share the contract's output object for that slot.
func hardeningCallsInOneBlock(d *Devnet, witnesses []int, cid, action, payload string, rcLimit int) (string, error) {
	ops := make([]hivego.HiveOperation, 0, len(witnesses))
	for _, n := range witnesses {
		js := fmt.Sprintf(`{"net_id":%q,"contract_id":%q,"action":%q,"payload":%q,"rc_limit":%d,"intents":[]}`, d.netId(), cid, action, payload, rcLimit)
		ops = append(ops, hivego.CustomJsonOperation{
			RequiredAuths:        []string{d.witnessAccount(n)},
			RequiredPostingAuths: []string{},
			Id:                   "vsc.call",
			Json:                 js,
		})
	}
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = devnetChainID
	wif := d.cfg.InitminerWIF
	return broadcastRetryingStale(client, ops, &wif, "calls in one block")
}

// hardeningOfflineSlot takes a node offline, lands one log of logBytes from
// each witness in one slot, brings the node back and reports whether it caught
// up with the others within `within`. With refused, the calls (one Hive
// transaction) must fail on the 0.16.0 size bound instead, and the output must
// stay small.
func hardeningOfflineSlot(t *testing.T, d *Devnet, ctx context.Context, cid string, logBytes int, refused bool, within time.Duration) (caughtUp bool, behindBy uint64) {
	t.Helper()
	const offline = 5
	witnesses := []int{1, 2, 3, 4, 5}
	if err := d.StopNode(ctx, offline); err != nil {
		t.Fatalf("stopping magi-%d: %v", offline, err)
	}
	devnetSleep(10 * time.Second)
	txId, err := hardeningCallsInOneBlock(d, witnesses, cid, "logBlob", fmt.Sprint(logBytes), logBytes+30_000)
	if err != nil {
		t.Fatalf("broadcasting the calls: %v", err)
	}
	// One Hive transaction has one status for all its operations.
	want := "CONFIRMED"
	if refused {
		want = "FAILED"
	}
	if st := rcpWaitFinal(d, ctx, 2, txId, 8*time.Minute); st != want {
		t.Fatalf("the calls (%d logs of %d bytes) are %q on magi-2, want %s", len(witnesses), logBytes, st, want)
	}
	if refused {
		reason := hardeningTxError(ctx, d, 2, txId)
		if !strings.Contains(reason, "over the") {
			t.Fatalf("the calls failed for %q, want the size bound", reason)
		}
		t.Logf("the calls failed on the size bound: %s", reason)
	}
	// The slot's output object, as an online node holds it: its content is what
	// this case claims, so it is measured. It appears once the slot's block is in.
	var outId string
	var outHeight int64
	if err := pollNode(2, 6*time.Minute, func() bool {
		id, _, h, e := rcpLatestOutput(ctx, d, 2, cid)
		if e != nil || id == "" {
			return false
		}
		// The newest output must be the one that carries these logs (or, when
		// refused, the failure: a small output newer than the last one).
		if size, _, logged := hardeningOutputSize(ctx, d, 2, id); (!refused && logged < logBytes) || (refused && (size == 0 || h <= hardeningLastOutputHeight)) {
			return false
		}
		outId, outHeight = id, h
		return true
	}); err != nil {
		id, _, h, _ := rcpLatestOutput(ctx, d, 2, cid)
		size, results, logged := hardeningOutputSize(ctx, d, 2, id)
		t.Fatalf("no contract output carrying the logs appeared: newest is %s (block %d): %d bytes, %d results, %d bytes of logs (%v)", id, h, size, results, logged, err)
	}
	hardeningLastOutputHeight = outHeight
	size, results, logBytesTotal := hardeningOutputSize(ctx, d, 2, outId)
	t.Logf("the slot's contract output %s (block %d): %d bytes as JSON, %d results, %d bytes of logs", outId, outHeight, size, results, logBytesTotal)
	if refused {
		if size > params.MAX_CONTRACT_OUTPUT_BYTES || logBytesTotal != 0 {
			t.Fatalf("the refused calls left an output of %d bytes with %d bytes of logs", size, logBytesTotal)
		}
	} else if results != len(witnesses) || logBytesTotal < len(witnesses)*logBytes {
		t.Fatalf("the output holds %d results with %d bytes of logs, want all %d calls in ONE output with %d bytes: the calls did not share a slot",
			results, logBytesTotal, len(witnesses), len(witnesses)*logBytes)
	}
	// The four online nodes built and signed the block: they must carry on.
	head := nodeBlock(t, d, ctx, 1)
	for n := 1; n <= 4; n++ {
		if err := d.WaitForBlockProcessing(ctx, n, head+20, 8*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d stalled after a slot with %d bytes of logs: %v", n, len(witnesses)*logBytes, err)
		}
	}
	if err := d.StartNode(ctx, offline); err != nil {
		t.Fatalf("restarting magi-%d: %v", offline, err)
	}
	target := nodeBlock(t, d, ctx, 1)
	err = d.WaitForBlockProcessing(ctx, offline, target, within)
	got, _ := nodeBlockOpt(d, ctx, offline)
	if has, _, _ := hardeningOutputSize(ctx, d, offline, outId); has > 0 {
		t.Logf("magi-%d now holds the output object %s itself (%d bytes as JSON)", offline, outId, has)
	} else {
		t.Logf("magi-%d does NOT hold the output object %s", offline, outId)
	}
	if err != nil {
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", offline))
		if strings.Contains(logs, "CONTENT FETCH STALLED") {
			t.Logf("magi-%d logs CONTENT FETCH STALLED: it is waiting for an object no peer can send it", offline)
		}
		if target > got {
			behindBy = target - got
		}
		return false, behindBy
	}
	return true, 0
}

// hardeningLastOutputHeight is the height of the newest probe output a case
// has seen, so a later case waits for a newer one.
var hardeningLastOutputHeight int64

// hardeningTxError is why a transaction failed, as a node recorded it.
func hardeningTxError(ctx context.Context, d *Devnet, node int, txId string) string {
	var out struct {
		Txs []struct {
			Error *string `json:"error"`
		} `json:"findTransaction"`
	}
	if err := d.gqlQuery(ctx, node, `query($id:String!){findTransaction(filterOptions:{byId:$id}){error}}`, map[string]any{"id": txId}, &out); err != nil || len(out.Txs) == 0 || out.Txs[0].Error == nil {
		return ""
	}
	return *out.Txs[0].Error
}

// A node that was offline while a slot's contract output was produced must be
// able to fetch it from its peers and catch up. A contract's results and logs
// for one slot are one object fetched in one message, so the 0.16.0 size bound
// keeps that object within the per-message limit.
func TestHardening_OfflineNodeCatchesUpAfterALargeSlotOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet hardening test in short mode")
	}
	d, ctx, cid := hardeningDevnet(t, 140*time.Minute)

	// Control: five logs of 150,000 bytes, a 0.75 MB output. The node comes back
	// and catches up; this is the same procedure as the case below.
	if !t.Run("01_a_0.75_MB_slot_output_is_fetched", func(t *testing.T) {
		ok, behind := hardeningOfflineSlot(t, d, ctx, cid, 150_000, false, 15*time.Minute)
		if !ok {
			t.Fatalf("the node that was offline is %d blocks behind after a 0.75 MB slot output", behind)
		}
	}) {
		t.Fatal("the control failed: the procedure itself does not work on this devnet")
	}

	// Five logs of 900,000 bytes would make an output larger than one message
	// carries. From 0.16.0 the calls fail on the size bound, so the object
	// stays fetchable and the node that was offline catches up.
	t.Run("02_logs_for_a_4.5_MB_output_are_refused_and_the_node_catches_up", func(t *testing.T) {
		ok, behind := hardeningOfflineSlot(t, d, ctx, cid, 900_000, true, 15*time.Minute)
		if !ok {
			t.Fatalf("the node that was offline is %d blocks behind and not moving after the refused calls", behind)
		}
	})

	t.Run("03_the_nodes_that_signed_the_block_keep_advancing", func(t *testing.T) {
		head := nodeBlock(t, d, ctx, 1)
		for n := 1; n <= 4; n++ {
			if err := d.WaitForBlockProcessing(ctx, n, head+20, 8*time.Minute); err != nil {
				t.Fatalf("magi-%d stalled: %v", n, err)
			}
		}
	})

	// A new node: it holds nothing and has to get every object of the chain's
	// history from its peers.
	t.Run("04_a_node_with_an_empty_store_syncs_the_whole_chain", func(t *testing.T) {
		const fresh = 5
		if err := d.StopNode(ctx, fresh); err != nil {
			t.Fatalf("stopping magi-%d: %v", fresh, err)
		}
		hardeningWipeNode(t, d, ctx, fresh)
		if err := d.StartNode(ctx, fresh); err != nil {
			t.Fatalf("restarting magi-%d: %v", fresh, err)
		}
		target := nodeBlock(t, d, ctx, 1)
		err := d.WaitForBlockProcessing(ctx, fresh, target, 20*time.Minute)
		got, _ := nodeBlockOpt(d, ctx, fresh)
		if err != nil {
			logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", fresh))
			stalled := strings.Contains(logs, "CONTENT FETCH STALLED")
			t.Fatalf("a node with an empty store stopped at block %d of %d and cannot sync (CONTENT FETCH STALLED logged: %v): %v", got, target, stalled, err)
		}
		t.Logf("the node with an empty store synced to block %d", got)
	})
}

// hardeningWipeNode empties a stopped node's database, block store and
// announce queue store, so that on restart it is a new node: it replays from
// genesis and fetches everything. Its keys and config stay.
func hardeningWipeNode(t *testing.T, d *Devnet, ctx context.Context, node int) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	if err := client.Database(d.nodeDbName(node)).Drop(ctx); err != nil {
		t.Fatalf("dropping magi-%d's database: %v", node, err)
	}
	// The block store is written by the container's user: remove it from a container.
	dataDir := filepath.Join(d.devnetDir, fmt.Sprintf("data-%d", node))
	if out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", dataDir+":/n", "alpine", "sh", "-c", "rm -rf /n/badger /n/badger-provider && ls /n").CombinedOutput(); err != nil {
		t.Fatalf("emptying magi-%d's block store: %v: %s", node, err, out)
	} else {
		t.Logf("magi-%d wiped: database dropped, block store removed; left in its data dir: %s", node, strings.Join(strings.Fields(string(out)), " "))
	}
}

// hardeningOutputSize reads a contract output object from a node's OWN store
// (getDagByCID never asks the network) and returns its size as JSON, how many
// call results it holds and the bytes of logs in them. Zeros when the node
// does not hold it.
func hardeningOutputSize(ctx context.Context, d *Devnet, node int, outputId string) (size, results, logBytes int) {
	var out struct {
		Dag any `json:"getDagByCID"`
	}
	if err := d.gqlQuery(ctx, node, `query($c:String!){ getDagByCID(cidString:$c) }`, map[string]any{"c": outputId}, &out); err != nil || out.Dag == nil {
		return 0, 0, 0
	}
	raw, ok := out.Dag.(string)
	if !ok {
		b, _ := json.Marshal(out.Dag)
		raw = string(b)
	}
	var obj struct {
		Results []struct {
			Logs []string `json:"logs"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return len(raw), 0, 0
	}
	for _, r := range obj.Results {
		for _, l := range r.Logs {
			logBytes += len(l)
		}
	}
	return len(raw), len(obj.Results), logBytes
}

// How a large slot output reaches a node that did not build it, separating the
// wipe procedure from the output size:
//
//   - 01 is the control: a node with an empty store syncs a chain with no
//     large output. If it fails, the wipe procedure (not the output size) is
//     what stops a new node.
//   - 02/03 build a large output from a return value while a node is offline,
//     then bring it back.
//   - 04 wipes that node and syncs it past the large output.
func TestHardening_LargeOutputReachesOtherNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet hardening test in short mode")
	}
	d, ctx, cid := hardeningDevnet(t, 150*time.Minute)
	const node = 5
	resync := func(t *testing.T, what string) {
		t.Helper()
		if err := d.StopNode(ctx, node); err != nil {
			t.Fatalf("stopping magi-%d: %v", node, err)
		}
		hardeningWipeNode(t, d, ctx, node)
		if err := d.StartNode(ctx, node); err != nil {
			t.Fatalf("restarting magi-%d: %v", node, err)
		}
		target := nodeBlock(t, d, ctx, 1)
		err := d.WaitForBlockProcessing(ctx, node, target, 20*time.Minute)
		got, _ := nodeBlockOpt(d, ctx, node)
		if err != nil {
			t.Fatalf("%s: magi-%d with an empty store stopped at block %d of %d: %v", what, node, got, target, err)
		}
		t.Logf("%s: magi-%d with an empty store synced to block %d", what, node, got)
	}

	if !t.Run("01_control_a_node_with_an_empty_store_syncs_a_chain_without_large_outputs", func(t *testing.T) {
		resync(t, "control")
	}) {
		t.Fatal("the control failed: a node with an empty store cannot sync this devnet at all, so the cases below cannot test the output size")
	}

	var outId string
	// From 0.16.0 a return value this large fails on the size bound, is charged
	// what it used, and its output holds only the small failure.
	t.Run("02_a_4.5_MB_return_value_while_a_node_is_offline_is_refused", func(t *testing.T) {
		if err := d.StopNode(ctx, node); err != nil {
			t.Fatalf("stopping magi-%d: %v", node, err)
		}
		devnetSleep(10 * time.Second)
		_, _, prevHeight, _ := rcpLatestOutput(ctx, d, 2, cid)
		o := rcpMeasure(t, d, ctx, d.witnessAccount(2), cid, "retBlob", "4500000", 100_000, nil)
		if o.Status != "FAILED" {
			t.Fatalf("status %s, want FAILED on the size bound", o.Status)
		}
		if reason := hardeningTxError(ctx, d, 2, o.TxId); !strings.Contains(reason, "over the") {
			t.Fatalf("failed for %q, want the size bound", reason)
		}
		if o.Used > 1000 {
			t.Fatalf("charged %d RC for the refused call, want about 160 (what it used)", o.Used)
		}
		if err := pollNode(2, 6*time.Minute, func() bool {
			id, _, h, e := rcpLatestOutput(ctx, d, 2, cid)
			if e != nil || id == "" || h <= prevHeight {
				return false
			}
			outId = id
			return true
		}); err != nil {
			t.Fatalf("no output with the failure appeared: %v", err)
		}
		size, results, _ := hardeningOutputSize(ctx, d, 2, outId)
		if size > 64*1024 {
			t.Fatalf("the refused call left an output of %d bytes", size)
		}
		t.Logf("a 4.5 MB return value failed on the bound for %d RC; the output %s is %d bytes, %d results", o.Used, outId, size, results)
	})

	t.Run("03_the_offline_node_comes_back_and_catches_up", func(t *testing.T) {
		head := nodeBlock(t, d, ctx, 1)
		for n := 1; n <= 4; n++ {
			if err := d.WaitForBlockProcessing(ctx, n, head+20, 8*time.Minute); err != nil {
				t.Fatalf("magi-%d stalled after the large output: %v", n, err)
			}
		}
		started := devnetNow()
		if err := d.StartNode(ctx, node); err != nil {
			t.Fatalf("restarting magi-%d: %v", node, err)
		}
		target := nodeBlock(t, d, ctx, 1)
		if err := d.WaitForBlockProcessing(ctx, node, target, 15*time.Minute); err != nil {
			got, _ := nodeBlockOpt(d, ctx, node)
			t.Fatalf("magi-%d is at block %d of %d after it came back: %v", node, got, target, err)
		}
		has, _, _ := hardeningOutputSize(ctx, d, node, outId)
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
		t.Logf("magi-%d caught up in %s; holds the output itself: %v (%d bytes); CONTENT FETCH STALLED logged: %v",
			node, devnetSince(started).Round(time.Second), has > 0, has, strings.Contains(logs, "CONTENT FETCH STALLED"))
	})

	t.Run("04_a_node_with_an_empty_store_syncs_past_the_refused_call", func(t *testing.T) {
		resync(t, "after the refused 4.5 MB return value")
	})
}

// The 0.16.0 size bounds on a real network: every object a block hands to
// other nodes stays within the per-message fetch limit.
//   - 01: two transactions in one block that return 600 KB each: the second
//     takes the contract's output for the block over 1 MiB and fails; the
//     first stands.
//   - 02/03: one stored value over 1 MiB, one key over 1 KiB fail; a value of
//     exactly 1 MiB is stored.
//   - 04: one call that makes 8,000 ledger transfers (about 1.4 MB of the
//     block's ledger record) fails; 2,000 pass.
//   - 05: a node that was offline through all of it, and a node with an empty
//     store, catch up.
func TestHardening_SizeBoundHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet hardening test in short mode")
	}
	d, ctx, cid := hardeningDevnet(t, 150*time.Minute)
	const offline = 5
	if err := d.StopNode(ctx, offline); err != nil {
		t.Fatalf("stopping magi-%d: %v", offline, err)
	}
	devnetSleep(10 * time.Second)

	wantRefused := func(t *testing.T, o rcpOutcome, what string) {
		t.Helper()
		if o.Status != "FAILED" {
			t.Fatalf("%s: status %s, want FAILED", what, o.Status)
		}
		reason := hardeningTxError(ctx, d, 2, o.TxId)
		if !strings.Contains(reason, "limit") {
			t.Fatalf("%s failed for %q, want a size limit", what, reason)
		}
		t.Logf("%s: FAILED, charged %d RC: %s", what, o.Used, reason)
	}

	t.Run("01_two_600_KB_returns_in_one_block_the_second_fails", func(t *testing.T) {
		for attempt := 1; attempt <= 3; attempt++ {
			a := rcpCallNoWait(t, d, d.witnessAccount(2), cid, "retBlob", "600000", 100_000)
			b := rcpCallNoWait(t, d, d.witnessAccount(3), cid, "retBlob", "600000", 100_000)
			sa, sb := rcpWaitFinal(d, ctx, 2, a, 8*time.Minute), rcpWaitFinal(d, ctx, 2, b, 8*time.Minute)
			t.Logf("attempt %d: %s %s, %s %s", attempt, a, sa, b, sb)
			if sa == "CONFIRMED" && sb == "CONFIRMED" {
				t.Logf("both confirmed: they landed in different blocks; again")
				continue
			}
			if (sa == "CONFIRMED") == (sb == "CONFIRMED") {
				t.Fatalf("statuses %s and %s, want one CONFIRMED and one FAILED", sa, sb)
			}
			failed := b
			if sa != "CONFIRMED" {
				failed = a
			}
			if reason := hardeningTxError(ctx, d, 2, failed); !strings.Contains(reason, "over the") {
				t.Fatalf("the second call failed for %q, want the output bound", reason)
			}
			return
		}
		t.Fatalf("three times the two calls landed in different blocks")
	})

	t.Run("02_a_value_over_1_MiB_and_a_key_over_1_KiB_fail", func(t *testing.T) {
		wantRefused(t, rcpMeasure(t, d, ctx, d.witnessAccount(3), cid, "setBlob", fmt.Sprintf("big:%d", params.MAX_STATE_VALUE_BYTES+1), 1_200_000, nil), "a value of 1 MiB + 1")
		wantRefused(t, rcpMeasure(t, d, ctx, d.witnessAccount(3), cid, "setString", strings.Repeat("k", params.MAX_STATE_KEY_BYTES+1)+",x", 100_000, nil), "a key of 1 KiB + 1")
	})

	t.Run("03_a_value_of_exactly_1_MiB_is_stored", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, d.witnessAccount(4), cid, "setBlob", fmt.Sprintf("max:%d", params.MAX_STATE_VALUE_BYTES), 1_080_000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("a value of exactly 1 MiB: status %s (%s)", o.Status, hardeningTxError(ctx, d, 2, o.TxId))
		}
	})

	t.Run("04_8000_ledger_transfers_in_one_call_fail_2000_pass", func(t *testing.T) {
		allow := []map[string]any{{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": "10.000"}}}
		wantRefused(t, rcpMeasure(t, d, ctx, d.witnessAccount(1), cid, "manyTransfers", "8000", 100_000, allow), "8,000 transfers")
		o := rcpMeasure(t, d, ctx, d.witnessAccount(1), cid, "manyTransfers", "2000", 100_000, allow)
		if o.Status != "CONFIRMED" {
			t.Fatalf("2,000 transfers: status %s (%s)", o.Status, hardeningTxError(ctx, d, 2, o.TxId))
		}
	})

	t.Run("05_the_offline_node_and_a_node_with_an_empty_store_catch_up", func(t *testing.T) {
		if err := d.StartNode(ctx, offline); err != nil {
			t.Fatalf("restarting magi-%d: %v", offline, err)
		}
		target := nodeBlock(t, d, ctx, 1)
		if err := d.WaitForBlockProcessing(ctx, offline, target, 15*time.Minute); err != nil {
			got, _ := nodeBlockOpt(d, ctx, offline)
			t.Fatalf("magi-%d is at block %d of %d after it came back: %v", offline, got, target, err)
		}
		if err := d.StopNode(ctx, offline); err != nil {
			t.Fatalf("stopping magi-%d: %v", offline, err)
		}
		hardeningWipeNode(t, d, ctx, offline)
		if err := d.StartNode(ctx, offline); err != nil {
			t.Fatalf("restarting magi-%d: %v", offline, err)
		}
		target = nodeBlock(t, d, ctx, 1)
		if err := d.WaitForBlockProcessing(ctx, offline, target, 20*time.Minute); err != nil {
			got, _ := nodeBlockOpt(d, ctx, offline)
			t.Fatalf("magi-%d with an empty store is at block %d of %d: %v", offline, got, target, err)
		}
		head := nodeBlock(t, d, ctx, 1)
		for n := 1; n <= 5; n++ {
			if err := d.WaitForBlockProcessing(ctx, n, head+10, 8*time.Minute); err != nil {
				t.Fatalf("magi-%d is not advancing: %v", n, err)
			}
		}
	})
}
