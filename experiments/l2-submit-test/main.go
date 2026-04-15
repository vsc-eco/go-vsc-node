// Standalone test harness: proves the L2 submission path for the mapping bot.
//
// Generates a did:ethr identity (secp256k1 → EIP-712 sig), builds a vsc.call
// "map" op targeting the testnet BTC mapping contract, signs through the
// existing TransactionCrafter, and posts via submitTransactionV1 on
// magi-test.techcoderx.com.
//
// Testnet at commit 937553c is go-vsc-node before KeyDID was added to Parse,
// so we use EthDID — which the testnet's dids.Parse does accept.
//
// We do NOT fund the DID. The goal is to prove the ingest path
// (size → CBOR decode → DID parse → EIP-712 signature verify → nonce checks).
// "not enough RCS available" is SUCCESS for our purposes: everything upstream
// of RC accounting has validated.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"
)

const (
	gqlURL          = "https://magi-test.techcoderx.com/api/v1/graphql"
	testnetBtcCtrct = "vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t"
	netId           = "vsc-testnet"
)

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   map[string]interface{} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func gqlCall(body gqlRequest) (gqlResponse, []byte, error) {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", gqlURL, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return gqlResponse{}, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out gqlResponse
	json.Unmarshal(raw, &out)
	return out, raw, nil
}

func getNonce(account string) (uint64, error) {
	resp, raw, err := gqlCall(gqlRequest{
		Query:     `query($a: String!){ getAccountNonce(account:$a){ nonce } }`,
		Variables: map[string]interface{}{"a": account},
	})
	if err != nil {
		return 0, fmt.Errorf("%w raw=%s", err, raw)
	}
	if len(resp.Errors) > 0 {
		return 0, fmt.Errorf("%v", resp.Errors)
	}
	n, _ := resp.Data["getAccountNonce"].(map[string]interface{})
	if n == nil {
		return 0, nil
	}
	switch v := n["nonce"].(type) {
	case float64:
		return uint64(v), nil
	case string:
		var u uint64
		fmt.Sscanf(v, "%d", &u)
		return u, nil
	}
	return 0, nil
}

func submit(txB64, sigB64 string) (gqlResponse, []byte) {
	resp, raw, _ := gqlCall(gqlRequest{
		Query: `query($tx:String!,$sig:String!){ submitTransactionV1(tx:$tx, sig:$sig){ id } }`,
		Variables: map[string]interface{}{
			"tx":  txB64,
			"sig": sigB64,
		},
	})
	return resp, raw
}

// buildPayload produces a MappingInputData-shaped JSON — same envelope the
// production bot submits — so we're measuring the real payload shape.
func buildPayload(rawTxHexLen int) json.RawMessage {
	pad := bytes.Repeat([]byte{0xAB}, rawTxHexLen/2)
	rawTxHex := hex.EncodeToString(pad)
	merkleProofHex := hex.EncodeToString(bytes.Repeat([]byte{0xCD}, 384))
	payload := map[string]interface{}{
		"tx_data": map[string]interface{}{
			"block_height":     uint64(890000),
			"raw_tx_hex":       rawTxHex,
			"merkle_proof_hex": merkleProofHex,
			"tx_index":         uint64(42),
		},
		"instructions": []string{"to=vaultec"},
	}
	b, _ := json.Marshal(payload)
	return b
}

func main() {
	size := flag.Int("size", 7802, "raw_tx_hex length (7802 = actual failing tx from bc1q74...rq09)")
	flag.Parse()

	privBytes := bytes.Repeat([]byte{0x42}, 32)
	priv, _ := ethCrypto.ToECDSA(privBytes)
	ethAddr := ethCrypto.PubkeyToAddress(priv.PublicKey).Hex()
	did := dids.NewEthDID(ethAddr)

	fmt.Println("=== L2 submission test (EthDID path) ===")
	fmt.Println("ETH addr:", ethAddr)
	fmt.Println("DID     :", did.String())

	nonce, err := getNonce(did.String())
	if err != nil {
		fmt.Println("nonce err:", err)
		os.Exit(1)
	}
	fmt.Println("starting nonce:", nonce)

	payload := buildPayload(*size)
	fmt.Println("payload size (JSON bytes):", len(payload))
	var payloadValue interface{}
	if err := json.Unmarshal(payload, &payloadValue); err != nil {
		fmt.Println("payload unmarshal err:", err)
		os.Exit(1)
	}
	hiveEnvelopeObj := map[string]interface{}{
		"net_id":      netId,
		"caller":      "hive:milo.vsc",
		"contract_id": testnetBtcCtrct,
		"action":      "map",
		"payload":     payloadValue,
		"rc_limit":    10000,
		"intents":     []interface{}{},
	}
	hiveEnvelopeBytes, err := json.Marshal(hiveEnvelopeObj)
	if err != nil {
		fmt.Println("hive envelope marshal err:", err)
		os.Exit(1)
	}
	hiveEnvelope := string(hiveEnvelopeBytes)
	over := len(hiveEnvelope) > 8192
	fmt.Printf("hive custom_json envelope size: %d (cap 8192 → %s)\n",
		len(hiveEnvelope),
		map[bool]string{true: "OVER CAP (rejected by Hive)", false: "within cap"}[over])

	call := &transactionpool.VscContractCall{
		ContractId: testnetBtcCtrct,
		Action:     "map",
		Payload:    string(payload),
		RcLimit:    100,
		Intents:    []contracts.Intent{},
		Caller:     did.String(),
		NetId:      netId,
	}
	op, err := call.SerializeVSC()
	if err != nil {
		fmt.Println("SerializeVSC err:", err)
		os.Exit(1)
	}

	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   netId,
		RcLimit: 100,
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(priv),
		Did:      did,
	}

	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		fmt.Println("SignFinal err:", err)
		os.Exit(1)
	}
	fmt.Printf("L2 CBOR tx size: %d (cap 16384 → %s)\n", len(sTx.Tx),
		map[bool]string{true: "OVER CAP", false: "within cap"}[len(sTx.Tx) > 16384])

	txB64 := base64.URLEncoding.EncodeToString(sTx.Tx)
	sigB64 := base64.URLEncoding.EncodeToString(sTx.Sig)

	resp, raw := submit(txB64, sigB64)
	fmt.Println("--- submitTransactionV1 response ---")
	if len(resp.Errors) > 0 {
		for _, e := range resp.Errors {
			fmt.Println("ERROR:", e.Message)
		}
	}
	if len(resp.Data) > 0 {
		out, _ := json.MarshalIndent(resp.Data, "", "  ")
		fmt.Println("DATA:", string(out))
	}
	if len(resp.Data) == 0 && len(resp.Errors) == 0 {
		fmt.Println("RAW:", strings.TrimSpace(string(raw)))
	}
}
