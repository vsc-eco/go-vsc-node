package main

// The 10-27 launch fixes, bot side:
//   RP-1  a fresh checkpoint starts at START_BLOCK or the verifier's tip, never block 1
//   RP-5  the L1 RPC must serve the chain the bridge is configured for
//   RP-6  a mined withdrawal left unconfirmed past CONFIRM_LAG_ALARM is an alarm
//   RP-7  MagiDeposit deposits (an explicit Magi recipient) are mapped
//   RP-9  the verifier stores headers on demand: the bot asks for the blocks it
//         needs (attestHistoryRoot + attestHeaders from EIP-2935 proofs fetched
//         at each newly finalized block, else submitHeaders down from a stored
//         header) and tells the operator when a proof is needed (NEEDS_FILE)
//   RP-11 every in-flight withdrawal is broadcast, confirmed in nonce order, and
//         queued ones are drained

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// RP-7: MagiDeposit
// ---------------------------------------------------------------------------

// The bridge derives MagiDeposit's address for its vault from these exact bytes
// (account-mapping constants/deposit_code.go, l1-deposit); keccak256 pinned by
// TestMagiDepositCreationCodeHash on both sides.
const (
	magiDepositCreationCode     = "60a060405234801561000f575f5ffd5b506040516105b13803806105b183398101604081905261002e91610066565b6001600160a01b03811661005557604051636347e7c160e11b815260040160405180910390fd5b6001600160a01b0316608052610093565b5f60208284031215610076575f5ffd5b81516001600160a01b038116811461008c575f5ffd5b9392505050565b6080516104f96100b85f395f8181607c0152818161011a01526102ad01526104f95ff3fe608060405260043610610033575f3560e01c8063a10d096014610037578063a26e118614610058578063fbfa77cf1461006b575b5f5ffd5b348015610042575f5ffd5b506100566100513660046103f5565b6100ba565b005b610056610066366004610458565b610280565b348015610076575f5ffd5b5061009e7f000000000000000000000000000000000000000000000000000000000000000081565b6040516001600160a01b03909116815260200160405180910390f35b825f036100da57604051631f2a200560e01b815260040160405180910390fd5b6100e4828261037f565b836001600160a01b03163b5f0361010e5760405163022e258160e11b815260040160405180910390fd5b604080513360248201527f00000000000000000000000000000000000000000000000000000000000000006001600160a01b039081166044830152606480830187905283518084039091018152608490920183526020820180516001600160e01b03166323b872dd60e01b17905291515f928392908816916101909190610497565b5f604051808303815f865af19150503d805f81146101c9576040519150601f19603f3d011682016040523d82523d5f602084013e6101ce565b606091505b509150915081158061020b575080511580159061020b57508051602014158061020b57508080602001905181019061020691906104ad565b600114155b156102295760405163022e258160e11b815260040160405180910390fd5b856001600160a01b0316336001600160a01b03167fef519b7eb82aaf6ac376a6df2d793843ebfd593de5f1a0601d3cc6ab49ebb395878787604051610270939291906104c4565b60405180910390a3505050505050565b345f036102a057604051631f2a200560e01b815260040160405180910390fd5b6102aa828261037f565b5f7f00000000000000000000000000000000000000000000000000000000000000006001600160a01b0316346040515f6040518083038185875af1925050503d805f8114610313576040519150601f19603f3d011682016040523d82523d5f602084013e610318565b606091505b505090508061033a5760405163096dc0e160e01b815260040160405180910390fd5b6040515f9033907fef519b7eb82aaf6ac376a6df2d793843ebfd593de5f1a0601d3cc6ab49ebb39590610372903490889088906104c4565b60405180910390a3505050565b8080158061038d5750608081115b156103ab576040516333d1661360e11b815260040160405180910390fd5b505050565b5f5f83601f8401126103c0575f5ffd5b50813567ffffffffffffffff8111156103d7575f5ffd5b6020830191508360208285010111156103ee575f5ffd5b9250929050565b5f5f5f5f60608587031215610408575f5ffd5b84356001600160a01b038116811461041e575f5ffd5b935060208501359250604085013567ffffffffffffffff811115610440575f5ffd5b61044c878288016103b0565b95989497509550505050565b5f5f60208385031215610469575f5ffd5b823567ffffffffffffffff81111561047f575f5ffd5b61048b858286016103b0565b90969095509350505050565b5f82518060208501845e5f920191825250919050565b5f602082840312156104bd575f5ffd5b5051919050565b83815260406020820152816040820152818360608301375f818301606090810191909152601f909201601f191601019291505056"
	magiDepositCreationCodeHash = "14392c96a95bc649b2b82d231436c17e70a3fa11ee2000545193d00aa14b713b"
	create2DeployerHex          = "4e59b44847b379578588920ca78fbf26c0b4956c"
	depositEventSig             = "ef519b7eb82aaf6ac376a6df2d793843ebfd593de5f1a0601d3cc6ab49ebb395" // Deposit(address,address,uint256,string)
	maxDepositPairDistance      = 4
)

