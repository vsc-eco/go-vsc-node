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
	// review2 #122: the private key must not be passed as a CLI arg —
	// argv is world-readable via /proc/<pid>/cmdline (and leaks to ps,
	// shell history, logs). Prefer ZK_TX_SIGNER_PRIVKEY; keep the
	// deprecated positional form as a fallback so a rollout version-skew
	// between caller and signer can't break signing.
	privKeyHex, fromArgv, argErr := resolvePrivKey(os.Args, os.Getenv("ZK_TX_SIGNER_PRIVKEY"))
	if argErr != nil {
		fmt.Fprintf(os.Stderr, "Usage: %s <contractId> <action> <payloadJSON> <netId> <rcLimit> <nonce> [<privateKeyHex>]\n  (set ZK_TX_SIGNER_PRIVKEY instead of the deprecated, insecure positional key) — %v\n", os.Args[0], argErr)
		os.Exit(1)
	}
	if fromArgv {
		fmt.Fprintln(os.Stderr, "zk-tx-signer: WARNING private key passed as a CLI argument (visible via /proc/<pid>/cmdline & ps); set ZK_TX_SIGNER_PRIVKEY instead")
	}

	contractID := os.Args[1]
	action := os.Args[2]
	payloadJSON := os.Args[3]
	netID := os.Args[4]
	rcLimit, err := strconv.ParseUint(os.Args[5], 10, strconv.IntSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid rcLimit: %v\n", err)
		os.Exit(1)
	}
	nonce, err := strconv.ParseUint(os.Args[6], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid nonce: %v\n", err)
		os.Exit(1)
	}

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

// resolvePrivKey returns the signing key, preferring envPrivKey
// (ZK_TX_SIGNER_PRIVKEY) over the deprecated positional argv form.
// args is os.Args (args[0] is the program name). fromArgv reports the
// insecure CLI-arg path so the caller can warn. review2 #122.
func resolvePrivKey(args []string, envPrivKey string) (privKey string, fromArgv bool, err error) {
	if envPrivKey != "" {
		// 6 positional args (no key on the command line).
		if len(args) != 7 {
			return "", false, fmt.Errorf("with ZK_TX_SIGNER_PRIVKEY set, expected 6 positional args, got %d", len(args)-1)
		}
		return envPrivKey, false, nil
	}
	// Legacy: key as the 7th positional arg (insecure, deprecated).
	if len(args) != 8 {
		return "", false, fmt.Errorf("expected 7 positional args, or set ZK_TX_SIGNER_PRIVKEY for 6, got %d", len(args)-1)
	}
	return args[7], true, nil
}
