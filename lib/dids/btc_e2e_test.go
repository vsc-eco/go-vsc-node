package dids_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"vsc-node/lib/dids"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	codecJson "github.com/ipld/go-ipld-prime/codec/json"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeDagCbor mirrors common.EncodeDagCbor (duplicated to avoid import cycle).
// json.Marshal → DAG-JSON decode → DAG-CBOR encode
func encodeDagCbor(obj interface{}) ([]byte, error) {
	buf, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	nb := basicnode.Prototype.Any.NewBuilder()
	if err := codecJson.Decode(nb, bytes.NewBuffer(buf)); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := dagcbor.Encode(nb.Build(), &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// makeCIDBlock encodes obj to DAG-CBOR, computes a CIDv1, and returns the block.
func makeCIDBlock(t *testing.T, obj interface{}) blocks.Block {
	t.Helper()
	raw, err := encodeDagCbor(obj)
	require.NoError(t, err)

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}
	c, err := prefix.Sum(raw)
	require.NoError(t, err)

	blk, err := blocks.NewBlockWithCid(raw, c)
	require.NoError(t, err)
	return blk
}

// backendReconstructSigningShell simulates what IngestTx does:
//  1. Decode each CBOR payload into a Go map
//  2. Re-serialize to JSON via cbornode.MarshalJSON() (alphabetical keys)
//  3. Build the signing shell struct
//  4. Encode to DAG-CBOR and create a CID block
//
// The frontend must produce the identical CID for verification to succeed.
func backendReconstructSigningShell(t *testing.T, txCBOR []byte, shell signingShell) blocks.Block {
	t.Helper()

	// Decode CBOR payload → Go map → MarshalJSON (alphabetical keys)
	// This is exactly what IngestTx lines 98-111 do.
	reconstructedOps := make([]map[string]interface{}, 0, len(shell.Tx))
	for _, op := range shell.Tx {
		// Decode the raw CBOR bytes the same way IngestTx does
		node, err := cbornode.Decode(op.rawPayloadCBOR, multihash.SHA2_256, -1)
		require.NoError(t, err)

		jsonBytes, err := node.MarshalJSON()
		require.NoError(t, err)

		reconstructedOps = append(reconstructedOps, map[string]interface{}{
			"type":    op.Type,
			"payload": string(jsonBytes), // JSON string, keys sorted alphabetically by json.Marshal
		})
	}

	// Build the signing struct that gets encoded to DAG-CBOR for CID computation.
	// Uses the same field names as VSCTransactionSignStruct json tags.
	txSignStruct := map[string]interface{}{
		"__t": shell.T,
		"__v": shell.V,
		"headers": map[string]interface{}{
			"nonce":          shell.Headers.Nonce,
			"required_auths": shell.Headers.RequiredAuths,
			"rc_limit":       shell.Headers.RcLimit,
			"net_id":         shell.Headers.NetId,
		},
		"tx": reconstructedOps,
	}

	return makeCIDBlock(t, txSignStruct)
}

// --- helper types matching the frontend's signing shell ---

type signingShellHeaders struct {
	Nonce         int      `json:"nonce"`
	RequiredAuths []string `json:"required_auths"`
	RcLimit       int      `json:"rc_limit"`
	NetId         string   `json:"net_id"`
}

type signingShellOp struct {
	Type            string `json:"type"`
	Payload         string `json:"payload"` // JSON string with alphabetically sorted keys
	rawPayloadCBOR  []byte // CBOR-encoded payload (simulates what's in the wire tx)
}

type signingShell struct {
	T       string              `json:"__t"`
	V       string              `json:"__v"`
	Headers signingShellHeaders `json:"headers"`
	Tx      []signingShellOp    `json:"tx"`
}

// --- Frontend simulation helpers ---

// sortedJSON produces a JSON string with keys sorted alphabetically,
// exactly as the frontend's sortKeys() + JSON.stringify() does.
func sortedJSON(t *testing.T, obj interface{}) string {
	t.Helper()
	// json.Marshal on a Go map sorts keys alphabetically — matches sortKeys()
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	return string(b)
}

// frontendBuildSigningShell simulates what the frontend does:
//  1. CBOR-encode each payload (simulating encodeCborg)
//  2. Decode the CBOR back and JSON.stringify(sortKeys(decoded)) for the signing shell
//  3. Encode the full signing shell to DAG-CBOR via encodePayload()
//  4. Return the CID string (which gets signed by the BTC wallet)
func frontendBuildSigningShell(t *testing.T, did string, opType string, payload map[string]interface{}) (cidString string, shell signingShell) {
	t.Helper()

	// Step 1: CBOR-encode the payload (frontend uses custom cborg encoder)
	// We use encodeDagCbor which produces equivalent output for simple maps
	payloadCBOR, err := encodeDagCbor(payload)
	require.NoError(t, err)

	// Step 2: Decode CBOR back and create JSON with sorted keys
	// Frontend: JSON.stringify(sortKeys(decodeCborg(op.payload)))
	// Since json.Marshal sorts map keys alphabetically, this matches sortKeys()
	payloadJSON := sortedJSON(t, payload)

	shell = signingShell{
		T: "vsc-tx",
		V: "0.2",
		Headers: signingShellHeaders{
			Nonce:         0,
			RequiredAuths: []string{did},
			RcLimit:       500,
			NetId:         "vsc-mainnet",
		},
		Tx: []signingShellOp{
			{
				Type:           opType,
				Payload:        payloadJSON,
				rawPayloadCBOR: payloadCBOR,
			},
		},
	}

	// Step 3: Encode signing shell to DAG-CBOR and get CID
	// Frontend uses dag-jose-utils encodePayload() which does the same thing
	shellForEncoding := map[string]interface{}{
		"__t": shell.T,
		"__v": shell.V,
		"headers": map[string]interface{}{
			"nonce":          shell.Headers.Nonce,
			"required_auths": shell.Headers.RequiredAuths,
			"rc_limit":       shell.Headers.RcLimit,
			"net_id":         shell.Headers.NetId,
		},
		"tx": []map[string]interface{}{
			{
				"type":    opType,
				"payload": payloadJSON,
			},
		},
	}

	blk := makeCIDBlock(t, shellForEncoding)
	cidString = blk.Cid().String()
	return cidString, shell
}

// =============================================================================
// End-to-end tests
// =============================================================================

// TestE2E_BtcTransferSigning tests a simple HBD transfer on Magi signed by a
// P2WPKH (bc1q) BTC wallet — the most common real-world scenario.
func TestE2E_BtcTransferSigning(t *testing.T) {
	// Generate a P2WPKH key pair (bc1q address)
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	// Frontend builds the signing shell for a transfer
	payload := map[string]interface{}{
		"from":   did,
		"to":     "hive:vsc.gateway",
		"amount": "0.001",
		"asset":  "hbd",
	}

	cidString, shell := frontendBuildSigningShell(t, did, "transfer", payload)

	// Frontend signs the CID string via BTC wallet (BIP-137)
	sig := signBIP137(privKey, cidString, true)

	// === Backend side ===

	// 1. Parse the DID from required_auths
	parsedDID, err := dids.Parse(did)
	require.NoError(t, err, "backend must parse BTC DID from required_auths")

	// 2. Backend reconstructs the signing shell and CID from the wire transaction
	backendBlock := backendReconstructSigningShell(t, nil, shell)

	// 3. Verify CIDs match
	assert.Equal(t, cidString, backendBlock.Cid().String(),
		"frontend CID and backend-reconstructed CID must be identical")

	// 4. Verify the BIP-137 signature
	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "BIP-137 signature must verify against backend-reconstructed CID")
}