// depositContractAddress is the vault's MagiDeposit address (lowercase 0x hex):
// CREATE2 through the deterministic deployment proxy, salt 0, init code =
// creation code ++ abi.encode(vault).
func depositContractAddress(vault string) string {
	v, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(vault), "0x"))
	if err != nil || len(v) != 20 {
		return ""
	}
	code, _ := hex.DecodeString(magiDepositCreationCode)
	init := append(code, append(make([]byte, 12), v...)...)
	proxy, _ := hex.DecodeString(create2DeployerHex)
	buf := append([]byte{0xff}, proxy...)
	buf = append(buf, make([]byte, 32)...)
	buf = append(buf, keccak256(init)...)
	return "0x" + hex.EncodeToString(keccak256(buf)[12:])
}

type receiptLog struct {
	Address  string   `json:"address"`
	Topics   []string `json:"topics"`
	Data     string   `json:"data"`
	LogIndex string   `json:"logIndex"`
}

func topicAddr(t string) string {
	t = strings.ToLower(strings.TrimPrefix(t, "0x"))
	if len(t) != 64 || strings.TrimLeft(t[:24], "0") != "" {
		return ""
	}
	return "0x" + t[24:]
}

// pairedTransferIdx mirrors account-mapping's pairedTransfer: the per-receipt
// index of the Transfer that paid a token Deposit at i, or -1.
func pairedTransferIdx(logs []receiptLog, i int, sender, token, vault string, amount *big.Int) int {
	for j := i - 1; j >= 0 && j >= i-maxDepositPairDistance; j-- {
		l := logs[j]
		if !strings.EqualFold(l.Address, token) || len(l.Topics) < 3 || strings.ToLower(strings.TrimPrefix(l.Topics[0], "0x")) != TransferEventSig {
			continue
		}
		v := new(big.Int).SetBytes(hexToBytes(l.Data))
		if topicAddr(l.Topics[1]) == sender && topicAddr(l.Topics[2]) == vault && v.Cmp(amount) == 0 {
			return j
		}
		return -1
	}
	return -1
}

