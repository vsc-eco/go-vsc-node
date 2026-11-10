//go:build evm_devnet

// CG-1 devnet proof: one node whose own contract_state reads fail, for as long
// as the fault lasts, while the user withdraws ETH from the EVM bridge.
//
// An EVM vault signature needs every listed signer. Before the CG-1 fix the EVM
// sign gate read the bridge's halt flags (th, si) from Mongo at sign time and
// counted a failed read as a halt, so that one node refused, and the signature
// could not complete on ANY node until its fault cleared. With the fix the flags
// come from the consensus cache and a refusal that rests only on this node's own
// failed read is not a refusal, so the withdrawal is signed while the fault lasts.
//
// Phases, on a real fleet (hived + 5 magid + anvil + the real bot):
//
//	CONTROL  no fault: withdrawal 1 is signed and mined; its time is the yardstick.
//	FAULT    a MongoDB failCommand failpoint makes `find` on ONE node's
//	         contract_state fail (Interrupted), persistently; every other
//	         collection and every other node keep serving. Withdrawal 2:
//	           before the fix: NOT mined while the fault lasts, and the faulted
//	           node logs the fail-closed halt read;
//	           with the fix: mined while the fault lasts, the faulted node logs
//	           the read fault and starts its signing session.
//	RECOVER  the failpoint is cleared; before the fix withdrawal 2 is then mined
//	         (time measured); with the fix a third withdrawal is mined as normal.
//
// Run: go test -tags evm_devnet -run TestEVMBridge_CG1_OneNodeReadFault ./tests/devnet/ -timeout 120m -v
// The bridge is bound in the nodes' config and the chain runs 0.13 (a01Devnet); without that the gate never runs.
// (CG1_FIXED=1 when the node binary carries the CG-1 fix)
package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	cg1FailClosedLog = "EVM vault keysign halt flag unreadable; treating as halted (fail closed)"
	cg1FaultLog      = "evm output-scoping: this node could not read what the verdict needs"
	cg1SignStartLog  = "sign dispatcher start"
)

func cg1Fixed() bool { return os.Getenv("CG1_FIXED") != "" }

// cg1Failpoint arms (on) or clears a failCommand failpoint on `find` in one
// node's contract_state collection.
func cg1Failpoint(t *testing.T, d *Devnet, ctx context.Context, node int, on bool) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo connect for failpoint: %v", err)
	}
	defer client.Disconnect(ctx)
	cmd := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}
	if on {
		cmd = bson.D{
			{Key: "configureFailPoint", Value: "failCommand"},
			{Key: "mode", Value: "alwaysOn"},
			{Key: "data", Value: bson.D{
				{Key: "failCommands", Value: bson.A{"find"}},
				{Key: "namespace", Value: d.nodeDbName(node) + ".contract_state"},
				{Key: "errorCode", Value: 11601}, // Interrupted: not retried by the driver
			}},
		}
	}
	if err := client.Database("admin").RunCommand(ctx, cmd).Err(); err != nil {
		t.Fatalf("configureFailPoint(on=%v, node=%d): %v", on, node, err)
	}
	t.Logf("failpoint on=%v for %s.contract_state", on, d.nodeDbName(node))
}

// cg1VerifyFailpoint: a find on the faulted node's contract_state fails, its
// tss_requests and a healthy node's contract_state still read.
func cg1VerifyFailpoint(t *testing.T, d *Devnet, ctx context.Context, faulted, healthy int) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	if err := client.Database(d.nodeDbName(faulted)).Collection("contract_state").FindOne(ctx, bson.M{}).Err(); err == nil || !strings.Contains(err.Error(), "Interrupted") && !strings.Contains(err.Error(), "11601") {
		t.Fatalf("faulted node %d contract_state find: err %v, want the failpoint's Interrupted", faulted, err)
	}
	if err := client.Database(d.nodeDbName(faulted)).Collection("tss_requests").FindOne(ctx, bson.M{}).Err(); err != nil && !strings.Contains(err.Error(), "no documents") {
		t.Fatalf("faulted node %d tss_requests must still read: %v", faulted, err)
	}
	if err := client.Database(d.nodeDbName(healthy)).Collection("contract_state").FindOne(ctx, bson.M{}).Err(); err != nil && !strings.Contains(err.Error(), "no documents") {
		t.Fatalf("healthy node %d contract_state must still read: %v", healthy, err)
	}
}