// TestE2E_BtcContractCallSigning tests a contract call (e.g., BTC transfer via
// mapping contract) — has more payload keys which stress-tests key ordering.
func TestE2E_BtcContractCallSigning(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	// Contract call payload — keys of varying lengths that expose sort-order bugs:
	// alphabetical: action, caller, contract_id, intents, payload, rc_limit
	// CBOR canonical (length-first): action, caller, intents, payload, rc_limit, contract_id
	payload := map[string]interface{}{
		"contract_id": "vs41q.....fake_contract_id",
		"action":      "transfer",
		"payload":     `{"amount":"100000","recipient_vsc_address":"did:pkh:bip122:000000000019d6689c085ae165831e93:bc1qtest"}`,
		"rc_limit":    float64(10000), // JSON numbers are float64
		"intents":     []interface{}{},
		"caller":      did,
	}

	cidString, shell := frontendBuildSigningShell(t, did, "call", payload)
	sig := signBIP137(privKey, cidString, true)

	// Backend side
	parsedDID, err := dids.Parse(did)
	require.NoError(t, err)

	backendBlock := backendReconstructSigningShell(t, nil, shell)

	assert.Equal(t, cidString, backendBlock.Cid().String(),
		"CIDs must match for contract call with many keys of different lengths")

	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "BIP-137 signature must verify for contract call")
}