// scanContractDeposits returns block height's MagiDeposit deposits and the
// keys ("txhash#perReceiptIdx") of the token Transfers that paid them, which
// are not plain ERC-20 deposits (the bridge refuses them on that path).
func scanContractDeposits(rpc *ethRPC, height uint64, dc, vault string) ([]detectedDeposit, map[string]bool, error) {
	paid := map[string]bool{}
	logsData, err := rpc.call("eth_getLogs", fmt.Sprintf(`{"fromBlock":"0x%x","toBlock":"0x%x","address":"%s","topics":["0x%s"]}`, height, height, dc, depositEventSig))
	if err != nil {
		return nil, nil, fmt.Errorf("eth_getLogs MagiDeposit block %d: %w", height, err)
	}
	var logs []struct {
		TransactionIndex string `json:"transactionIndex"`
		TransactionHash  string `json:"transactionHash"`
		LogIndex         string `json:"logIndex"`
	}
	if err := json.Unmarshal(logsData, &logs); err != nil {
		return nil, nil, fmt.Errorf("parse MagiDeposit logs block %d: %w", height, err)
	}
	var deps []detectedDeposit
	for _, lg := range logs {
		if lg.LogIndex == "" {
			return nil, nil, fmt.Errorf("eth_getLogs returned a log with no logIndex, tx %s", lg.TransactionHash)
		}
		rData, err := rpc.getReceipt(lg.TransactionHash)
		if err != nil {
			return nil, nil, fmt.Errorf("receipt %s: %w", lg.TransactionHash, err)
		}
		var rc struct {
			Logs []receiptLog `json:"logs"`
		}
		if err := json.Unmarshal(rData, &rc); err != nil {
			return nil, nil, fmt.Errorf("decode receipt %s: %w", lg.TransactionHash, err)
		}
		idx := -1
		for i, l := range rc.Logs {
			if l.LogIndex == lg.LogIndex {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, nil, fmt.Errorf("MagiDeposit log %s not in receipt %s", lg.LogIndex, lg.TransactionHash)
		}
		l := rc.Logs[idx]
		if len(l.Topics) == 3 {
			data := hexToBytes(l.Data)
			if token := topicAddr(l.Topics[2]); token != "" && token != "0x0000000000000000000000000000000000000000" && len(data) >= 32 {
				if j := pairedTransferIdx(rc.Logs, idx, topicAddr(l.Topics[1]), token, strings.ToLower(vault), new(big.Int).SetBytes(data[:32])); j >= 0 {
					paid[strings.ToLower(lg.TransactionHash)+"#"+strconv.Itoa(j)] = true
				}
			}
		}
		deps = append(deps, detectedDeposit{BlockHeight: height, TxIndex: int(hexToUint64(lg.TransactionIndex)), LogIndex: idx,
			TxHash: lg.TransactionHash, DepositType: "contract"})
	}
	return deps, paid, nil
}

// contractMapPayload is a "contract" map: the receipt proof, base64, with the
// receipt as its leaf (not sent on its own).
func contractMapPayload(dep detectedDeposit, proofNodes [][]byte) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"tx_data": map[string]any{
		"block_height": dep.BlockHeight, "tx_index": dep.TxIndex, "log_index": dep.LogIndex,
		"deposit_type": "contract", "receipt_proof": base64.StdEncoding.EncodeToString(bytes.Join(proofNodes, nil)),
	}})
	return data
}

// ---------------------------------------------------------------------------
// RP-9: headers on demand
// ---------------------------------------------------------------------------

const (
	historyContract    = "0x0000F90827F1C53a10cb7A02335B175320002935"
	historyServeWindow = uint64(8191)
	// chainFallbackMax: the most headers the bot links down by parent hash
	// (submitHeaders, ~3.4k RC each) for one block no EIP-2935 proof covers.
	chainFallbackMax = uint64(256)
	// chainFallbackAfter: how long a need waits for an EIP-2935 attestation
	// before the bot links it down from the proven tip instead.
	chainFallbackAfter = 20 * time.Minute
	maxAttestCache     = 16
	attestPerCall      = 2 // storage proof + header ~3.6 KB each; 8 KB payload cap
	linkPerCall        = 6
)

// attestEntry holds EIP-2935 proofs fetched in the state of block F, for the
// blocks the bot needed then. They are submitted once F's header is stored (a
// proof landed at F).
type attestEntry struct {
	AccountProof []string                 `json:"account_proof"`
	Headers      map[uint64]attestedBlock `json:"headers"`
	FetchedAt    int64                    `json:"fetched_at"`
	Err          string                   `json:"err,omitempty"`
}

type attestedBlock struct {
	RLP          string   `json:"rlp"`
	StorageProof []string `json:"storage_proof"`
}

