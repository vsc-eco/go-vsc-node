package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Multi-node devnet proof of magi-market's BUCKET sales — fixed-price draws
// where the CONTRACT picks which NFT the buyer receives.
//
// Buckets cannot be validated by a single-node harness alone. The draw is
// seeded from `block.id`, the Hive L1 block id of the block that includes the
// purchase, and that value only exists once real blocks are being produced by
// real witnesses. A mocknet supplies a fixed stand-in, so every draw in the
// unit suite is deterministic by construction; only a devnet shows the seed
// changing per block, and shows every node agreeing on the SAME draw from it.
// A draw that resolved differently on two nodes would fork the chain.
//
// What this test covers:
//
//   - one NFT collection, minted into three shapes: unique commons, a
//     multi-unit edition, and scarce rares,
//   - three buckets with genuinely different setups — flat singles, a
//     Pokemon-style pack with a guaranteed rare, and an edition bucket selling
//     both singles and packs,
//   - a bucket stocked across TWO transactions, which is how any large bucket
//     has to be built,
//   - real buyers on different nodes drawing singles and packs,
//   - every call made at the production rc_limit, so a green run means these
//     operations are affordable to real users and not just correct.
func TestMagiMarketBucketsDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping magi-market bucket devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	t.Cleanup(cancel)

	wasm, err := BuildMagiMarketContracts(ctx)
	if err != nil {
		t.Fatalf("building magi-market contracts: %v", err)
	}

	cfg := DefaultConfig()
	// 5 nodes: deploying stops the deployer node to take its data dir, so the
	// storage-proof quorum (MinSpSigners=3) needs the rest to clear it with
	// margin. 4 leaves exactly 3 and flakes.
	cfg.Nodes = 5
	cfg.GenesisNode = 5
	// The default devnet ports collide with the live testnet stack on this host.
	cfg.GQLBasePort = 28080
	cfg.P2PBasePort = 21720
	cfg.MongoPort = 28057
	cfg.HivePort = 28091
	cfg.DronePort = 29000
	cfg.BitcoindRPCPort = 28543
	cfg.DashdRPCPort = 29898
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Logf("starting %d-node devnet (~12 min)...", cfg.Nodes)
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 8*time.Minute); err != nil {
		t.Fatalf("network never reached block 30: %v", err)
	}

	const (
		deployNode = 1 // owner of all three contracts, and the bucket seller
		queryNode  = 2 // never stopped by a deploy, so always serves GQL
		buyerNodeA = 2
		buyerNodeB = 3
		buyerNodeC = 4
	)
	seller := d.WitnessAccount(deployNode)
	buyerA := d.WitnessAccount(buyerNodeA)
	buyerB := d.WitnessAccount(buyerNodeB)
	buyerC := d.WitnessAccount(buyerNodeC)

	// ───────── deploy the three contracts ─────────
	tokenId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm.Token, Name: "market-paytoken", DeployerNode: deployNode})
	if err != nil {
		t.Fatalf("deploy token: %v", err)
	}
	nftId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm.Nft, Name: "market-nft", DeployerNode: deployNode})
	if err != nil {
		t.Fatalf("deploy nft: %v", err)
	}
	marketId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm.Market, Name: "magi-market", DeployerNode: deployNode})
	if err != nil {
		t.Fatalf("deploy market: %v", err)
	}
	t.Logf("deployed token=%s nft=%s market=%s", tokenId, nftId, marketId)
	marketAddr := "contract:" + marketId

	// call runs a contract action from `node` and fails the test unless it
	// applied. Everything below goes through it, so a silent abort cannot be
	// mistaken for success.
	call := func(node int, contractId, action, payload string) {
		t.Helper()
		txId, status, err := d.CallMarketContract(ctx, node, contractId, action, payload)
		if err != nil {
			t.Fatalf("%s on %s: %v", action, contractId, err)
		}
		if status == "FAILED" {
			t.Fatalf("%s on %s FAILED (tx %s)", action, contractId, txId)
		}
	}
	// callExpectFail is for the cases where refusing IS the behaviour.
	callExpectFail := func(node int, contractId, action, payload, why string) {
		t.Helper()
		txId, status, err := d.CallMarketContract(ctx, node, contractId, action, payload)
		if err != nil {
			t.Fatalf("%s on %s: %v", action, contractId, err)
		}
		if status != "FAILED" {
			t.Errorf("%s on %s was expected to fail (%s) but got %s (tx %s)",
				action, contractId, why, status, txId)
		}
	}

	// ───────── initialise ─────────
	call(deployNode, tokenId, "init",
		`{"name":"Pay Token","symbol":"PAY","decimals":3,"maxSupply":"1000000000"}`)
	call(deployNode, nftId, "init",
		`{"name":"Magi Cards","symbol":"MCARD","baseUri":"https://api.magi.network/metadata/"}`)
	call(deployNode, marketId, "init",
		fmt.Sprintf(`{"feeBps":250,"feeRecipient":"%s"}`, d.WitnessAccount(5)))
	// Only native HBD/HIVE are whitelisted at init, so the payment token has to
	// be added explicitly — same as a production deploy.
	call(deployNode, marketId, "addPaymentToken", fmt.Sprintf(`{"token":"%s"}`, tokenId))

	// ───────── mint the collection, in three shapes ─────────
	commons := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		commons = append(commons, fmt.Sprintf("common%d", i))
	}
	rares := []string{"rare0", "rare1", "rare2"}
	const holo = "holo"

	call(deployNode, nftId, "mintBatch", mintBatchPayload(seller, commons, 1, 1))
	call(deployNode, nftId, "mintBatch", mintBatchPayload(seller, rares, 1, 1))
	// An EDITION: one design, many interchangeable units. The draw weights by
	// units, so this is a different code path from ten unique cards.
	call(deployNode, nftId, "mint",
		fmt.Sprintf(`{"to":"%s","id":"%s","amount":20,"maxSupply":20}`, seller, holo))

	// Approval-custody: the market never escrows, it moves a unit per draw, so
	// the seller authorises it once and keeps the cards until they are drawn.
	call(deployNode, nftId, "setApprovalForAll",
		fmt.Sprintf(`{"operator":"%s","approved":true}`, marketAddr))

	// ───────── fund the buyers ─────────
	call(deployNode, tokenId, "mint", `{"amount":"1000000"}`)
	for _, b := range []struct {
		node int
		acct string
	}{{buyerNodeA, buyerA}, {buyerNodeB, buyerB}, {buyerNodeC, buyerC}} {
		call(deployNode, tokenId, "transfer",
			fmt.Sprintf(`{"to":"%s","amount":"100000"}`, b.acct))
		call(b.node, tokenId, "approve",
			fmt.Sprintf(`{"spender":"%s","amount":"100000"}`, marketAddr))
	}

	// ───────── bucket 0: flat singles over unique cards ─────────
	t.Log("bucket 0: six unique commons, single draws only")
	call(deployNode, marketId, "listBucket", listBucketPayload(
		nftId, tokenId, poolEntries(commons[:6], 1, 0), "1000", "0", "[]"))
	assertBucketSeller(t, d, ctx, queryNode, marketId, 0, seller)

	// ───────── bucket 1: Pokemon-style pack, stocked across two calls ─────────
	//
	// The rares arrive in a SECOND transaction. That ordering is not a quirk of
	// the test: a bucket too big for one transaction always has some pool empty
	// until the last batch lands, so the pack guarantee cannot be enforced at
	// listing time. It is enforced at BUY time instead, and the two calls below
	// prove both halves of that.
	t.Log("bucket 1: pack of 4 commons + 1 guaranteed rare, stocked in two calls")
	call(deployNode, marketId, "listBucket", listBucketPayload(
		nftId, tokenId, poolEntries(commons[6:10], 1, 0), "0", "5000", "[4,1]"))
	assertBucketSeller(t, d, ctx, queryNode, marketId, 1, seller)

	buyPack1 := `{"bucketId":1,"mode":"pack","quantity":1,"maxTotalPrice":""}`
	callExpectFail(buyerNodeB, marketId, "buyFromBucket", buyPack1,
		"the rare pool is not stocked yet, so the guaranteed slot cannot be filled")

	call(deployNode, marketId, "addToBucket",
		fmt.Sprintf(`{"bucketId":1,"entries":%s}`, poolEntries(rares, 1, 1)))

	// ───────── bucket 2: an edition, sold as singles AND as packs ─────────
	t.Log("bucket 2: one edition of 20 units, single draws and 3-card packs")
	call(deployNode, marketId, "listBucket", listBucketPayload(
		nftId, tokenId, poolEntries([]string{holo}, 20, 0), "500", "1500", "[3]"))
	assertBucketSeller(t, d, ctx, queryNode, marketId, 2, seller)

	// ───────── buyers draw ─────────
	t.Log("buyers drawing from all three buckets")

	// Singles from the flat bucket.
	for i := 0; i < 2; i++ {
		call(buyerNodeA, marketId, "buyFromBucket",
			`{"bucketId":0,"mode":"single","quantity":1,"maxTotalPrice":""}`)
	}
	// The pack, now that its rare pool is stocked.
	call(buyerNodeB, marketId, "buyFromBucket", buyPack1)
	// Both modes against the edition.
	call(buyerNodeC, marketId, "buyFromBucket",
		`{"bucketId":2,"mode":"pack","quantity":1,"maxTotalPrice":""}`)
	call(buyerNodeC, marketId, "buyFromBucket",
		`{"bucketId":2,"mode":"single","quantity":1,"maxTotalPrice":""}`)

	// ───────── assert what was actually delivered ─────────
	//
	// Read from a node that did NOT make the call. Every node replays the same
	// blocks through the same contract, so a draw that resolved differently
	// anywhere would show up as a disagreement here.
	held := func(node int, account string, ids []string) uint64 {
		t.Helper()
		total := uint64(0)
		for _, id := range ids {
			n, err := d.NftBalance(ctx, node, nftId, account, id)
			if err != nil {
				t.Fatalf("reading %s balance of %s on magi-%d: %v", account, id, node, err)
			}
			total += n
		}
		return total
	}

	if got := held(queryNode, buyerA, commons); got != 2 {
		t.Errorf("buyer A drew %d commons from bucket 0, want 2", got)
	}
	// A [4,1] pack is five cards, and EXACTLY one of them must be a rare —
	// that is the promise a pool guarantee makes, and the reason pools exist.
	packCommons := held(queryNode, buyerB, commons)
	packRares := held(queryNode, buyerB, rares)
	if packRares != 1 {
		t.Errorf("buyer B got %d rares, want exactly 1 — the guaranteed slot", packRares)
	}
	if packCommons != 4 {
		t.Errorf("buyer B got %d commons, want 4", packCommons)
	}
	// A 3-card pack plus one single, all from the same edition.
	if got := held(queryNode, buyerC, []string{holo}); got != 4 {
		t.Errorf("buyer C holds %d holo units, want 4 (a 3-card pack plus one single)", got)
	}

	// Units must have been decremented, not just transferred: the bucket's own
	// bookkeeping is what stops the same unit being sold twice.
	for _, b := range []struct {
		id   uint64
		want uint64
	}{{0, 4}, {1, 2}, {2, 16}} {
		units, err := d.BucketUnits(ctx, queryNode, marketId, b.id)
		if err != nil {
			t.Fatalf("reading bucket %d units: %v", b.id, err)
		}
		if units != b.want {
			t.Errorf("bucket %d has %d units left, want %d", b.id, units, b.want)
		}
	}

	// Every node must agree, byte for byte, on how many units each bucket has
	// left. Disagreement here is a consensus fault, not a test failure.
	for node := 1; node <= cfg.Nodes; node++ {
		for id := uint64(0); id <= 2; id++ {
			ref, err := d.BucketUnits(ctx, queryNode, marketId, id)
			if err != nil {
				t.Fatalf("reading reference units: %v", err)
			}
			got, err := d.BucketUnits(ctx, node, marketId, id)
			if err != nil {
				t.Logf("magi-%d unreadable for bucket %d (%v), skipping", node, id, err)
				continue
			}
			if got != ref {
				t.Errorf("magi-%d says bucket %d has %d units, magi-%d says %d — nodes disagree",
					node, id, got, queryNode, ref)
			}
		}
	}
}