// TestE2E_BtcWithdrawSigning tests a withdraw (Magi → Hive) signed by BTC wallet.
func TestE2E_BtcWithdrawSigning(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	payload := map[string]interface{}{
		"from":   did,
		"to":     "hive:lordbutterfly",
		"amount": "1.000",
		"asset":  "hbd",
	}

	cidString, shell := frontendBuildSigningShell(t, did, "withdraw", payload)
	sig := signBIP137(privKey, cidString, true)

	parsedDID, err := dids.Parse(did)
	require.NoError(t, err)

	backendBlock := backendReconstructSigningShell(t, nil, shell)

	assert.Equal(t, cidString, backendBlock.Cid().String())

	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "withdraw tx BIP-137 signature must verify")
}

// TestE2E_CIDKeyOrdering verifies that alphabetical key ordering (used by both
// the frontend's sortKeys() and Go's json.Marshal) produces the same CID as the
// backend's reconstruction path. This is the critical invariant — if key ordering
// diverges, CIDs differ and ALL BTC signatures fail silently.
func TestE2E_CIDKeyOrdering(t *testing.T) {
	// Payload with keys that sort differently under CBOR canonical vs alphabetical:
	// Alphabetical:      action, caller, contract_id, intents, payload, rc_limit
	// CBOR canonical:    action(6), caller(6), intents(7), payload(7), rc_limit(8), contract_id(11)
	payload := map[string]interface{}{
		"contract_id": "vs41qabc",
		"action":      "transfer",
		"payload":     `{}`,
		"rc_limit":    float64(500),
		"intents":     []interface{}{},
		"caller":      "did:pkh:bip122:000000000019d6689c085ae165831e93:bc1qtest",
	}

	// Frontend path: JSON.stringify(sortKeys(payload))
	// Go's json.Marshal sorts alphabetically — identical to sortKeys()
	frontendJSON := sortedJSON(t, payload)

	// Backend path: CBOR encode → cbornode.Decode → MarshalJSON
	payloadCBOR, err := encodeDagCbor(payload)
	require.NoError(t, err)

	node, err := cbornode.Decode(payloadCBOR, multihash.SHA2_256, -1)
	require.NoError(t, err)

	backendJSONBytes, err := node.MarshalJSON()
	require.NoError(t, err)
	backendJSON := string(backendJSONBytes)

	assert.Equal(t, frontendJSON, backendJSON,
		"Frontend sortKeys()+JSON.stringify() must produce identical JSON to backend cbornode.MarshalJSON()")

	t.Logf("Frontend JSON: %s", frontendJSON)
	t.Logf("Backend  JSON: %s", backendJSON)
}

