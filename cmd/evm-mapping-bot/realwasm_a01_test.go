//go:build evm_realwasm

package main

// A01 on the bridge wasm, end to end, with the bot's own readers and
// builders: a rotation with a withdrawal in flight at the cut-over, the
// one-batch sweep (ETH first, then the token), the nonce hand-off, the first
// withdrawal from the new key, the leftover sweep, retire and purge. Every
// call's RC is logged and checked against the bot's sizing.
//
//	EVM_MAPPING_WASM=~/evm-fix/account-mapping/evm-mapping-contract/bin/a01-5aa018d.wasm \
//	  go test -tags evm_realwasm ./cmd/evm-mapping-bot/ -run RealWasm_A01 -v

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// rwRC records the RC each bridge call used, by action.
type rwRC struct {
	t    *testing.T
	ct   *test_utils.ContractTest
	used map[string][]int64
}

// call runs action as caller with a 1M rc_limit (above any op) and requires
// it to succeed.
func (r *rwRC) call(caller, action string, payload []byte) test_utils.ContractTestCallResult {
	r.t.Helper()
	res := rwCallLimit(r.ct, caller, action, payload, 1_000_000)
	if !res.Success {
		r.t.Fatalf("%s: %s: %s", action, res.Err, res.ErrMsg)
	}
	r.used[action] = append(r.used[action], res.RcUsed)
	return res
}

// proofList collects trie.Prove's nodes in order, root first.
type proofList [][]byte

func (p *proofList) Put(_ []byte, v []byte) error {
	*p = append(*p, append([]byte{}, v...))
	return nil
}
func (p *proofList) Delete([]byte) error { return nil }

// rwStateProof builds a state trie holding addr (nonce, balance) among other
// accounts and serves eth_getProof for it, as an archive RPC would. It returns
// the RPC and the state root.
func rwStateProof(t *testing.T, addr common.Address, nonce uint64, balance *big.Int) (*ethRPC, common.Hash) {
	t.Helper()
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	put := func(a common.Address, n uint64, b *big.Int) {
		enc, _ := gethrlp.EncodeToBytes(&types.StateAccount{Nonce: n, Balance: uint256.MustFromBig(b), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()})
		tr.MustUpdate(ethCrypto.Keccak256(a.Bytes()), enc)
	}
	for i := 1; i <= 40; i++ { // neighbours: a multi-node proof
		put(common.BigToAddress(big.NewInt(int64(i*7919))), uint64(i), big.NewInt(int64(i)*1e15))
	}
	put(addr, nonce, balance)
	root := tr.Hash()
	var proof proofList
	if err := tr.Prove(ethCrypto.Keccak256(addr.Bytes()), &proof); err != nil {
		t.Fatal(err)
	}
	nodes := make([]string, len(proof))
	for i, n := range proof {
		nodes[i] = hexutil.Encode(n)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"nonce": hexutil.EncodeUint64(nonce), "balance": hexutil.EncodeBig(balance), "accountProof": nodes}})
	}))
	t.Cleanup(srv.Close)
	return newEthRPC(srv.URL), root
}

// rwAddSigned appends an already-signed tx to c with a receipt of status.
func rwAddSigned(c *fakeChain, tx *types.Transaction, status uint64, logs ...*types.Log) {
	prior := 0
	for _, rc := range c.receipts {
		prior += len(rc.Logs)
	}
	for i, l := range logs {
		l.TxHash, l.TxIndex, l.Index = tx.Hash(), uint(len(c.txs)), uint(prior+i)
	}
	rc := &types.Receipt{Type: types.DynamicFeeTxType, Status: status, CumulativeGasUsed: uint64(60000 * (len(c.receipts) + 1)),
		Logs: logs, TxHash: tx.Hash(), TransactionIndex: uint(len(c.txs))}
	rc.Bloom = types.CreateBloom(types.Receipts{rc})
	c.txs, c.receipts = append(c.txs, tx), append(c.receipts, rc)
}

