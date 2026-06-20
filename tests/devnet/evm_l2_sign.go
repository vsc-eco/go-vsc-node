//go:build evm_devnet

// Evm-key-signed L2 contract calls for the EVM-bridge waves (W1/W2/W3).
//
// The default harness CallContract signs as a Hive witness account. But an ETH
// deposit credits the L2 balance to AddressToDID(depositor_evm_addr) (review6 H9
// disables ETH deposit routing), so unmapETH must be called AS that evm DID —
// i.e. an L2 tx signed by the depositor's secp256k1 key. This mirrors the bot's
// callContractL2 exactly (transactionpool + lib/dids), reusing the same vsc-node
// libraries so the envelope is byte-faithful.
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"fmt"
	"strings"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// EthKey bundles a secp256k1 key with its derived L2 DID + 0x address.
type EthKey struct {
	Priv *ecdsa.PrivateKey
	DID  dids.EthDID
	Addr string // 0x-prefixed checksummed
}

// NewEthKey builds an EthKey from a 64-hex private key (no 0x).
func NewEthKey(privHex string) (*EthKey, error) {
	priv, err := ethCrypto.HexToECDSA(privHex)
	if err != nil {
		return nil, fmt.Errorf("bad eth privkey: %w", err)
	}
	addr := ethCrypto.PubkeyToAddress(priv.PublicKey).Hex() // EIP-55 checksummed (display)
	// The contract credits deposits to AddressToDID = lowercase ("0x"+hex). Balance
	// and RC lookups are case-sensitive on the exact DID string, so the L2 caller
	// DID MUST be lowercase to match. Signature verification is case-insensitive
	// (EthDID.Verify uses strings.EqualFold), so a lowercase DID still verifies.
	did := dids.NewEthDID(strings.ToLower(addr))
	return &EthKey{Priv: priv, DID: did, Addr: addr}, nil
}

// FetchL2Nonce reads the L2 account nonce for a DID/account string.
func (d *Devnet) FetchL2Nonce(ctx context.Context, node int, account string) (uint64, error) {
	const q = `query($a:String!){ getAccountNonce(account:$a){ nonce } }`
	var out struct {
		GetAccountNonce struct {
			Nonce uint64 `json:"nonce"`
		} `json:"getAccountNonce"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"a": account}, &out); err != nil {
		return 0, err
	}
	return out.GetAccountNonce.Nonce, nil
}

// SubmitL2Tx submits a signed (base64url) VSC L2 transaction, returns its CID id.
func (d *Devnet) SubmitL2Tx(ctx context.Context, node int, txB64, sigB64 string) (string, error) {
	const q = `query($tx:String!,$sig:String!){ submitTransactionV1(tx:$tx, sig:$sig){ id } }`
	var out struct {
		SubmitTransactionV1 struct {
			Id string `json:"id"`
		} `json:"submitTransactionV1"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"tx": txB64, "sig": sigB64}, &out); err != nil {
		return "", err
	}
	return out.SubmitTransactionV1.Id, nil
}

// CallContractEvm broadcasts an L2 contract call signed by an evm key, so the
// contract sees Caller == the key's evm DID (mirrors bot callContractL2).
func (d *Devnet) CallContractEvm(ctx context.Context, node int, key *EthKey, contractID, action, payload string, rcLimit uint64) (string, error) {
	if rcLimit == 0 {
		rcLimit = 5_000_000
	}
	nonce, err := d.FetchL2Nonce(ctx, node, key.DID.String())
	if err != nil {
		return "", fmt.Errorf("fetch L2 nonce for %s: %w", key.DID.String(), err)
	}
	call := &transactionpool.VscContractCall{
		ContractId: contractID,
		Action:     action,
		Payload:    payload,
		RcLimit:    uint(rcLimit),
		Intents:    []contracts.Intent{},
		Caller:     key.DID.String(),
		NetId:      d.netId(),
	}
	op, err := call.SerializeVSC()
	if err != nil {
		return "", fmt.Errorf("serialize L2 op: %w", err)
	}
	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   d.netId(),
		RcLimit: rcLimit,
	}
	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(key.Priv),
		Did:      key.DID,
	}
	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", fmt.Errorf("sign L2 tx: %w", err)
	}
	if len(sTx.Tx) > transactionpool.MAX_TX_SIZE {
		return "", fmt.Errorf("L2 tx too large: %d > %d", len(sTx.Tx), transactionpool.MAX_TX_SIZE)
	}
	return d.SubmitL2Tx(ctx, node,
		base64.URLEncoding.EncodeToString(sTx.Tx),
		base64.URLEncoding.EncodeToString(sTx.Sig))
}