// verifierState reads keys of the ZK verifier wired into the bridge; binary
// keys (b-, hr-) go through verifierStateBin.
func verifierState(ctx context.Context, gql *vscGraphQL, contractID string, keys []string) (string, map[string]string, error) {
	return verifierStateEnc(ctx, gql, contractID, keys, false)
}

func verifierStateBin(ctx context.Context, gql *vscGraphQL, contractID string, keys []string) (string, map[string]string, error) {
	return verifierStateEnc(ctx, gql, contractID, keys, true)
}

func verifierStateEnc(ctx context.Context, gql *vscGraphQL, contractID string, keys []string, bin bool) (string, map[string]string, error) {
	st, err := gql.fetchContractState(ctx, contractID, []string{"zkverifier"})
	if err != nil {
		return "", nil, err
	}
	vid := st["zkverifier"]
	if vid == "" {
		return "", nil, fmt.Errorf("no verifier wired into the bridge")
	}
	if len(keys) == 0 {
		return vid, map[string]string{}, nil
	}
	if bin {
		vs, err := gql.fetchContractStateHex(ctx, vid, keys)
		return vid, vs, err
	}
	vs, err := gql.fetchContractState(ctx, vid, keys)
	return vid, vs, err
}

// headersStored reports which of blocks have a stored header on the verifier.
func headersStored(ctx context.Context, gql *vscGraphQL, contractID string, blocks []uint64) (map[uint64]bool, error) {
	keys := make([]string, len(blocks))
	for i, b := range blocks {
		keys[i] = "b-" + strconv.FormatUint(b, 10)
	}
	_, st, err := verifierStateBin(ctx, gql, contractID, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[uint64]bool, len(blocks))
	for _, b := range blocks {
		out[b] = len(st["b-"+strconv.FormatUint(b, 10)]) >= 137
	}
	return out, nil
}

// noteHeaderNeed records that block x's header is needed (a deposit to map, a
// withdrawal to confirm).
func noteHeaderNeed(cp *Checkpoint, x uint64) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.HeaderNeeds == nil {
		cp.HeaderNeeds = map[uint64]int64{}
	}
	if _, ok := cp.HeaderNeeds[x]; !ok {
		cp.HeaderNeeds[x] = time.Now().Unix()
		slog.Info("header needed", "block", x)
	}
}

// headerFields is every header field in RLP order, Prague through Glamsterdam
// (blockAccessListHash, slotNumber; live on Sepolia). Each is optional past
// baseFeePerGas; a header with a field not listed fails the hash check below.
var headerFields = []struct {
	name string
	num  bool
}{
	{"parentHash", false}, {"sha3Uncles", false}, {"miner", false}, {"stateRoot", false},
	{"transactionsRoot", false}, {"receiptsRoot", false}, {"logsBloom", false}, {"difficulty", true},
	{"number", true}, {"gasLimit", true}, {"gasUsed", true}, {"timestamp", true}, {"extraData", false},
	{"mixHash", false}, {"nonce", false}, {"baseFeePerGas", true}, {"withdrawalsRoot", false},
	{"blobGasUsed", true}, {"excessBlobGas", true}, {"parentBeaconBlockRoot", false}, {"requestsHash", false},
	{"blockAccessListHash", false}, {"slotNumber", true},
}

// headerRLP fetches block n's header and encodes it as RLP; the encoding must
// hash to the block hash the RPC reports.
func headerRLP(rpc *ethRPC, n uint64) ([]byte, error) {
	raw, err := rpc.call("eth_getBlockByNumber", fmt.Sprintf(`"0x%x", false`, n))
	if err != nil {
		return nil, err
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return nil, fmt.Errorf("block %d: bad JSON", n)
	}
	var items []byte
	for _, fd := range headerFields {
		v, ok := f[fd.name].(string)
		if !ok {
			continue
		}
		var b []byte
		if fd.num {
			// A quantity has no leading zeros, so its hex can be odd-length.
			q, ok := new(big.Int).SetString(strings.TrimPrefix(v, "0x"), 16)
			if !ok {
				return nil, fmt.Errorf("block %d: %s %q is not a quantity", n, fd.name, v)
			}
			b = q.Bytes() // minimal big-endian
		} else {
			b = hexToBytes(v)
		}
		items = append(items, rlpEncodeRaw(b)...)
	}
	out := encodeRLPList(items)
	if h, _ := f["hash"].(string); !strings.EqualFold("0x"+hex.EncodeToString(keccak256(out)), h) {
		return nil, fmt.Errorf("block %d: header RLP does not hash to %s (a header field this bot does not know?)", n, h)
	}
	return out, nil
}