// TestE2E_P2PKH_AddressType tests the flow with a legacy P2PKH address.
func TestE2E_P2PKH_AddressType(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	payload := map[string]interface{}{
		"from":   did,
		"to":     "hive:test",
		"amount": "0.001",
		"asset":  "hbd",
	}

	cidString, shell := frontendBuildSigningShell(t, did, "transfer", payload)
	sig := signBIP137(privKey, cidString, true)

	parsedDID, err := dids.Parse(did)
	require.NoError(t, err)

	backendBlock := backendReconstructSigningShell(t, nil, shell)
	assert.Equal(t, cidString, backendBlock.Cid().String())

	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "P2PKH address BIP-137 signature must verify")
}

// TestE2E_P2SH_AddressType tests the flow with a P2SH-P2WPKH address (3...).
func TestE2E_P2SH_AddressType(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	redeemScript := append([]byte{0x00, 0x14}, pubKeyHash...)
	addr, err := btcutil.NewAddressScriptHash(redeemScript, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	payload := map[string]interface{}{
		"from":   did,
		"to":     "hive:test",
		"amount": "0.001",
		"asset":  "hbd",
	}

	cidString, shell := frontendBuildSigningShell(t, did, "transfer", payload)
	sig := signBIP137(privKey, cidString, true)

	parsedDID, err := dids.Parse(did)
	require.NoError(t, err)

	backendBlock := backendReconstructSigningShell(t, nil, shell)
	assert.Equal(t, cidString, backendBlock.Cid().String())

	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "P2SH-P2WPKH address BIP-137 signature must verify")
}

// TestE2E_DIDParseRoundtrip verifies that the generic Parse() dispatcher routes
// BTC DIDs correctly alongside existing EVM and key DIDs.
func TestE2E_DIDParseRoundtrip(t *testing.T) {
	tests := []struct {
		name    string
		did     string
		wantErr bool
	}{
		{
			name: "BTC P2WPKH DID",
			did:  "did:pkh:bip122:000000000019d6689c085ae165831e93:bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		},
		{
			name: "BTC P2PKH DID",
			did:  "did:pkh:bip122:000000000019d6689c085ae165831e93:1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		},
		{
			name: "EVM DID",
			did:  "did:pkh:eip155:1:0x553Cb1F25f4409360E081E5e015812d1FB238e23",
		},
		{
			name:    "Taproot rejected",
			did:     "did:pkh:bip122:000000000019d6689c085ae165831e93:bc1p5d7rjq7g6rdk2yhzks9smlaqtedr4dekq08ge8ztwac72sfr9rusxg3s7p",
			wantErr: true,
		},
		{
			name:    "garbage DID",
			did:     "not-a-did",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := dids.Parse(tc.did)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.did, parsed.String())
			}
		})
	}
}

