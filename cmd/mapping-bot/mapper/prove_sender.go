package mapper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/modules/common/params"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// SENDER-REFUND-1 (owner 11-21): a deposit swap that does not complete goes back
// to the address that SENT it, which nobody types. The contract learns it from
// proveSender: the transaction the deposit's first input spends, proven against
// a stored header (THORChain's bifrost reads the same output). The bot sends it
// before its map, so the contract can refund there and pay a swap out on
// Ethereum knowing where a payout that comes back returns; and again after a
// map someone else made first without it, which left the deposit held. Every
// step is best effort: when the proof cannot be made (the previous transaction
// is too large for a call, or older than the contract's header window) the map
// still goes, and the deposit is held for the owner's manual refund.

// senderProofs queues deposits that were mapped without a sender proof.
type senderProofs struct {
	mu  sync.Mutex
	txs map[string]*wire.MsgTx
}

func (s *senderProofs) add(tx *wire.MsgTx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs == nil {
		s.txs = map[string]*wire.MsgTx{}
	}
	if len(s.txs) < 256 {
		s.txs[tx.TxID()] = tx
	}
}

func (s *senderProofs) take() []*wire.MsgTx {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*wire.MsgTx, 0, len(s.txs))
	for id, tx := range s.txs {
		out = append(out, tx)
		delete(s.txs, id)
	}
	return out
}

// isSwapInstruction reports whether an instruction is a deposit swap.
func isSwapInstruction(instruction string) bool {
	return strings.Contains(instruction, "swap_to=")
}

type proveSenderPayload struct {
	DepositTxB64 string               `json:"deposit_tx_b64"`
	Sender       *VerificationRequest `json:"sender"`
}

var errSenderProofTooLarge = errors.New("the sender proof is larger than a contract call allows")