// prefetchHistoryProofs fetches, in the state of finalized block F, the
// history contract's account proof and the storage proofs of the needed blocks
// in F's window. It must run while the RPC still serves F's state (geth: 128
// blocks), which is why it runs as soon as F is finalized.
func prefetchHistoryProofs(rpc *ethRPC, f uint64, needs []uint64) *attestEntry {
	e := &attestEntry{Headers: map[uint64]attestedBlock{}, FetchedAt: time.Now().Unix()}
	var slots []string
	var xs []uint64
	for _, x := range needs {
		if x < f && f-x <= historyServeWindow {
			slots = append(slots, fmt.Sprintf(`"0x%x"`, x%historyServeWindow))
			xs = append(xs, x)
		}
	}
	if len(xs) == 0 {
		return nil
	}
	raw, err := rpc.call("eth_getProof", fmt.Sprintf(`"%s", [%s], "0x%x"`, historyContract, strings.Join(slots, ","), f))
	if err != nil {
		e.Err = err.Error()
		slog.Warn("RP-9: eth_getProof at a finalized block failed (the RPC must serve recent state); headers fall back to submitHeaders",
			"block", f, "err", err)
		return e
	}
	var p struct {
		AccountProof []string `json:"accountProof"`
		StorageProof []struct {
			Proof []string `json:"proof"`
		} `json:"storageProof"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || len(p.StorageProof) != len(xs) {
		e.Err = fmt.Sprintf("bad eth_getProof result: %v", err)
		return e
	}
	e.AccountProof = p.AccountProof
	for i, x := range xs {
		rlp, err := headerRLP(rpc, x)
		if err != nil {
			slog.Warn("RP-9: header RLP", "block", x, "err", err)
			continue
		}
		e.Headers[x] = attestedBlock{RLP: hex.EncodeToString(rlp), StorageProof: p.StorageProof[i].Proof}
	}
	return e
}

func joinHexNodes(nodes []string) string {
	var all []byte
	for _, n := range nodes {
		all = append(all, hexToBytes(n)...)
	}
	return base64.StdEncoding.EncodeToString(all)
}

// handleHeaderNeeds stores the headers the bot needs and keeps NEEDS_FILE
// current for the operator.
func handleHeaderNeeds(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, submitter *l2Submitter, cp *Checkpoint, cfg EVMBotConfig) {
	cp.mu.Lock()
	var needs []uint64
	for x := range cp.HeaderNeeds {
		needs = append(needs, x)
	}
	cp.mu.Unlock()
	sort.Slice(needs, func(i, j int) bool { return needs[i] < needs[j] })

	vid, vst, err := verifierState(ctx, gql, cfg.ContractID, []string{"h"})
	if err != nil {
		slog.Debug("verifier state", "err", err)
		return
	}
	tip, _ := strconv.ParseUint(vst["h"], 10, 64)
	defer writeNeedsFile(ctx, gql, cp, cfg, tip)
	if len(needs) == 0 {
		cp.mu.Lock()
		cp.AttestCache = nil
		cp.mu.Unlock()
		return
	}
	stored, err := headersStored(ctx, gql, cfg.ContractID, needs)
	if err != nil {
		return
	}
	var open []uint64
	cp.mu.Lock()
	for _, x := range needs {
		if stored[x] {
			delete(cp.HeaderNeeds, x)
		} else {
			open = append(open, x)
		}
	}
	cp.mu.Unlock()
	if len(open) == 0 {
		return
	}

	// Prefetch at the newest finalized block, once.
	if f, err := rpc.getFinalizedBlock(); err == nil {
		cp.mu.Lock()
		_, have := cp.AttestCache[f]
		cp.mu.Unlock()
		if !have {
			if e := prefetchHistoryProofs(rpc, f, open); e != nil {
				cp.mu.Lock()
				if cp.AttestCache == nil {
					cp.AttestCache = map[uint64]*attestEntry{}
				}
				cp.AttestCache[f] = e
				for len(cp.AttestCache) > maxAttestCache {
					oldest := uint64(1<<63 - 1)
					for k := range cp.AttestCache {
						if k < oldest {
							oldest = k
						}
					}
					delete(cp.AttestCache, oldest)
				}
				cp.mu.Unlock()
			}
		}
	}

	// Submit the attestations whose root is stored now (a proof landed at F).
	cp.mu.Lock()
	var roots []uint64
	for f, e := range cp.AttestCache {
		if e != nil && e.Err == "" && f <= tip {
			roots = append(roots, f)
		}
	}
	cp.mu.Unlock()
	sort.Slice(roots, func(i, j int) bool { return roots[i] > roots[j] })
	if len(roots) > 0 {
		rs, err := headersStored(ctx, gql, cfg.ContractID, roots)
		if err == nil {
			for _, f := range roots {
				if !rs[f] {
					continue
				}
				attestFromRoot(ctx, gql, submitter, cp, cfg, vid, f, open)
			}
		}
	}

	// Fallback: link down from the proven tip.
	for _, x := range open {
		cp.mu.Lock()
		since := cp.HeaderNeeds[x]
		cp.mu.Unlock()
		if since == 0 || x >= tip || time.Since(time.Unix(since, 0)) < chainFallbackAfter {
			continue
		}
		if tip-x > chainFallbackMax {
			slog.Error("RP-9 CRITICAL: a needed header is outside every stored EIP-2935 window and too far below the tip to link down; attest it from an archive RPC",
				"block", x, "tip", tip)
			continue
		}
		linkDown(ctx, rpc, submitter, vid, x, tip)
		break // one run per tick
	}
}

// attestFromRoot submits attestHistoryRoot (if needed) and attestHeaders for
// the open needs cached at root f.
func attestFromRoot(ctx context.Context, gql *vscGraphQL, submitter *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, vid string, f uint64, open []uint64) {
	cp.mu.Lock()
	e := cp.AttestCache[f]
	cp.mu.Unlock()
	var items []map[string]string
	for _, x := range open {
		if hb, ok := e.Headers[x]; ok {
			items = append(items, map[string]string{"rlp_hex": hb.RLP, "storage_proof": joinHexNodes(hb.StorageProof)})
		}
	}
	if len(items) == 0 {
		return
	}
	fk := strconv.FormatUint(f, 10)
	st, err := gql.fetchContractStateHex(ctx, vid, []string{"hr-" + fk})
	if err != nil {
		return
	}
	if len(st["hr-"+fk]) != 32 {
		p, _ := json.Marshal(map[string]any{"block_number": f, "account_proof": joinHexNodes(e.AccountProof)})
		if _, err := submitter.callContractL2(ctx, vid, "attestHistoryRoot", p, 20_000); err != nil {
			slog.Warn("RP-9: attestHistoryRoot", "root", f, "err", err)
			return
		}
	}
	for i := 0; i < len(items); i += attestPerCall {
		end := i + attestPerCall
		if end > len(items) {
			end = len(items)
		}
		p, _ := json.Marshal(map[string]any{"root_block": f, "headers": items[i:end]})
		if _, err := submitter.callContractL2(ctx, vid, "attestHeaders", p, 20_000); err != nil {
			slog.Warn("RP-9: attestHeaders", "root", f, "err", err)
			return
		}
	}
	slog.Info("RP-9: headers attested", "root", f, "count", len(items))
}

// linkDown stores blocks x .. tip-1 with submitHeaders, newest first (each call
// links to the parent hash the previous one stored).
func linkDown(ctx context.Context, rpc *ethRPC, submitter *l2Submitter, vid string, x, tip uint64) {
	slog.Warn("RP-9: no EIP-2935 attestation for a needed header; linking down from the proven tip", "block", x, "tip", tip, "headers", tip-x)
	for top := tip - 1; ; {
		lo := x
		if top+1-x > linkPerCall {
			lo = top + 1 - linkPerCall
		}
		var hs []map[string]string
		for n := lo; n <= top; n++ {
			r, err := headerRLP(rpc, n)
			if err != nil {
				slog.Warn("RP-9: header RLP for submitHeaders", "block", n, "err", err)
				return
			}
			hs = append(hs, map[string]string{"rlp_hex": hex.EncodeToString(r)})
		}
		p, _ := json.Marshal(map[string]any{"headers": hs})
		if _, err := submitter.callContractL2(ctx, vid, "submitHeaders", p, 30_000); err != nil {
			slog.Warn("RP-9: submitHeaders", "from", lo, "to", top, "err", err)
			return
		}
		if lo == x {
			return
		}
		top = lo - 1
	}
}

// writeNeedsFile tells the operator (magi_operator, NEEDS_FILE) whether a proof
// is wanted: a needed header has no stored root above it, or withdrawals wait
// on a price older than pricingStaleAfter, or one is in flight while the tip
// header is old (the roll/expiry windows count proven L1 blocks).
func writeNeedsFile(ctx context.Context, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig, tip uint64) {
	path := os.Getenv("NEEDS_FILE")
	if path == "" {
		return
	}
	cp.mu.Lock()
	var blocks []uint64
	for x := range cp.HeaderNeeds {
		blocks = append(blocks, x)
	}
	cp.mu.Unlock()
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	reasons := []string{}
	for _, x := range blocks {
		if x >= tip {
			reasons = append(reasons, "header needed above the proven tip")
			break
		}
	}
	if st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"wqh", "wqt", "n", "np"}); err == nil {
		h, _ := strconv.ParseUint(st["wqh"], 10, 64)
		t, _ := strconv.ParseUint(st["wqt"], 10, 64)
		n, _ := strconv.ParseUint(st["n"], 10, 64)
		np, _ := strconv.ParseUint(st["np"], 10, 64)
		if age := tipHeaderAge(ctx, gql, cfg, tip); age > pricingStaleAfter && (t > h || np > n) {
			reasons = append(reasons, fmt.Sprintf("withdrawals waiting (queued %d, in flight %d) on a %s-old tip header", t-h, np-n, age.Round(time.Minute)))
		}
	}
	cp.mu.Lock()
	if len(reasons) == 0 {
		cp.WantProofSince = 0
	} else if cp.WantProofSince == 0 {
		cp.WantProofSince = time.Now().Unix()
	}
	since := cp.WantProofSince
	cp.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"want_proof": len(reasons) > 0, "since": since, "reasons": reasons,
		"blocks": blocks, "tip": tip, "updated": time.Now().Unix()})
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// pricingStaleAfter: a little under the bridge's MaxPricingHeaderAge (1 h), so
// a proof lands before queued withdrawals stop being priced.
var pricingStaleAfter = 45 * time.Minute

// tipHeaderAge is how old the proven tip header is (its L1 timestamp).
func tipHeaderAge(ctx context.Context, gql *vscGraphQL, cfg EVMBotConfig, tip uint64) time.Duration {
	_, st, err := verifierStateBin(ctx, gql, cfg.ContractID, []string{"b-" + strconv.FormatUint(tip, 10)})
	b := []byte(st["b-"+strconv.FormatUint(tip, 10)])
	if err != nil || len(b) < 137 {
		return 0
	}
	ts := int64(new(big.Int).SetBytes(b[121:129]).Uint64())
	return time.Since(time.Unix(ts, 0))
}

// ---------------------------------------------------------------------------
// RP-11: drain the withdrawal queue
// ---------------------------------------------------------------------------

const maxInFlightWithdrawals = 8

// drainIfQueued issues queued withdrawals into free slots
// (drainWithdrawalQueue; only that call issues queued ones, ~5k RC each).
func drainIfQueued(ctx context.Context, gql *vscGraphQL, submitter *l2Submitter, cfg EVMBotConfig) {
	st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"wqh", "wqt", "n", "np"})
	if err != nil {
		return
	}
	h, _ := strconv.ParseUint(st["wqh"], 10, 64)
	t, _ := strconv.ParseUint(st["wqt"], 10, 64)
	n, _ := strconv.ParseUint(st["n"], 10, 64)
	np, _ := strconv.ParseUint(st["np"], 10, 64)
	if t <= h || np-n >= maxInFlightWithdrawals {
		return
	}
	max := maxInFlightWithdrawals - (np - n)
	if q := t - h; q < max {
		max = q
	}
	p, _ := json.Marshal(map[string]any{"max": max})
	if _, err := submitter.callContractL2(ctx, cfg.ContractID, "drainWithdrawalQueue", p, 6_000*max+4_000); err != nil {
		slog.Warn("RP-11: drainWithdrawalQueue", "err", err)
		return
	}
	slog.Info("RP-11: drained the withdrawal queue", "max", max, "queued", t-h, "inFlight", np-n)
}

// ---------------------------------------------------------------------------
// RP-1 / RP-5: startup
// ---------------------------------------------------------------------------

// checkChainID refuses an L1 RPC on another chain than the bridge's.
func checkChainID(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, contractID string) error {
	st, err := gql.fetchContractState(ctx, contractID, []string{"chainid"})
	if err != nil {
		return fmt.Errorf("read the bridge's chain id: %w", err)
	}
	want, _ := strconv.ParseUint(st["chainid"], 10, 64)
	if want == 0 {
		want = 1 // the bridge's chainId() when none is set
	}
	raw, err := rpc.call("eth_chainId", "")
	if err != nil {
		return fmt.Errorf("eth_chainId: %w", err)
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	if got := hexToUint64(s); got != want {
		return fmt.Errorf("RP-5: the L1 RPC serves chain %d, the bridge is chain %d", got, want)
	}
	return nil
}

// startBlock sets a fresh checkpoint's scan start: START_BLOCK, else the
// verifier's tip (deposits below it need START_BLOCK). It refuses to scan from
// block 1, which took ~36 days to catch up on mainnet.
func startBlock(ctx context.Context, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig) error {
	if cp.LastScannedBlock > 0 {
		return nil
	}
	if v := os.Getenv("START_BLOCK"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			return fmt.Errorf("START_BLOCK %q is not a block number", v)
		}
		cp.LastScannedBlock = n - 1
		slog.Info("RP-1: new checkpoint starts at START_BLOCK", "block", n)
		return nil
	}
	_, st, err := verifierState(ctx, gql, cfg.ContractID, []string{"h"})
	if err != nil {
		return fmt.Errorf("RP-1: no checkpoint and no START_BLOCK, and the verifier tip is unreadable: %w", err)
	}
	h, _ := strconv.ParseUint(st["h"], 10, 64)
	if h == 0 {
		return fmt.Errorf("RP-1: no checkpoint, no START_BLOCK and no verifier tip; set START_BLOCK")
	}
	cp.LastScannedBlock = h
	slog.Warn("RP-1: new checkpoint starts at the verifier tip; set START_BLOCK to map deposits made before it", "block", h+1)
	return nil
}

// confirmLagAlarm: a withdrawal mined and final this long without a confirm is
// an alarm (RP-6: it holds the queue head; confirmSpend is permissionless, so
// anyone can settle it).
var confirmLagAlarm = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CONFIRM_LAG_ALARM")); err == nil && d > 0 {
		return d
	}
	return 6 * time.Hour
}()
