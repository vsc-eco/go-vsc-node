//go:build evm_devnet

package devnet

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"go.mongodb.org/mongo-driver/bson"
)

// C1 (consensus 0.13.0) on a devnet of its own: the EVM bridge's vault key is
// held by all five members; two of them stand down. With 5 holders the key
// needs 4 signers and only 3 are left in the election, so the key signs only if
// the departed holders stay readiness-eligible. Then the key is handed to the 3
// remaining members, and signs again with the departed nodes offline.
//
// Both arms run the same binaries at consensus 0.13.0. EVM_CUSTODY_AB=off is the
// A/B arm: the nodes are not told the bridge's contract id
// (OracleParams.EvmBridgeContracts), so the 0.13.0 rule has no key to cover, the
// departed holders are not eligible, and the key must NOT sign.
// Run with EVM_CUSTODY_RUN=1 (own 5-node devnet, ~1.5 h).
func TestEVMBridge_C1_VaultKeyHoldersOutsideTheElection(t *testing.T) {
	if testing.Short() || os.Getenv("EVM_CUSTODY_RUN") == "" {
		t.Skip("set EVM_CUSTODY_RUN=1")
	}
	ruleOn := os.Getenv("EVM_CUSTODY_AB") != "off"
	cfg := evmBridgeConfig()
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 13, 1
	if cfg.SysConfigOverrides.TssParams == nil {
		cfg.SysConfigOverrides.TssParams = &params.TssParams{}
	}
	cfg.SysConfigOverrides.TssParams.RotateInterval = 60
	d, ctx := startDevnetNoKey(t, cfg, 150*time.Minute)
	if err := bootstrapEvmOwner(ctx, d); err != nil {
		t.Fatal(err)
	}
	e := setupEvmBridgeOn(t, ctx, d)
	eth := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

	// Every node learns the bridge's contract id (consensus config), then restarts.
	if d.cfg.SysConfigOverrides == nil {
		d.cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{}
	}
	if d.cfg.SysConfigOverrides.OracleParams == nil {
		d.cfg.SysConfigOverrides.OracleParams = &params.OracleParams{}
	}
	if ruleOn {
		d.cfg.SysConfigOverrides.OracleParams.EvmBridgeContracts = map[string]string{"ETH": e.amID}
		d.cfg.SysConfigOverrides.OracleParams.EvmBridgeFromHeight = map[string]uint64{"ETH": 0}
	}
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		t.Fatal(err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 5*time.Minute, func() bool { _, _, err := d.LocalNodeInfo(ctx, e.gqlNode); return err == nil }); err != nil {
		t.Fatal("nodes did not come back after the config restart")
	}
	// Precondition: the chain runs 0.13.0 in both arms.
	var ver string
	if err := pollUntil(ctx, 10*time.Minute, func() bool {
		var out struct {
			LocalNodeInfo struct {
				V string `json:"consensus_version_display"`
			} `json:"localNodeInfo"`
		}
		if err := d.gqlQuery(ctx, 2, `{ localNodeInfo { consensus_version_display } }`, nil, &out); err != nil {
			return false
		}
		ver = out.LocalNodeInfo.V
		return strings.HasPrefix(ver, "0.13.")
	}); err != nil {
		t.Fatalf("PRECONDITION FAILED: chain-active version %q, want 0.13.x", ver)
	}

	anvil := StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(anvil.Stop)
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, e.ownerAcct)); err != nil {
		t.Fatal(err)
	}
	vault, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	keyId := e.amID + "-primary"
	keyEpoch, keygenEpoch, _ := custodyEpochs(d, ctx, 2, keyId)
	t.Logf("chain %s, EVM custody rule wired=%v; vault key %s epoch=%d keygen=%d; vault %s", ver, ruleOn, keyId, keyEpoch, keygenEpoch, vault.Hex())
	_ = anvil.SetBalance(ctx, vault, eth(100))

	depKey, _ := ethCrypto.GenerateKey()
	did, err := e.depositETH(anvil, depKey, vault, eth(6))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := e.fundDID(did, "10000.000"); err != nil {
		t.Fatal(err)
	}
	K, _ := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(depKey)))

	// Two holders stand down; their nodes stay up.
	departed := []int{4, 5}
	e0 := custodyCurrentEpoch(d, ctx, 2)
	for _, n := range departed {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), cfg.Nodes)
	}
	var members []string
	out := false
	for end := devnetNow().Add(20 * time.Minute); devnetNow().Before(end) && !out; devnetSleep(20 * time.Second) {
		ep := custodyCurrentEpoch(d, ctx, 2)
		if ep <= e0 {
			continue
		}
		members, _ = d.GetElectionMembers(ctx, 2, ep)
		out = len(members) == 3 && !contains(members, d.witnessAccount(4)) && !contains(members, d.witnessAccount(5))
	}
	if !out {
		t.Fatalf("PRECONDITION FAILED: the departing holders never left the election (members=%v)", members)
	}
	t.Logf("election now %v; magi-4 and magi-5 hold vault-key shares from outside it", members)

	// SIGN: 4 of the 5 holders must sign, 3 are in the election.
	_, ps := evmCustodyWithdraw(t, e, K, 10*time.Minute)
	signed, err := e.tssSign(ps.UnsignedTxHex, vault, 20*time.Minute)
	if !ruleOn {
		if err == nil {
			t.Fatal("A/B: the vault key signed with only 3 holders in the election and the rule unwired; the arm must freeze")
		}
		t.Logf("A/B PASS (rule unwired): the vault key cannot sign with 2 of its 5 holders outside the election: %v", err)
		return
	}
	if err != nil {
		t.Fatalf("C1-SIGN: the vault key did not sign with the departed holders eligible: %v", err)
	}
	t.Logf("C1-SIGN PASS: the vault key signed a withdrawal with only 3 of its 5 holders in the election")
	evmCustodySettle(t, e, anvil, ps, signed)

	// HANDOVER: a reshare to the 3 remaining members.
	handed := false
	for end := devnetNow().Add(25 * time.Minute); devnetNow().Before(end) && !handed; devnetSleep(20 * time.Second) {
		cs, _ := d.GetCommitments(ctx, 2, bson.M{"key_id": keyId, "type": "reshare", "epoch": bson.M{"$gt": e0}})
		for _, cm := range cs {
			if names, err := d.GetElectionMembers(ctx, 2, cm.Epoch); err == nil && len(names) == 3 {
				handed = true
				t.Logf("C1-HANDOVER PASS: reshare commitment at epoch %d, committee %v", cm.Epoch, names)
			}
		}
	}
	if !handed {
		t.Fatal("C1-HANDOVER: the vault key was never reshared to the 3 remaining members")
	}

	// AFTER: the departed holders go offline; the key signs with the 3.
	vfStopNodes(t, d, ctx, departed)
	defer vfStartNodes(t, d, ctx, departed)
	_, ps2 := evmCustodyWithdraw(t, e, K, 10*time.Minute)
	signed2, err := e.tssSign(ps2.UnsignedTxHex, vault, 20*time.Minute)
	if err != nil {
		t.Fatalf("C1-AFTER: no signature with the departed holders offline: %v", err)
	}
	evmCustodySettle(t, e, anvil, ps2, signed2)
	t.Logf("C1-AFTER PASS: after the handover the vault key signs with the departed holders offline")
}