// buildProveSenderPayload proves deposit's sender: the previous transaction of
// its first input, with its merkle proof. It also returns that output's address.
func (b *Bot) buildProveSenderPayload(deposit *wire.MsgTx) (json.RawMessage, string, error) {
	if len(deposit.TxIn) == 0 {
		return nil, "", errors.New("deposit has no inputs")
	}
	in := deposit.TxIn[0].PreviousOutPoint
	details, err := b.Chain.Client.GetTxDetails(in.Hash.String())
	if err != nil {
		return nil, "", fmt.Errorf("previous tx %s: %w", in.Hash, err)
	}
	if !details.Confirmed {
		return nil, "", fmt.Errorf("previous tx %s is not confirmed", in.Hash)
	}
	rawBlock, err := b.Chain.Client.GetRawBlock(details.BlockHash)
	if err != nil {
		return nil, "", fmt.Errorf("block %s: %w", details.BlockHash, err)
	}
	var block wire.MsgBlock
	if err := block.Deserialize(bytes.NewReader(rawBlock)); err != nil {
		return nil, "", fmt.Errorf("block %s: %w", details.BlockHash, err)
	}
	idx := int(details.TxIndex)
	if idx >= len(block.Transactions) || block.Transactions[idx].TxHash() != in.Hash {
		return nil, "", fmt.Errorf("previous tx %s is not at index %d of block %s", in.Hash, idx, details.BlockHash)
	}
	prev := block.Transactions[idx]
	if int(in.Index) >= len(prev.TxOut) {
		return nil, "", fmt.Errorf("previous tx %s has no output %d", in.Hash, in.Index)
	}
	proof, err := generateMerkleProof(&block, idx)
	if err != nil {
		return nil, "", err
	}
	var prevBuf, depBuf bytes.Buffer
	if err := prev.SerializeNoWitness(&prevBuf); err != nil {
		return nil, "", err
	}
	if err := deposit.SerializeNoWitness(&depBuf); err != nil {
		return nil, "", err
	}
	p := proveSenderPayload{
		DepositTxB64: base64.StdEncoding.EncodeToString(depBuf.Bytes()),
		Sender: &VerificationRequest{
			BlockHeight:    details.BlockHeight,
			RawTxB64:       base64.StdEncoding.EncodeToString(prevBuf.Bytes()),
			MerkleProofHex: proof,
			TxIndex:        uint64(idx),
		},
	}
	if payloadNodeSize(p) > params.MAX_CONTRACT_PAYLOAD_SIZE {
		return nil, "", errSenderProofTooLarge
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	sender := ""
	if _, addrs, _, err := txscript.ExtractPkScriptAddrs(prev.TxOut[in.Index].PkScript, b.ChainParams); err == nil && len(addrs) == 1 {
		sender = addrs[0].EncodeAddress()
	}
	return raw, sender, nil
}

// proveSender submits deposit's sender proof and watches the sending address
// for its refund. Errors are logged by the caller; nothing depends on it.
func (b *Bot) proveSender(ctx context.Context, deposit *wire.MsgTx) error {
	payload, sender, err := b.buildProveSenderPayload(deposit)
	if err != nil {
		return err
	}
	// Dry run first: a proof the contract refuses (a vault-owned output, a header
	// outside its window) is not broadcast and costs no RC.
	if sim, err := b.gql().SimulateContractCall(ctx, b.botEthDID.String(), b.BotConfig.ContractId(), "proveSender", string(payload), simulateRcCap); err == nil && !sim.Success && sim.Err != "gas_limit_hit" {
		return fmt.Errorf("the contract refuses the sender proof: %s %s", sim.Err, sim.ErrMsg)
	}
	if _, err := b.callWithRetry(ctx, payload, "proveSender", broadcastRetryAttempts); err != nil {
		return err
	}
	if sender != "" {
		b.watchRefundAddress(sender)
	}
	b.L.Info("deposit sender proven", "btcTxId", deposit.TxID(), "sender", sender)
	return nil
}

// heldWithoutSender reports whether any output of deposit is held waiting for a
// sender proof ("sph-<txid>:<vout>").
func (b *Bot) heldWithoutSender(ctx context.Context, deposit *wire.MsgTx) (bool, error) {
	keys := make([]string, len(deposit.TxOut))
	for i := range deposit.TxOut {
		keys[i] = contractinterface.SenderPendingPrefix + deposit.TxID() + ":" + strconv.Itoa(i)
	}
	st, err := b.fetchStateHex(ctx, keys)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if len(st[k]) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// proveHeldSenders proves the senders of swap deposits mapped (by anyone)
// without one, whose refunds are held until then.
func (b *Bot) proveHeldSenders(ctx context.Context) {
	if !b.provesSenders() {
		b.pendingSenders.take()
		return
	}
	for _, tx := range b.pendingSenders.take() {
		if ctx.Err() != nil {
			return
		}
		held, err := b.heldWithoutSender(ctx, tx)
		if err != nil {
			b.L.Warn("could not read a deposit's held state; its sender proof waits for the next block", "btcTxId", tx.TxID(), "err", err)
			b.pendingSenders.add(tx)
			continue
		}
		if !held {
			continue
		}
		if err := b.proveSender(ctx, tx); err != nil {
			b.L.Error("a held deposit's sender could not be proven; it stays held for a manual refund",
				"btcTxId", tx.TxID(), "err", err)
		}
	}
}

// proveSenderBeforeMap proves a swap deposit's sender just before its map, so
// the contract refunds there (and can pay the swap out on Ethereum). Best effort.
func (b *Bot) proveSenderBeforeMap(ctx context.Context, in *MappingInputData) {
	// Only the BTC mapping contract has proveSender (the LTC/DASH/DOGE/BCH ones
	// are not on mainnet and refund only to hive: or hold).
	if !b.provesSenders() {
		return
	}
	swap := false
	for _, instr := range in.Instructions {
		if isSwapInstruction(instr) {
			swap = true
		}
	}
	if !swap || in.TxData == nil {
		return
	}
	tx, err := decodeMapTx(in.TxData)
	if err != nil {
		b.L.Warn("could not read a deposit for its sender proof; mapping it without one", "err", err)
		return
	}
	if err := b.proveSender(ctx, tx); err != nil {
		b.L.Warn("deposit sender not proven; a refund of it is held until it is (or for a manual refund)",
			"btcTxId", tx.TxID(), "err", err)
		b.pendingSenders.add(tx)
	}
}

// decodeMapTx is the deposit transaction of a map proof (hex or base64).
func decodeMapTx(td *VerificationRequest) (*wire.MsgTx, error) {
	var raw []byte
	var err error
	if td.RawTxB64 != "" {
		raw, err = base64.StdEncoding.DecodeString(td.RawTxB64)
	} else {
		raw, err = hex.DecodeString(td.RawTxHex)
	}
	if err != nil {
		return nil, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return &tx, nil
}

// senderProofProbe caches whether the deployed contract takes proveSender.
type senderProofProbe struct {
	mu      sync.Mutex
	ok      bool
	checked time.Time
}

// senderProbeEvery is how often a contract without proveSender is asked again.
const senderProbeEvery = 10 * time.Minute

// provesSenders reports whether this bot's contract takes sender proofs: the
// BTC chain, and the deployed contract has proveSender (a dry run with no
// proof answers "proveSender needs ..."; a contract from before it has no
// such action). Until then the old instruction rules stay (refund_to needed),
// so a bot deployed before the contract cannot relax them.
func (b *Bot) provesSenders() bool {
	if b.Chain == nil || !strings.EqualFold(b.Chain.Name, "btc") {
		return false
	}
	p := &b.senderProbe
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ok || (!p.checked.IsZero() && time.Since(p.checked) < senderProbeEvery) {
		return p.ok
	}
	p.checked = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sim, err := b.gql().SimulateContractCall(ctx, b.botEthDID.String(), b.BotConfig.ContractId(), "proveSender", "{}", simulateRcCap)
	p.ok = err == nil && strings.Contains(sim.ErrMsg, "proveSender needs")
	if !p.ok {
		b.L.Info("BTC contract has no proveSender yet: refund_to stays required", "err", err, "reason", sim.ErrMsg)
	}
	return p.ok
}

// ProvesSenders is provesSenders for the HTTP server's instruction check.
func (b *Bot) ProvesSenders() bool { return b.provesSenders() }