func TestEVMBridge_CG1_OneNodeReadFault(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	// The EVM sign gate runs only for a configured EVM bridge on the 0.13 line:
	// a01Devnet sets the consensus floor 0.13, binds the deployed bridge in
	// every node's OracleParams.EvmBridgeContracts and restarts the nodes.
	// (A first run on setupEvmBridgeDevnet had no binding: the gate never ran
	// and the withdrawal signed through the fault on the pre-fix binary.)
	ctx, e := a01Devnet(t, 110*time.Minute)
	d := e.d
	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	t.Cleanup(func() { cg1Failpoint(t, d, context.Background(), 2, false) })

	botKey, _ := ethCrypto.GenerateKey()
	botDID := dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()).String()
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	if err := e.fundDID(botDID, "20000.000"); err != nil {
		t.Fatalf("fund bot DID: %v", err)
	}
	e.botAttests = true
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := anvil.SetBalance(ctx, vault, eth(10)); err != nil {
		t.Fatalf("fund vault gas: %v", err)
	}
	bot := StartBot(t, ctx, BotConfig{
		EthRPC: anvil.URL, ContractID: e.amID,
		GraphQLURL: e.d.GQLEndpoint(e.gqlNode), EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)),
	})
	t.Cleanup(bot.Stop)
	botLogs := func() string { return strings.Join(bot.Logs(), "\n") }
	if !bot.WaitForLog(ctx, "E23: attested the vault key's check signature", evmL2Wait+10*time.Minute) {
		t.Fatalf("the bot never attested the vault key:\n%s", botLogs())
	}
	if !bot.WaitForLog(ctx, "A18: vault is the TSS key's address", evmL2Wait) {
		t.Fatalf("the bot did not take the vault:\n%s", botLogs())
	}

	// Deposit 6 ETH; the bot maps it.
	depKey, _ := ethCrypto.GenerateKey()
	depAddr := ethCrypto.PubkeyToAddress(depKey.PublicKey)
	if err := anvil.SetBalance(ctx, depAddr, eth(7)); err != nil {
		t.Fatal(err)
	}
	_, depBlock, err := anvil.SendETH(ctx, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := e.plantFinalized(anvil, depBlock); err != nil {
		t.Fatalf("plant deposit header: %v", err)
	}
	user := evmDID(depAddr)
	balKey := "a-" + user + "-eth"
	if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr(balKey) == "5940000000000000000" }); err != nil {
		t.Fatalf("deposit never mapped: %s=%q\nbot:\n%s", balKey, e.stateStr(balKey), botLogs())
	}
	if err := e.fundDID(user, "10000.000"); err != nil {
		t.Fatalf("fund user DID: %v", err)
	}
	K, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))
	if err != nil {
		t.Fatal(err)
	}

	// withdraw queues a 0.1 ETH withdrawal at nonce n and returns when it is queued.
	withdraw := func(n uint64) {
		t.Helper()
		recip := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
		unmap := fmt.Sprintf(`{"to":"%s","amount":"100000000000000000","asset":"eth"}`, recip.Hex())
		if _, err := e.d.CallContractEvm(ctx, e.gqlNode, K, e.amID, "unmapETH", unmap, 5000000); err != nil {
			t.Fatalf("unmapETH %d: %v", n, err)
		}
		key := fmt.Sprintf("d-%d", n)
		var ps struct {
			UnsignedTxHex string `json:"unsigned_tx_hex"`
		}
		if err := pollUntil(ctx, evmL2Wait, func() bool {
			return json.Unmarshal([]byte(e.stateStr(key)), &ps) == nil && ps.UnsignedTxHex != ""
		}); err != nil {
			t.Fatalf("withdrawal %d not queued (%s=%q)", n, key, e.stateStr(key))
		}
	}
	mined := func(n uint64) bool {
		_, _, found, ferr := anvil.FindTxFrom(ctx, vault, n, depBlock)
		return ferr == nil && found
	}
	settle := func(n uint64) { // the bot confirms it once its header lands
		t.Helper()
		_, blk, found, _ := anvil.FindTxFrom(ctx, vault, n, depBlock)
		if !found {
			return
		}
		if err := e.plantFinalized(anvil, blk); err != nil {
			t.Fatalf("plant withdrawal %d header: %v", n, err)
		}
		want := fmt.Sprint(n + 1)
		if err := pollUntil(ctx, 6*time.Minute, func() bool { return e.stateStr("n") == want }); err != nil {
			t.Fatalf("withdrawal %d never settled (n=%q)\nbot:\n%s", n, e.stateStr("n"), botLogs())
		}
	}

	// CONTROL.
	t0 := devnetNow()
	withdraw(0)
	if err := pollUntil(ctx, 20*time.Minute, func() bool { return mined(0) }); err != nil {
		t.Fatalf("CONTROL: withdrawal 0 never mined on a healthy fleet\nbot:\n%s", botLogs())
	}
	control := devnetNow().Sub(t0)
	settle(0)
	t.Logf("CONTROL PASS: healthy fleet, withdrawal 0 signed and mined in %s", control.Round(time.Second))

	// FAULT: one committee node that is neither the owner (1) nor the test's GraphQL node.
	faulted := 2
	if e.gqlNode == 2 {
		faulted = 3
	}
	healthy := e.gqlNode
	hold := 3 * control
	if hold < 10*time.Minute {
		hold = 10 * time.Minute
	}
	closedBefore := vfCountLogs(d, ctx, faulted, cg1FailClosedLog)
	faultBefore := vfCountLogs(d, ctx, faulted, cg1FaultLog)
	startBefore := vfCountLogs(d, ctx, faulted, cg1SignStartLog)
	cg1Failpoint(t, d, ctx, faulted, true)
	cg1VerifyFailpoint(t, d, ctx, faulted, healthy)
	t1 := devnetNow()
	withdraw(1)
	err = pollUntil(ctx, hold, func() bool { return mined(1) })
	during := devnetNow().Sub(t1)
	closed := vfCountLogs(d, ctx, faulted, cg1FailClosedLog) - closedBefore
	faultLogs := vfCountLogs(d, ctx, faulted, cg1FaultLog) - faultBefore
	starts := vfCountLogs(d, ctx, faulted, cg1SignStartLog) - startBefore
	t.Logf("FAULT (node %d, persistent): withdrawal 1 mined=%v after %s (hold %s); faulted node: fail-closed logs %d, read-fault logs %d, sign sessions started %d",
		faulted, err == nil, during.Round(time.Second), hold.Round(time.Second), closed, faultLogs, starts)
	if starts == 0 && closed == 0 && faultLogs == 0 {
		t.Fatalf("FAULT: the faulted node neither signed nor refused nor logged a read fault: it was not in the signing session, nothing was tested")
	}
	if cg1Fixed() {
		if err != nil {
			t.Fatalf("FAULT (fixed): withdrawal 1 was not signed within %s while one node's reads fail\nbot:\n%s", hold, botLogs())
		}
		if closed != 0 {
			t.Fatalf("FAULT (fixed): the faulted node logged the fail-closed halt read %d times", closed)
		}
		if faultLogs == 0 {
			t.Fatalf("FAULT (fixed): the faulted node never logged the read fault: its EVM sign gate did not run, nothing was tested")
		}
		if starts == 0 {
			t.Fatalf("FAULT (fixed): the faulted node started no signing session: it was not tested")
		}
		settle(1)
		t.Logf("FAULT PASS (fixed): signed and mined in %s with node %d's contract_state reads failing (control %s)", during.Round(time.Second), faulted, control.Round(time.Second))
	} else {
		if err == nil {
			t.Fatalf("FAULT (pre-fix): withdrawal 1 was signed while node %d's reads fail; the veto did not reproduce (fail-closed logs %d)", faulted, closed)
		}
		if closed == 0 {
			t.Fatalf("FAULT (pre-fix): withdrawal 1 stalled but the faulted node never logged the fail-closed halt read: another cause")
		}
		t.Logf("FAULT REPRODUCED (pre-fix): withdrawal 1 not signed for %s while node %d's reads fail; it refused %d times", hold.Round(time.Second), faulted, closed)
	}

	// RECOVER.
	cg1Failpoint(t, d, ctx, faulted, false)
	t2 := devnetNow()
	if cg1Fixed() {
		withdraw(2)
		if err := pollUntil(ctx, 20*time.Minute, func() bool { return mined(2) }); err != nil {
			t.Fatalf("RECOVER (fixed): withdrawal 2 never mined after the fault cleared\nbot:\n%s", botLogs())
		}
		t.Logf("RECOVER PASS (fixed): withdrawal 2 mined in %s after the fault", devnetNow().Sub(t2).Round(time.Second))
		return
	}
	if err := pollUntil(ctx, 20*time.Minute, func() bool { return mined(1) }); err != nil {
		t.Fatalf("RECOVER (pre-fix): withdrawal 1 never mined after the fault cleared\nbot:\n%s", botLogs())
	}
	t.Logf("RECOVER (pre-fix): withdrawal 1 mined %s after the fault cleared; stalled %s in all",
		devnetNow().Sub(t2).Round(time.Second), devnetNow().Sub(t1).Round(time.Second))
}