// rwSignSweep signs an open sweep version the way the TSS group does (DER)
// and attaches it with the bot's signedSweep, checking the bot's own
// successor guard on the way.
func rwSignSweep(t *testing.T, key *ecdsa.PrivateKey, unsignedHex string, from, successor common.Address) *types.Transaction {
	t.Helper()
	if !sweepTxPaysOnly(unsignedHex, successor) {
		t.Fatalf("the bot would refuse the bridge's sweep %s", unsignedHex)
	}
	digest, _ := computeSighash(unsignedHex)
	h, _ := hex.DecodeString(digest)
	sig, _ := ethCrypto.Sign(h, key)
	signed, err := signedSweep(unsignedHex, TssSignature{Bytes: derSig(sig, false)}, strings.ToLower(from.Hex()))
	if err != nil {
		t.Fatal(err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(hexToBytes(signed)); err != nil {
		t.Fatal(err)
	}
	return &tx
}

func rwSweep(t *testing.T, ct *test_utils.ContractTest, gen uint32, nonce uint64) sweepRecord {
	t.Helper()
	var r sweepRecord
	k := fmt.Sprintf("ms-%d-%d", gen, nonce)
	if err := json.Unmarshal([]byte(ct.StateGet(rwBridge, k)), &r); err != nil || len(r.Versions) == 0 {
		t.Fatalf("%s = %q", k, ct.StateGet(rwBridge, k))
	}
	return r
}

func rwView(t *testing.T, ct *test_utils.ContractTest) vaultView {
	t.Helper()
	v, err := decodeVaultView(rwBridge, hex.EncodeToString([]byte(ct.StateGet(rwBridge, "v"))))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRealWasm_A01_RotationEndToEnd(t *testing.T) {
	ct := rwContract(t)
	ct.VaultRotationV2, ct.KeyReshared = true, true
	rc := &rwRC{t: t, ct: ct, used: map[string][]int64{}}
	key0, vault0 := rwVaultKey(t, ct)
	c0 := rwChain{ct: ct, vaultKey: key0, vault: vault0}
	if msg, ok := rwRegisterToken(t, c0, rwUSDC, "usdc"); !ok {
		t.Fatalf("registerToken: %s", msg)
	}
	for _, who := range []string{"hive:owner", rwRelayer} {
		ct.Deposit(who, 5_000_000, ledgerDb.AssetHbd)
	}
	ct.StateSet(rwBridge, "rl-"+rwRelayer, "1")
	rc.call("hive:owner", "setVaultOperator", []byte(rwRelayer))

	// Deposits to generation 0: 25 USDC (block 101), 6 ETH (block 102).
	chain, dep := erc20DepositFixture(t, vault0, big.NewInt(25_000_000))
	rwHeaderAt(ct, 101, chain, rwBlockTime)
	rc.call(rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), chain.rpc(t), dep, dep.BlockHeight))
	userKey, _ := ethCrypto.GenerateKey()
	user := rwDID(ethCrypto.PubkeyToAddress(userKey.PublicKey))
	ct.Deposit(user, 5_000_000, ledgerDb.AssetHbd)
	b102 := &fakeChain{}
	six := new(big.Int).Mul(big.NewInt(6), big.NewInt(1e18))
	ethTx := b102.addTx(t, userKey, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault0, Value: six})
	rwHeaderAt(ct, 102, b102, rwBlockTime)
	rc.call(rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), b102.rpc(t), detectedDeposit{BlockHeight: 102, TxIndex: 0, LogIndex: -1, TxHash: ethTx.Hash().Hex(), DepositType: "eth"}, 102))

	// A withdrawal from generation 0, in flight at the cut-over.
	rc.call(user, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e2","amount":"1000000000000000000","asset":"eth"}`))
	inflight := parsePendingSpend(0, ct.StateGet(rwBridge, "d-0"))
	if inflight == nil || !strings.EqualFold(inflight.VaultAtQueue, vault0.Hex()) {
		t.Fatalf("generation 0 withdrawal %+v", inflight)
	}

	// The rotation: keygen, check signature, reshare (TssGetKey fields 4 and 5).
	rc.call("hive:owner", "createKey", nil)
	key1, _ := ethCrypto.GenerateKey()
	vault1 := ethCrypto.PubkeyToAddress(key1.PublicKey)
	pub1 := hex.EncodeToString(ethCrypto.CompressPubkey(&key1.PublicKey))
	ct.Tss.Keys.SetKey(tss_db.TssKey{Id: rwBridge + "-primaryv1", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType, PublicKey: pub1, SignatureVerified: true, Epoch: 2})
	ct.Tss.Commitments.SetCommitmentData(tss_db.TssCommitment{Type: "keygen", KeyId: rwBridge + "-primaryv1", Epoch: 1, PublicKey: &pub1})
	if r := rc.call("hive:owner", "activateKey", nil); !strings.Contains(r.Ret, "activated generation 1") {
		t.Fatalf("activateKey with a withdrawal in flight: %q", r.Ret)
	}
	v := rwView(t, ct)
	if v.Active == nil || v.Active.Gen != 1 || v.Active.Address != strings.ToLower(vault1.Hex()) || v.Gens[0].Status != btcvault.VaultStatusRetiring {
		t.Fatalf("view after the cut-over %+v", v)
	}
	if ct.StateGet(rwBridge, "wnh") != "0" || ct.StateGet(rwBridge, "wnc-0") != "1" {
		t.Fatalf("hand-off state wnh=%q wnc-0=%q", ct.StateGet(rwBridge, "wnh"), ct.StateGet(rwBridge, "wnc-0"))
	}

	// A second withdrawal waits for the hand-off (and the sweep).
	r := rc.call(user, "unmapETH", []byte(`{"to":"0x00000000000000000000000000000000000000e3","amount":"500000000000000000","asset":"eth"}`))
	if !strings.HasPrefix(r.Ret, "queued:") {
		t.Fatalf("withdrawal during the hand-off: %q", r.Ret)
	}

	// Generation 0's withdrawal mines (block 103) and settles: generation 0
	// signed it, as the cut-over left it to.
	ch103, tx103 := rwMine(t, c0, inflight, 103, types.ReceiptStatusSuccessful)
	payload, err := buildConfirmSpendPayload(context.Background(), ch103.rpc(t), tx103.Hash().Hex(), 103, 0, inflight)
	if err != nil {
		t.Fatal(err)
	}
	rc.call(rwRelayer, "confirmSpend", payload)

	// A late 0.5 ETH deposit to generation 0's address (block 104): credited.
	b104 := &fakeChain{}
	half := big.NewInt(5e17)
	lateTx := b104.addTx(t, userKey, &types.DynamicFeeTx{Nonce: 1, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault0, Value: half})
	rwHeaderAt(ct, 104, b104, rwBlockTime)
	rc.call(rwRelayer, "map", buildMapPayloadFromRPC(context.Background(), b104.rpc(t), detectedDeposit{BlockHeight: 104, TxIndex: 0, LogIndex: -1, TxHash: lateTx.Hash().Hex(), DepositType: "eth"}, 104))

	// The batch: the bot proves generation 0's balance (nonce 1, after its
	// withdrawal) at block 105 and migrateVault builds ETH then USDC.
	if res := rwCallLimit(ct, rwRelayer, "migrateVault", nil, 1_000_000); res.Success || !strings.Contains(res.ErrMsg, "eth_proof") {
		t.Fatalf("migrateVault without the proof: %v %s", res.Success, res.ErrMsg)
	}
	book, _ := new(big.Int).SetString(ct.StateGet(rwBridge, "vb-0-eth"), 10)
	balance := new(big.Int).Add(book, big.NewInt(1e15)) // and 0.001 ETH stray
	proofRPC, stateRoot := rwStateProof(t, vault0, 1, balance)
	rwHeaderState(ct, 105, stateRoot, &fakeChain{}, rwBlockTime)
	proof, nonce, _, err := fetchAccountProof(context.Background(), proofRPC, vault0.Hex(), 105)
	if err != nil || nonce != 1 {
		t.Fatalf("fetchAccountProof: nonce %d %v", nonce, err)
	}
	mp, _ := json.Marshal(map[string]any{"eth_proof": proof})
	r = rc.call(rwRelayer, "migrateVault", mp)
	if !strings.HasPrefix(r.Ret, "sweep 0-1 eth ") || !strings.Contains(r.Ret, "; sweep 0-2 usdc 25000000") {
		t.Fatalf("batch: %q", r.Ret)
	}
	ethSweep, usdcSweep := rwSweep(t, ct, 0, 1), rwSweep(t, ct, 0, 2)

	// Both mine in block 106; the bot confirms each.
	ch106 := &fakeChain{}
	rwAddSigned(ch106, rwSignSweep(t, key0, ethSweep.Versions[0].Unsigned, vault0, vault1), types.ReceiptStatusSuccessful)
	usdc := common.HexToAddress(rwUSDC)
	transfer := &types.Log{Address: usdc, Topics: []common.Hash{ethCrypto.Keccak256Hash([]byte("Transfer(address,address,uint256)")),
		common.BytesToHash(vault0.Bytes()), common.BytesToHash(vault1.Bytes())}, Data: common.LeftPadBytes(big.NewInt(25_000_000).Bytes(), 32)}
	rwAddSigned(ch106, rwSignSweep(t, key0, usdcSweep.Versions[0].Unsigned, vault0, vault1), types.ReceiptStatusSuccessful, transfer)
	rwHeaderAt(ct, 106, ch106, rwBlockTime)
	for i, s := range []sweepRecord{ethSweep, usdcSweep} {
		p, err := buildConfirmSweepPayload(context.Background(), ch106.rpc(t), s.Gen, s.Nonce, 106, i)
		if err != nil {
			t.Fatal(err)
		}
		rc.call("hive:anyone", "confirmSweep", p)
	}
	if ct.StateGet(rwBridge, "msr") != "" || ct.StateGet(rwBridge, "vb-0-usdc") != "" && ct.StateGet(rwBridge, "vb-0-usdc") != "0" {
		t.Fatalf("after the batch: msr=%q usdc book %q", ct.StateGet(rwBridge, "msr"), ct.StateGet(rwBridge, "vb-0-usdc"))
	}

	// A sweep is a transfer between two of the vault's own addresses, not a
	// deposit: mapping it (mapping is permissionless) must not credit
	// anyone or add supply (devnet weather wave: the ETH sweep was mapped and
	// the books ran 6 ETH over custody).
	supplyEth, supplyUsdc := ct.StateGet(rwBridge, "s-eth"), ct.StateGet(rwBridge, "s-usdc")
	ethSweepDep := detectedDeposit{BlockHeight: 106, TxIndex: 0, LogIndex: -1, TxHash: ch106.txs[0].Hash().Hex(), DepositType: "eth"}
	if r := rwCall(ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), ch106.rpc(t), ethSweepDep, 106)); r.Success {
		t.Errorf("the ETH sweep was accepted as a deposit: s-eth %q -> %q, credited %q", supplyEth, ct.StateGet(rwBridge, "s-eth"),
			ct.StateGet(rwBridge, "a-"+rwDID(vault0)+"-eth"))
	}
	usdcSweepDep := detectedDeposit{BlockHeight: 106, TxIndex: 1, LogIndex: 0, TxHash: ch106.txs[1].Hash().Hex(), DepositType: "erc20", TokenAddress: rwUSDC}
	if r := rwCall(ct, "hive:anyone", "map", buildMapPayloadFromRPC(context.Background(), ch106.rpc(t), usdcSweepDep, 106)); r.Success {
		t.Errorf("the USDC sweep was accepted as a deposit: s-usdc %q -> %q, credited %q", supplyUsdc, ct.StateGet(rwBridge, "s-usdc"),
			ct.StateGet(rwBridge, "a-"+rwDID(vault0)+"-usdc"))
	}
	if ct.StateGet(rwBridge, "s-eth") != supplyEth || ct.StateGet(rwBridge, "s-usdc") != supplyUsdc {
		t.Fatalf("supply moved by mapping a sweep: eth %q -> %q, usdc %q -> %q", supplyEth, ct.StateGet(rwBridge, "s-eth"), supplyUsdc, ct.StateGet(rwBridge, "s-usdc"))
	}

	// Hand-off: the queued withdrawal issues from generation 1 at nonce 0,
	// signed by its key; it mines and settles.
	rc.call(rwRelayer, "drainWithdrawalQueue", []byte(`{"max":8}`))
	first := parsePendingSpend(0, ct.StateGet(rwBridge, "d-0"))
	if first == nil || !strings.EqualFold(first.VaultAtQueue, vault1.Hex()) || ct.StateGet(rwBridge, "wnh") != "" {
		t.Fatalf("first withdrawal of generation 1: %+v (wnh %q)", first, ct.StateGet(rwBridge, "wnh"))
	}
	ch107, tx107 := rwMine(t, rwChain{ct: ct, vaultKey: key1, vault: vault1}, first, 107, types.ReceiptStatusSuccessful)
	if payload, err = buildConfirmSpendPayload(context.Background(), ch107.rpc(t), tx107.Hash().Hex(), 107, 0, first); err != nil {
		t.Fatal(err)
	}
	rc.call(rwRelayer, "confirmSpend", payload)

	// The gas kept back is left over: swept with a proof at nonce 3.
	left, _ := new(big.Int).SetString(ct.StateGet(rwBridge, "vb-0-eth"), 10)
	if left == nil || left.Sign() <= 0 {
		t.Fatalf("leftover eth book %q", ct.StateGet(rwBridge, "vb-0-eth"))
	}
	proofRPC, stateRoot = rwStateProof(t, vault0, 3, left)
	rwHeaderState(ct, 108, stateRoot, &fakeChain{}, rwBlockTime)
	proof, _, _, _ = fetchAccountProof(context.Background(), proofRPC, vault0.Hex(), 108)
	mp, _ = json.Marshal(map[string]any{"eth_proof": proof})
	if r = rc.call(rwRelayer, "migrateVault", mp); !strings.HasPrefix(r.Ret, "sweep 0-3 eth ") || strings.Contains(r.Ret, ";") {
		t.Fatalf("leftover sweep: %q", r.Ret)
	}
	lastSweep := rwSweep(t, ct, 0, 3)
	ch109 := &fakeChain{}
	rwAddSigned(ch109, rwSignSweep(t, key0, lastSweep.Versions[0].Unsigned, vault0, vault1), types.ReceiptStatusSuccessful)
	rwHeaderAt(ct, 109, ch109, rwBlockTime)
	p, _ := buildConfirmSweepPayload(context.Background(), ch109.rpc(t), 0, 3, 109, 0)
	rc.call("hive:anyone", "confirmSweep", p)

	// Retire, then purge after the grace (64 L1 blocks off mainnet).
	if r = rc.call(rwRelayer, "retireVault", nil); !strings.Contains(r.Ret, "inactivated=0") {
		t.Fatalf("retire: %q", r.Ret)
	}
	rwHeaderAt(ct, 109+65, &fakeChain{}, rwBlockTime)
	if r = rc.call(rwRelayer, "retireVault", nil); !strings.Contains(r.Ret, "purged=0") {
		t.Fatalf("purge: %q", r.Ret)
	}
	v = rwView(t, ct)
	if fh := v.fundHolding(); len(fh) != 1 || fh[0] != strings.ToLower(vault1.Hex()) {
		t.Fatalf("fund-holding after the purge %v", fh)
	}

	// RC per call: logged, and within what the bot declares (2x the dry run,
	// at most vaultOpRcLimit) and the free-tier sizing the BTC bot uses.
	actions := make([]string, 0, len(rc.used))
	for a := range rc.used {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		max := int64(0)
		for _, u := range rc.used[a] {
			if u > max {
				max = u
			}
		}
		t.Logf("RC %-22s %v (bot declares %d)", a, rc.used[a], rcFromDryRun(max, vaultOpRcLimit))
		if uint64(max)*2 > vaultOpRcLimit {
			t.Errorf("%s used %d RC: twice that is above the bot's %d ceiling", a, max, vaultOpRcLimit)
		}
	}
}