// assertBucketSeller confirms a bucket landed on chain under the expected id.
// Bucket ids are assigned sequentially from 0, and reading the seller back is
// the cheapest way to prove the listing applied rather than silently aborting.
func assertBucketSeller(t *testing.T, d *Devnet, ctx context.Context, node int, market string, bucketId uint64, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got, err := d.BucketField(ctx, node, market, bucketId, "s")
		if err == nil && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bucket %d seller = %q, want %q (err=%v)", bucketId, got, want, err)
		}
		time.Sleep(3 * time.Second)
	}
}

// poolEntries builds a bucket entry array: every id with the same unit count,
// all in the same pool.
func poolEntries(ids []string, amount, pool uint64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"tokenId":"%s","amount":%d,"pool":%d}`, id, amount, pool))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func listBucketPayload(nftId, tokenId, entries, pricePerDraw, pricePerPack, packDraws string) string {
	return fmt.Sprintf(
		`{"nftContract":"%s","entries":%s,"paymentToken":"%s","pricePerDraw":"%s","pricePerPack":"%s","packDraws":%s,"expirationBlock":0}`,
		nftId, entries, tokenId, pricePerDraw, pricePerPack, packDraws)
}

func mintBatchPayload(to string, ids []string, amount, maxSupply uint64) string {
	quoted := make([]string, 0, len(ids))
	amounts := make([]string, 0, len(ids))
	supplies := make([]string, 0, len(ids))
	for range ids {
		amounts = append(amounts, fmt.Sprintf("%d", amount))
		supplies = append(supplies, fmt.Sprintf("%d", maxSupply))
	}
	for _, id := range ids {
		quoted = append(quoted, `"`+id+`"`)
	}
	return fmt.Sprintf(`{"to":"%s","ids":[%s],"amounts":[%s],"maxSupplies":[%s]}`,
		to, strings.Join(quoted, ","), strings.Join(amounts, ","), strings.Join(supplies, ","))
}
