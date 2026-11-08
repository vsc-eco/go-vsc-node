package devnet

import (
	"encoding/base64"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// 18r devnet proof — the submitter broadcasts, then leaves.
//
// THE BUG. ReceiveTx (the p2p path) called indexTx and nothing else, while
// SubmitTx calls indexTx AND PutRaw. So on every node except the original
// submitter, a gossiped transaction existed in Mongo as UNCONFIRMED with no
// bytes behind its CID in the datalayer.
//
// That is invisible until the tx is anchored in a block. The apply path then
// resolves the CID through getDagOrBlock, which blocks with capped backoff
// rather than skip the entry -- skipping would let a node apply a different op
// set than its peers and fork its ledger -- and blockingRetry's loop is
// `for attempt := 1; ;`, so it never gives up. With the only holder of the
// bytes gone, every remaining node stops advancing. Permanently.
//
// WHY THIS NEEDS A DEVNET. The unit test in modules/transaction-pool proves the
// load-bearing claim (gossiped bytes hash to the CID the apply path requests),
// but it cannot stage "the submitter disconnects after broadcasting": that needs
// real gossip between real nodes and one of them actually leaving.
//
// WHAT THIS ASSERTS. Submit to node 1, stop node 1 before the tx is anchored,
// and require that the REMAINING nodes keep advancing and the tx reaches a
// terminal status. Pre-fix the network stalls here; post-fix the bytes are
// already on every node that received the gossip, so the block applies.
func TestTx18rSubmitterLeavesDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	requireDocker(t)

	cfg := DefaultConfig()
	cfg.Nodes = 4
	cfg.GenesisNode = 4
	cfg.LogLevel = "info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 20,
		},
	}

	d, ctx := startDevnetNoKey(t, cfg, 25*time.Minute)
	t.Logf("18r: devnet running: GQL=%s", d.GQLEndpoint(1))

	// ── Phase 1: baseline — everyone advancing ─────────────────────────────
	heights := func() []uint64 {
		out := make([]uint64, cfg.Nodes)
		for n := 1; n <= cfg.Nodes; n++ {
			h, _, err := d.LocalNodeInfo(ctx, n)
			if err == nil {
				out[n-1] = h
			}
		}
		return out
	}

	base := heights()
	t.Logf("18r: baseline heights %v", base)

	// ── Phase 2: fund a fresh ETH DID so it can pay rc_limit ───────────────
	privKey, err := ethCrypto.GenerateKey()
	if err != nil {
		t.Fatalf("18r BLOCKED: generating ETH key: %v", err)
	}
	senderDID := dids.EthDIDPrefix + ethCrypto.PubkeyToAddress(privKey.PublicKey).Hex()
	witnessAcct := "hive:" + d.witnessAccount(1)
	t.Logf("18r: sender DID = %s", senderDID)

	waitForHbd := func(account string, min int64, timeout time.Duration) int64 {
		poll, cancel := devnetWithTimeout(ctx, timeout)
		defer cancel()
		var last int64 = -1
		for {
			if bal, berr := d.GetAccountBalance(poll, 1, account); berr == nil && bal != nil {
				last = bal.Hbd
				if bal.Hbd >= min {
					return bal.Hbd
				}
			}
			select {
			case <-poll.Done():
				return last
			case <-devnetAfter(5 * time.Second):
			}
		}
	}

	if _, err := d.Deposit(ctx, 1, "5.000", "hbd"); err != nil {
		t.Fatalf("18r BLOCKED: deposit HBD for witness-1: %v", err)
	}
	if got := waitForHbd(witnessAcct, 5000, 6*time.Minute); got < 5000 {
		t.Fatalf("18r BLOCKED: deposit never credited to %s (last hbd=%d)", witnessAcct, got)
	}

	// ledgerOp only takes bare hive accounts, and the recipient here is a DID,
	// so the payload is built explicitly.
	transferPayload := map[string]interface{}{
		"from":   witnessAcct,
		"to":     senderDID,
		"amount": "2.000",
		"asset":  "hbd",
		"net_id": d.netId(),
	}
	if _, err := d.BroadcastCustomJSON("vsc.transfer", []string{d.witnessAccount(1)}, transferPayload, d.cfg.InitminerWIF); err != nil {
		t.Fatalf("18r BLOCKED: transfer HBD to sender DID: %v", err)
	}
	if got := waitForHbd(senderDID, 2000, 6*time.Minute); got < 2000 {
		t.Fatalf("18r BLOCKED: sender DID never credited (last hbd=%d)", got)
	}
	t.Log("18r: sender DID funded — RC budget ready")

	// ── Phase 3: submit an L2 transfer to NODE 1 ONLY ──────────────────────
	//
	// A `transfer` op is handled by OffchainTransaction.ToTransaction, so this
	// is an ordinary, valid tx that will be included and applied. The point is
	// not what it does, it is that its BYTES must be resolvable by whichever
	// node applies the block.
	payload, err := common.EncodeDagCbor(map[string]interface{}{
		"from":   senderDID,
		"to":     witnessAcct,
		"amount": int64(1),
		"asset":  "hbd",
		"net_id": d.netId(),
	})
	if err != nil {
		t.Fatalf("18r BLOCKED: encoding transfer payload: %v", err)
	}

	tx := &transactionpool.VSCTransaction{
		Ops: []transactionpool.VSCTransactionOp{{
			Type:    "transfer",
			Payload: payload,
			RequiredAuths: struct {
				Active  []string
				Posting []string
			}{Active: []string{senderDID}},
		}},
		Nonce:   0,
		NetId:   d.netId(),
		RcLimit: 500,
	}

	serialized, err := tx.Serialize()
	if err != nil {
		t.Fatalf("18r BLOCKED: serializing tx: %v", err)
	}
	signable, err := tx.ToSignableBlock()
	if err != nil {
		t.Fatalf("18r BLOCKED: building signable block: %v", err)
	}
	sigStr, err := dids.NewEthProvider(privKey).Sign(signable)
	if err != nil {
		t.Fatalf("18r BLOCKED: signing tx: %v", err)
	}
	sigBytes, err := common.EncodeDagCbor(transactionpool.SignaturePackage{
		Type: "vsc-sig",
		Sigs: []common.Sig{{Algo: "eth", Kid: senderDID, Sig: sigStr}},
	})
	if err != nil {
		t.Fatalf("18r BLOCKED: encoding sig package: %v", err)
	}

	const gqlSubmit = `query($tx:String!,$sig:String!){submitTransactionV1(tx:$tx,sig:$sig){id}}`
	var submitOut struct {
		SubmitTransactionV1 *struct {
			Id *string `json:"id"`
		} `json:"submitTransactionV1"`
	}
	if err := d.gqlQuery(ctx, 1, gqlSubmit,
		map[string]any{
			"tx":  base64.StdEncoding.EncodeToString(serialized.Tx),
			"sig": base64.StdEncoding.EncodeToString(sigBytes),
		}, &submitOut); err != nil {
		t.Fatalf("18r BLOCKED: node-1 rejected the tx at submit: %v", err)
	}
	txCID := ""
	if submitOut.SubmitTransactionV1 != nil && submitOut.SubmitTransactionV1.Id != nil {
		txCID = *submitOut.SubmitTransactionV1.Id
	}
	if txCID == "" {
		t.Fatalf("18r BLOCKED: submit returned no CID")
	}
	t.Logf("18r: node-1 accepted tx CID=%s — gossiped to nodes 2..%d", txCID, cfg.Nodes)

	// ── Phase 3b: PRECONDITION — node 2 holds the tx from GOSSIP, unanchored ──
	//
	// Without this the test can pass while proving nothing. If node 2 never
	// received the gossip, there is nothing for it to stall on; if the tx was
	// already anchored and applied while node 1 was still up, node 1 could
	// serve the bytes and the vulnerable moment never happened. Either way
	// "the survivors kept advancing" would say nothing about the fix.
	// The state that exposes the bug is exactly: node 2 has the tx INDEXED
	// (UNCONFIRMED) and node 1 leaves before it is anchored.
	gossiped := false
	for i := 0; i < 40 && !gossiped; i++ {
		s, _ := d.FindTransactionStatus(ctx, 2, txCID)
		switch {
		case s == "UNCONFIRMED":
			gossiped = true
		case s != "":
			t.Fatalf("18r INCONCLUSIVE: tx was already %s on node-2 before node-1 left — "+
				"it was anchored too early, so this run never staged the vulnerable moment", s)
		default:
			devnetSleep(250 * time.Millisecond)
		}
	}
	if !gossiped {
		t.Fatalf("18r INCONCLUSIVE: node-2 never indexed tx %s from gossip within 10s — "+
			"without that there is nothing for the bug to act on", txCID)
	}
	t.Log("18r: precondition met — node-2 holds the tx from gossip, still UNCONFIRMED")

	// ── Phase 4: the submitter LEAVES, before the tx can be anchored ───────
	//
	// Node 1 is the only node that called PutRaw. Pre-fix, the bytes leave the
	// network with it.
	if err := d.StopNode(ctx, 1); err != nil {
		t.Fatalf("18r BLOCKED: stopping node 1: %v", err)
	}
	t.Log("18r: node-1 STOPPED — the only holder of the raw bytes (pre-fix) is gone")
	if s, _ := d.FindTransactionStatus(ctx, 2, txCID); s != "UNCONFIRMED" {
		t.Fatalf("18r INCONCLUSIVE: tx was %q on node-2 by the time node-1 stopped — "+
			"anchored while node-1 could still serve the bytes", s)
	}

	// ── Phase 5: the remaining nodes must keep advancing ───────────────────
	//
	// This is the assertion. Pre-fix they block forever in getDagOrBlock on a
	// CID no reachable peer can serve.
	pre := heights()
	t.Logf("18r: surviving-node heights at stop time: %v", pre)

	const settle = 90 * time.Second
	devnetSleep(settle)

	post := heights()
	t.Logf("18r: surviving-node heights after %s: %v", settle, post)

	stalled := make([]int, 0, cfg.Nodes)
	for n := 2; n <= cfg.Nodes; n++ {
		if post[n-1] <= pre[n-1] {
			stalled = append(stalled, n)
		}
	}
	if len(stalled) > 0 {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("18r FAIL: nodes %v stopped advancing after the submitter left "+
			"(before %v, after %v). This is the 18r stall: the tx was indexed from gossip "+
			"but its bytes were never stored, so the apply path blocks on a CID no "+
			"reachable peer can serve.", stalled, pre, post)
	}
	t.Logf("18r: all surviving nodes advanced — no stall")

	// And the tx itself must reach a terminal state on a node that never
	// submitted it, which can only happen if that node had the bytes.
	//
	// THIS is the decisive assertion, not the height check above. A control
	// run with the gossip-path PutRaw removed (2026-11-05) passed the height
	// check -- the survivors still crept 82 -> 91 in the window, against
	// 82 -> 112 with the fix, because the stall only begins when the block
	// carrying this tx comes up -- and then failed HERE: the tx never left
	// UNCONFIRMED. With the fix it reached a terminal state (FAILED on that
	// run: the op itself is refused at execution, which still requires the
	// bytes to decode it).
	deadline := devnetNow().Add(4 * time.Minute)
	status := ""
	for devnetNow().Before(deadline) {
		if s, err := d.FindTransactionStatus(ctx, 2, txCID); err == nil && s != "" && s != "UNCONFIRMED" {
			status = s
			break
		}
		devnetSleep(5 * time.Second)
	}
	if status == "" {
		t.Fatalf("18r FAIL: tx %s never left UNCONFIRMED on node-2 within the deadline — "+
			"the block carrying it was never applied", txCID)
	}
	t.Logf("18r PASS: tx reached %s on node-2 with the submitter offline", status)
}
