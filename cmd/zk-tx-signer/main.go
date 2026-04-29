// zk-tx-signer: Go sidecar for signing Magi L2 transactions.
//
// Used by the Rust SP1-Helios prover operator to submit proofs to Magi.
// Wraps go-vsc-node's TransactionCrafter to produce correctly formatted
// CBOR + EIP-712 signed transactions.
//
// Usage: zk-tx-signer <contractId> <action> <payloadJSON> <netId> <rcLimit> <nonce> <privateKeyHex>
// Output: JSON {"tx_b64":"...","sig_b64":"..."}
//
// Build: go build -o zk-tx-signer ./cmd/zk-tx-signer/
// (from go-vsc-node repo with this file at cmd/zk-tx-signer/main.go)
package main

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func main() {
	if len(os.Args) != 8 {
		fmt.Fprintf(os.Stderr, "Usage: %s <contractId> <action> <payloadJSON> <netId> <rcLimit> <nonce> <privateKeyHex>\n", os.Args[0])
		os.Exit(1)
	}

	contractID := os.Args[1]
	action := os.Args[2]
	payloadJSON := os.Args[3]
	netID := os.Args[4]
	rcLimit, err := strconv.ParseUint(os.Args[5], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid rcLimit: %v\n", err)
		os.Exit(1)
	}
	nonce, err := strconv.ParseUint(os.Args[6], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid nonce: %v\n", err)
		os.Exit(1)
	}
	privKeyHex := os.Args[7]

	privKey, err := ethCrypto.HexToECDSA(privKeyHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid private key: %v\n", err)
		os.Exit(1)
	}

	txB64, sigB64, err := signTransaction(privKey, contractID, action, payloadJSON, netID, rcLimit, nonce)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign error: %v\n", err)
		os.Exit(1)
	}

	result := map[string]string{
		"tx_b64":  txB64,
		"sig_b64": sigB64,
	}
	json.NewEncoder(os.Stdout).Encode(result)
}

func signTransaction(
	privKey *ecdsa.PrivateKey,
	contractID, action, payloadJSON, netID string,
	rcLimit, nonce uint64,
) (string, string, error) {
	addr := ethCrypto.PubkeyToAddress(privKey.PublicKey).Hex()
	did := dids.NewEthDID(addr)

	call := &transactionpool.VscContractCall{
		ContractId: contractID,
		Action:     action,
		Payload:    payloadJSON,
		RcLimit:    uint(rcLimit),
		Intents:    []contracts.Intent{},
		Caller:     did.String(),
		NetId:      netID,
	}

	op, err := call.SerializeVSC()
	if err != nil {
		return "", "", fmt.Errorf("serialize op: %w", err)
	}

	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   netID,
		RcLimit: rcLimit,
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(privKey),
		Did:      did,
	}

	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", "", fmt.Errorf("sign: %w", err)
	}

	txB64 := base64.URLEncoding.EncodeToString(sTx.Tx)
	sigB64 := base64.URLEncoding.EncodeToString(sTx.Sig)

	return txB64, sigB64, nil
}