type evmCustodySpend struct {
	Nonce         uint64   `json:"nonce"`
	Amount        *big.Int `json:"amount"`
	To            string   `json:"to"`
	Asset         string   `json:"asset"`
	UnsignedTxHex string   `json:"unsigned_tx_hex"`
}

// evmCustodyWithdraw queues a 0.1 ETH unmapETH from K and returns the queued spend.
func evmCustodyWithdraw(t *testing.T, e *evmBridgeEnv, K *EthKey, within time.Duration) (string, evmCustodySpend) {
	t.Helper()
	n := e.stateStr("n")
	if n == "" {
		n = "0"
	}
	recipKey, _ := ethCrypto.GenerateKey()
	recip := ethCrypto.PubkeyToAddress(recipKey.PublicKey)
	tx, err := e.d.CallContractEvm(e.ctx, e.gqlNode, K, e.amID, "unmapETH",
		fmt.Sprintf(`{"to":"%s","amount":"100000000000000000","asset":"eth"}`, recip.Hex()), 5000000)
	if err != nil {
		t.Fatalf("unmapETH: %v", err)
	}
	var ps evmCustodySpend
	if err := pollUntil(e.ctx, within, func() bool {
		s := e.stateStr("d-" + n)
		return s != "" && json.Unmarshal([]byte(s), &ps) == nil && ps.UnsignedTxHex != ""
	}); err != nil {
		t.Fatalf("unmapETH (tx %s) queued no withdrawal at nonce %s", tx, n)
	}
	return tx, ps
}

// evmCustodySettle broadcasts the signed withdrawal, plants its block and runs
// confirmSpend, so the next withdrawal can queue.
func evmCustodySettle(t *testing.T, e *evmBridgeEnv, anvil *Anvil, ps evmCustodySpend, signed *ethtypes.Transaction) {
	t.Helper()
	raw, _ := signed.MarshalBinary()
	h, blockN, err := anvil.SendRawTx(e.ctx, raw)
	if err != nil {
		t.Fatalf("withdrawal broadcast: %v", err)
	}
	if err := e.plantFinalized(anvil, blockN); err != nil {
		t.Fatal(err)
	}
	idx, err := anvil.FindTxIndex(e.ctx, blockN, h)
	if err != nil {
		t.Fatal(err)
	}
	_, txProofHex, err := anvil.TxTrieProof(e.ctx, blockN, idx)
	if err != nil {
		t.Fatal(err)
	}
	_, rcptProofHex, err := anvil.ReceiptTrieProof(e.ctx, blockN, idx)
	if err != nil {
		t.Fatal(err)
	}
	txB64, _ := b64Proof(txProofHex)
	rcptB64, _ := b64Proof(rcptProofHex)
	want := strconv.FormatUint(ps.Nonce+1, 10)
	cs := fmt.Sprintf(`{"block_height":%d,"tx_index":%d,"tx_proof":"%s","receipt_proof":"%s","intent_nonce":%d,"intent_to":"%s","intent_amount":"%s","intent_asset":"%s"}`,
		blockN, idx, txB64, rcptB64, ps.Nonce, ps.To, ps.Amount.String(), ps.Asset)
	if _, err := e.callOnce(e.ownerNode, e.amID, "confirmSpend", cs); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(e.ctx, 5*time.Minute, func() bool { return e.stateStr("n") == want }); err != nil {
		t.Fatalf("confirmSpend did not close nonce %d (n=%q)", ps.Nonce, e.stateStr("n"))
	}
}