// signBIP322E2E creates a BIP-322 "simple" signature for E2E tests.
// Mirrors what Leather wallet produces for bc1q addresses.
func signBIP322E2E(t *testing.T, privKey *btcec.PrivateKey, msg string) string {
	t.Helper()

	pubkey := privKey.PubKey()
	pubkeyHash := btcutil.Hash160(pubkey.SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubkeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)
	scriptPubKey, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	// BIP-322 tagged message hash
	tag := sha256.Sum256([]byte("BIP0322-signed-message"))
	h := sha256.New()
	h.Write(tag[:])
	h.Write(tag[:])
	h.Write([]byte(msg))
	msgHash := h.Sum(nil)

	// Build to_spend transaction
	toSpend := wire.NewMsgTx(0)
	scriptSig := make([]byte, 0, 34)
	scriptSig = append(scriptSig, txscript.OP_0)
	scriptSig = append(scriptSig, txscript.OP_DATA_32)
	scriptSig = append(scriptSig, msgHash...)
	nullHash := chainhash.Hash{}
	txIn := wire.NewTxIn(wire.NewOutPoint(&nullHash, 0xFFFFFFFF), scriptSig, nil)
	txIn.Sequence = 0
	toSpend.AddTxIn(txIn)
	toSpend.AddTxOut(wire.NewTxOut(0, scriptPubKey))

	// Build to_sign transaction
	toSpendHash := toSpend.TxHash()
	toSign := wire.NewMsgTx(0)
	toSignIn := wire.NewTxIn(wire.NewOutPoint(&toSpendHash, 0), nil, nil)
	toSignIn.Sequence = 0
	toSign.AddTxIn(toSignIn)
	toSign.AddTxOut(wire.NewTxOut(0, []byte{txscript.OP_RETURN}))

	// Compute BIP-143 sighash
	scriptCode, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_DUP).
		AddOp(txscript.OP_HASH160).
		AddData(pubkeyHash).
		AddOp(txscript.OP_EQUALVERIFY).
		AddOp(txscript.OP_CHECKSIG).
		Script()
	require.NoError(t, err)

	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(scriptPubKey, 0)
	sigHashes := txscript.NewTxSigHashes(toSign, prevOutputFetcher)
	sighashBytes, err := txscript.CalcWitnessSigHash(scriptCode, sigHashes, txscript.SigHashAll, toSign, 0, 0)
	require.NoError(t, err)

	sig := ecdsa.Sign(privKey, sighashBytes)
	derSig := sig.Serialize()
	sigWithSighash := append(derSig, byte(txscript.SigHashAll))

	compressedPubkey := pubkey.SerializeCompressed()
	witness := make([]byte, 0, 1+1+len(sigWithSighash)+1+len(compressedPubkey))
	witness = append(witness, 2)
	witness = append(witness, byte(len(sigWithSighash)))
	witness = append(witness, sigWithSighash...)
	witness = append(witness, byte(len(compressedPubkey)))
	witness = append(witness, compressedPubkey...)

	return base64.StdEncoding.EncodeToString(witness)
}

// TestE2E_BtcTransferSigning_BIP322 tests a transfer signed with BIP-322
// (the format Leather wallet actually produces for bc1q addresses).
func TestE2E_BtcTransferSigning_BIP322(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	payload := map[string]interface{}{
		"from":   did,
		"to":     "hive:vsc.gateway",
		"amount": "0.001",
		"asset":  "hbd",
	}

	cidString, shell := frontendBuildSigningShell(t, did, "transfer", payload)

	// Sign with BIP-322 (what Leather actually does for bc1q)
	sig := signBIP322E2E(t, privKey, cidString)

	// Backend side
	parsedDID, err := dids.Parse(did)
	require.NoError(t, err)

	backendBlock := backendReconstructSigningShell(t, nil, shell)
	assert.Equal(t, cidString, backendBlock.Cid().String(),
		"frontend CID and backend-reconstructed CID must be identical")

	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "BIP-322 signature must verify against backend-reconstructed CID")
}

// TestE2E_BtcStakeHBD_BIP322 tests a stake_hbd operation with BIP-322 signature.
func TestE2E_BtcStakeHBD_BIP322(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := "did:pkh:bip122:000000000019d6689c085ae165831e93:" + addr.String()

	payload := map[string]interface{}{
		"from":   did,
		"to":     did,
		"amount": "10.000",
		"asset":  "hbd",
		"type":   "stake_hbd",
	}

	cidString, shell := frontendBuildSigningShell(t, did, "stake_hbd", payload)
	sig := signBIP322E2E(t, privKey, cidString)

	parsedDID, err := dids.Parse(did)
	require.NoError(t, err)

	backendBlock := backendReconstructSigningShell(t, nil, shell)
	assert.Equal(t, cidString, backendBlock.Cid().String())

	valid, err := parsedDID.Verify(backendBlock, sig)
	require.NoError(t, err)
	assert.True(t, valid, "BIP-322 stake_hbd signature must verify")
}
