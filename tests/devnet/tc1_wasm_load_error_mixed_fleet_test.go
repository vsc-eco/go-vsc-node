package devnet

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestWasmLoadErrorMixedFleet (TC-1): a contract call whose bytecode cannot be
// registered writes the runtime's error text into the contract output, and so
// into the block. Mainnet's build (TC1_OLD_DIR, aa112bc1) on magi-4 and magi-5,
// the candidate on magi-1..3. Two calls:
//   - junk bytecode (deployable: nothing validates wasm at deploy), which fails to parse;
//   - a valid module whose start function loops, which runs out of gas while it is
//     being registered ("cost limit exceeded", the common case on testnet history).
//
// Every node must store the identical output for both calls, and L2 blocks must
// keep finalizing on every node.
func TestWasmLoadErrorMixedFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	oldDir := os.Getenv("TC1_OLD_DIR")
	if oldDir == "" {
		t.Skip("TC1_OLD_DIR not set (mainnet's tree, aa112bc1)")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 5
	cfg.OldCodeSourceDir = oldDir
	cfg.OldCodeNodes = []int{4, 5}
	cfg.OldCodeSysconfig = true
	cfg.SkipFunding = false        // the deploys pay the contract fee from magi.test1
	cfg.LogLevel = "error,bp=info" // keep the block producer's signature-count warnings
	d, ctx := startDevnetNoKey(t, cfg, 60*time.Minute)

	time.Sleep(90 * time.Second)
	before, _, div0, rerr0 := vfScanBlockHeaders(d, ctx, cfg.Nodes)
	t.Logf("L2 max slot per node before: %v (divergence=%q readErr=%q)", before, div0, rerr0)
	if div0 != "" {
		t.Fatalf("PRECONDITION FAILED: nodes already diverged: %s", div0)
	}

	loop, _ := hex.DecodeString("0061736d01000000" + "010401600000" + "03020100" + "080100" + "0a09010700" + "03400c000b0b")
	bodies := map[string][]byte{
		"junk": []byte("\x00asm\x01\x00\x00\x00\xff\xff\xff\xff\x0fTC1-junk-bytecode"),
		"loop": loop,
	}
	calls := map[string]string{}
	for _, name := range []string{"junk", "loop"} {
		p := filepath.Join(t.TempDir(), name+".wasm")
		if err := os.WriteFile(p, bodies[name], 0o644); err != nil {
			t.Fatal(err)
		}
		// The deployer gives up on its storage proof after a short timeout (seen right
		// after a node restart), so retry the deploy.
		var cid string
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			if cid, err = d.DeployContract(ctx, ContractDeployOpts{WasmPath: p, Name: "tc1-" + name, Description: "TC-1 " + name, DeployerNode: 1}); err == nil {
				break
			}
			t.Logf("deploying the %s contract, attempt %d: %v", name, attempt, err)
			time.Sleep(30 * time.Second)
		}
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: deploying the %s contract: %v", name, err)
		}
		time.Sleep(20 * time.Second)
		tx, err := d.CallContractWithIntents(ctx, 1, cid, "run", "{}", nil, 10000)
		if err != nil {
			t.Fatalf("calling the %s contract: %v", name, err)
		}
		calls[name] = tx
		t.Logf("%s contract %s, call tx %s", name, cid, tx)
	}
	callSlots, _, _, _ := vfScanBlockHeaders(d, ctx, cfg.Nodes)
	t.Logf("L2 max slot per node right after the calls: %v", callSlots)
	time.Sleep(90 * time.Second)

	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	for _, name := range []string{"junk", "loop"} {
		seen := map[int]string{}
		for n := 1; n <= cfg.Nodes; n++ {
			var doc struct {
				Id      string `bson:"id"`
				Results []struct {
					Ok     bool   `bson:"ok"`
					ErrMsg string `bson:"errMsg"`
				} `bson:"results"`
			}
			err := client.Database(d.nodeDbName(n)).Collection("contract_state").FindOne(ctx, bson.M{"inputs": calls[name]}).Decode(&doc)
			if err != nil || len(doc.Results) == 0 {
				seen[n] = "NO OUTPUT"
				t.Errorf("VACUOUS: magi-%d stored no output for the %s call (%v)", n, name, err)
				continue
			}
			seen[n] = fmt.Sprintf("%s ok=%v err=%q", doc.Id, doc.Results[0].Ok, doc.Results[0].ErrMsg)
		}
		t.Logf("RESULT %s call per node: %v", name, seen)
		for n := 2; n <= cfg.Nodes; n++ {
			if seen[n] != seen[1] {
				t.Errorf("%s call: magi-%d stored %q, magi-1 stored %q (old build on magi-4,5)", name, n, seen[n], seen[1])
			}
		}
	}

	// Every node missing the output means no block carrying it was ever signed:
	// show what the producers logged about signatures.
	for n := 1; n <= cfg.Nodes; n++ {
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
		short := 0
		var last string
		for _, l := range strings.Split(logs, "\n") {
			if strings.Contains(l, "not enough signatures") {
				short++
				last = l
			}
		}
		t.Logf("RESULT magi-%d: %d 'not enough signatures' lines; last: %s", n, short, last)
	}

	time.Sleep(120 * time.Second)
	after, compared, div, rerr := vfScanBlockHeaders(d, ctx, cfg.Nodes)
	t.Logf("RESULT L2 max slot per node after: %v (compared %d slots, divergence=%q readErr=%q)", after, compared, div, rerr)
	if div != "" {
		t.Errorf("L2 block divergence after the calls: %s", div)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if after[n] <= before[n] {
			t.Errorf("magi-%d made no L2 progress after the calls (%d -> %d)", n, before[n], after[n])
		}
	}
}
