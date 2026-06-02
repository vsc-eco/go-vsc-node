package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	data_availability_client "vsc-node/modules/data-availability/client"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/vsc-eco/hivego"
)

func main() {
	args, err := ParseArgs()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	identityConfig := common.NewIdentityConfig(args.dataDir)
	hiveConfig := streamer.NewHiveConfig(args.dataDir)
	p2pConf := p2pInterface.NewConfig(args.dataDir)
	sysConfig := systemconfig.FromNetwork(args.network)
	if args.sysconfigPath != "" {
		if err := sysConfig.LoadOverrides(args.sysconfigPath); err != nil {
			fmt.Println("Error loading sysconfig overrides:", err)
			os.Exit(1)
		}
	}

	// Submit a previously-prepared, externally-signed transaction. This needs
	// neither a private key nor the P2P/data-availability stack (the storage
	// proof is already baked into the bundle's operations), so it short-circuits
	// the rest of the deploy flow.
	if args.broadcastSigned != "" {
		if err := hiveConfig.Init(); err != nil {
			fmt.Println("failed to init hive config", err)
			os.Exit(1)
		}
		broadcastSignedBundle(sysConfig, hiveConfig, args)
		return
	}

	wits := witnesses.NewEmptyWitnesses()
	p2p := p2pInterface.New(wits, p2pConf, identityConfig, sysConfig, nil)
	da := datalayer.New(p2p, args.dataDir)
	client := data_availability_client.New(p2p, identityConfig, sysConfig, da)

	plugins := []aggregate.Plugin{
		identityConfig,
		p2pConf,
		hiveConfig,
		p2p,
		da,
		client,
	}
	a := aggregate.New(
		plugins,
	)

	initErr := a.Init()
	if initErr != nil {
		fmt.Println("failed to init plugins", initErr)
		os.Exit(1)
	}
	if args.isInit {
		fmt.Println("generated config files successfully")
		os.Exit(0)
	}
	if args.contractId == "" && args.wasmPath == "" {
		fmt.Println("Path to compiled WASM bytecode must be specified when deploying new contract")
		os.Exit(1)
	}

	a.Start()

	if args.wasmPath != "" {
		code, err := os.ReadFile(args.wasmPath)
		if err != nil {
			fmt.Println("failed to read WASM file", err)
			os.Exit(1)
		}
		fmt.Println("code:", len(code), code[:10], "...")
		proof, proofError := client.RequestProof(args.gqlUrl, code)
		if proofError != nil {
			fmt.Println("failed to request storage proof", proofError)
		} else {
			fmt.Println("storage proof", proof)
			if args.contractId == "" {
				deployNewContract(sysConfig, identityConfig, hiveConfig, &proof, args)
			} else {
				updateContract(sysConfig, identityConfig, hiveConfig, &proof, args)
			}
		}
	} else {
		updateContract(sysConfig, identityConfig, hiveConfig, nil, args)
	}

	err = a.Stop()
	if err != nil {
		fmt.Println("failed to stop plugins", err)
		os.Exit(1)
	}
}

func deployNewContract(
	sysConfig systemconfig.SystemConfig,
	identityConfig common.IdentityConfig,
	hiveConfig streamer.HiveConfig,
	proof *stateEngine.StorageProof,
	args args,
) {
	user := identityConfig.Get().HiveUsername
	if len(user) == 0 {
		fmt.Println("not publishing contract as username is not specified in identityConfig.json")
		return
	}

	tx := stateEngine.TxCreateContract{
		Version:      "0.1",
		NetId:        sysConfig.NetId(),
		Name:         args.name,
		Description:  args.description,
		Owner:        args.owner,
		Code:         proof.Hash,
		Runtime:      wasm_runtime.Go,
		StorageProof: *proof,
	}

	j, err := json.Marshal(tx)
	if err != nil {
		fmt.Println("failed to marshal tx json data", err)
		os.Exit(1)
	}
	fmt.Println(string(j))

	currency := "HBD"
	if sysConfig.OnTestnet() {
		currency = "TBD"
	}

	deployOp := hivego.CustomJsonOperation{
		RequiredAuths:        []string{user},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.create_contract",
		Json:                 string(j),
	}
	feeOp := hivego.TransferOperation{
		From:   user,
		To:     sysConfig.GatewayWallet(),
		Amount: "10.000 " + currency,
		Memo:   "",
	}

	submitOrPrepare(sysConfig, identityConfig, hiveConfig, []hivego.HiveOperation{deployOp, feeOp}, user, args)
}

func updateContract(
	sysConfig systemconfig.SystemConfig,
	identityConfig common.IdentityConfig,
	hiveConfig streamer.HiveConfig,
	proof *stateEngine.StorageProof,
	args args,
) {
	user := identityConfig.Get().HiveUsername
	if len(user) == 0 {
		fmt.Println("could not update contract as username is not specified in identityConfig.json")
		return
	}

	tx := stateEngine.TxUpdateContract{
		NetId:       sysConfig.NetId(),
		Id:          args.contractId,
		Name:        args.name,
		Description: args.description,
	}
	if proof != nil {
		tx.Runtime = &wasm_runtime.Go
		tx.Code = proof.Hash
		tx.StorageProof = proof
	}
	if args.owner != "" {
		tx.Owner = args.owner
	}

	j, err := json.Marshal(tx)
	if err != nil {
		fmt.Println("failed to marshal tx json data", err)
		os.Exit(1)
	}
	fmt.Println(string(j))

	currency := "HBD"
	if sysConfig.OnTestnet() {
		currency = "TBD"
	}

	updateOp := hivego.CustomJsonOperation{
		RequiredAuths:        []string{user},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.update_contract",
		Json:                 string(j),
	}
	feeOp := hivego.TransferOperation{
		From:   user,
		To:     sysConfig.GatewayWallet(),
		Amount: "10.000 " + currency,
		Memo:   "",
	}
	ops := []hivego.HiveOperation{updateOp}
	if proof != nil {
		ops = append(ops, feeOp)
	}

	submitOrPrepare(sysConfig, identityConfig, hiveConfig, ops, user, args)
}

// submitOrPrepare either broadcasts the operations with the local active key
// (the default), or — when -no-broadcast is set — builds the transaction and
// emits a signing bundle so the active signature can be produced externally
// (e.g. on a Ledger) and later submitted with -broadcast-signed.
func submitOrPrepare(
	sysConfig systemconfig.SystemConfig,
	identityConfig common.IdentityConfig,
	hiveConfig streamer.HiveConfig,
	ops []hivego.HiveOperation,
	user string,
	args args,
) {
	hiveClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURIs)
	hiveClient.ChainID = sysConfig.HiveChainId()

	if args.noBroadcast {
		prepareSigningBundle(hiveClient, ops, user, args)
		return
	}

	if args.ledgerSignCmd != "" {
		signWithLedgerAndBroadcast(hiveClient, ops, user, args)
		return
	}

	wif := identityConfig.Get().HiveActiveKey
	if len(wif) == 0 {
		fmt.Println("not broadcasting as active key is not specified in identityConfig.json (use -no-broadcast to sign externally)")
		return
	}

	txid, err := hiveClient.Broadcast(ops, &wif)
	if err != nil {
		fmt.Println("failed to broadcast contract tx", err)
		return
	}
	reportBroadcast(txid, ops, args)
}

// reportBroadcast prints the broadcast result, including the derived contract id
// for a fresh deploy (a leading custom_json op with no existing contract id).
func reportBroadcast(txid string, ops []hivego.HiveOperation, args args) {
	fmt.Println("tx id:", txid)
	if len(ops) > 0 {
		if _, ok := ops[0].(hivego.CustomJsonOperation); ok && args.contractId == "" {
			fmt.Println("contract id:", common.ContractId(txid, 0))
		}
	}
}

// signWithLedgerAndBroadcast builds the transaction, asks an external command
// (args.ledgerSignCmd) to produce the active signature, attaches it, and
// broadcasts — all in one run, so the transaction stays byte-identical between
// signing and broadcast. The command runs via 'sh -c', receives the signing
// request (a signingBundle JSON, with .path set) on stdin, and must print only
// the signature hex on stdout.
func signWithLedgerAndBroadcast(
	hiveClient *hivego.HiveRpcNode,
	ops []hivego.HiveOperation,
	user string,
	args args,
) {
	bundle, tx, err := buildSigningBundle(hiveClient, ops, user, args)
	if err != nil {
		fmt.Println("failed to build transaction for signing:", err)
		os.Exit(1)
	}
	bundle.Path = args.ledgerPath

	req, err := json.Marshal(bundle)
	if err != nil {
		fmt.Println("failed to encode signing request:", err)
		os.Exit(1)
	}

	fmt.Printf("requesting signature from external signer for account %q (path %s)\n", user, args.ledgerPath)
	fmt.Println("confirm the transaction on your device — it expires at", bundle.Transaction.Expiration, "UTC")

	sig, err := runExternalSigner(args.ledgerSignCmd, req)
	if err != nil {
		fmt.Println("external signer failed:", err)
		fmt.Println("command:", args.ledgerSignCmd)
		fmt.Println("(is the signer set up? see cmd/contract-deployer/ledger-signer/README.md)")
		os.Exit(1)
	}

	tx.AddSig(sig)
	txid, err := hiveClient.BroadcastRaw(tx)
	if err != nil {
		fmt.Println("failed to broadcast signed transaction:", err)
		os.Exit(1)
	}
	reportBroadcast(txid, ops, args)
}

// runExternalSigner runs the operator-provided signing command via 'sh -c',
// feeding it the request on stdin and returning the validated signature hex from
// stdout. Anything the command writes to stderr is folded into the error so a
// missing/misconfigured signer surfaces a useful message.
func runExternalSigner(signCmd string, request []byte) (string, error) {
	cmd := exec.Command("sh", "-c", signCmd)
	cmd.Stdin = bytes.NewReader(request)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w\n%s", err, msg)
		}
		return "", err
	}

	sig := strings.TrimSpace(stdout.String())
	if err := validateSignatureHex(sig); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("returned an invalid signature (%w)\n%s", err, msg)
		}
		return "", fmt.Errorf("returned an invalid signature: %w", err)
	}
	return sig, nil
}

// validateSignatureHex checks the external signer returned a 65-byte compact
// recoverable secp256k1 signature in hex — the format Hive expects.
func validateSignatureHex(sig string) error {
	b, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("not valid hex: %w", err)
	}
	if len(b) != 65 {
		return fmt.Errorf("expected 65 bytes (130 hex chars), got %d bytes", len(b))
	}
	return nil
}

// unsignedTx is a Hive transaction in the standard wire shape, directly
// consumable by Hive signing tools (e.g. hive-ledger-js / hive-tx).
type unsignedTx struct {
	RefBlockNum    uint16               `json:"ref_block_num"`
	RefBlockPrefix uint32               `json:"ref_block_prefix"`
	Expiration     string               `json:"expiration"`
	Operations     [][2]json.RawMessage `json:"operations"`
	Extensions     []string             `json:"extensions"`
	Signatures     []string             `json:"signatures"`
}

// signingBundle is everything an external signer needs to produce the active
// signature, plus everything -broadcast-signed needs to reconstruct the exact
// same transaction byte-for-byte (so the signature still validates).
type signingBundle struct {
	Network string `json:"network"`
	// ChainId must be fed to the signer: the digest is sha256(chainId || tx).
	ChainId string `json:"chain_id"`
	// Account whose active authority must sign the transaction.
	Account string `json:"account"`
	// SigningDigest is the 32-byte hash (hex) to sign directly when using the
	// Ledger Hive app's hash/blind-signing mode.
	SigningDigest string     `json:"signing_digest"`
	SerializedTx  string     `json:"serialized_tx"`
	TxId          string     `json:"txid"`
	Transaction   unsignedTx `json:"transaction"`
	// Path is the BIP32 derivation path for the external signer. Only populated
	// for the -ledger-sign-cmd request; omitted from the -no-broadcast bundle.
	Path string `json:"path,omitempty"`
}

// buildSigningBundle fetches chain state, builds the (unsigned) transaction from
// the given operations, and returns everything needed to sign it externally and
// later reconstruct it byte-for-byte. The returned hivego transaction is the
// canonical source for the digest/txid in the bundle.
func buildSigningBundle(
	hiveClient *hivego.HiveRpcNode,
	ops []hivego.HiveOperation,
	user string,
	args args,
) (signingBundle, hivego.HiveTransaction, error) {
	propsB, err := hiveClient.GetDynamicGlobalProps()
	if err != nil {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("fetch dynamic global props: %w", err)
	}
	var props struct {
		HeadBlockNumber int    `json:"head_block_number"`
		HeadBlockId     string `json:"head_block_id"`
		Time            string `json:"time"`
	}
	if err := json.Unmarshal(propsB, &props); err != nil {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("parse dynamic global props: %w", err)
	}

	refBlockNum := uint16(props.HeadBlockNumber & 0xffff)
	hbidB, err := hex.DecodeString(props.HeadBlockId)
	if err != nil || len(hbidB) < 8 {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("decode head_block_id %q: %w", props.HeadBlockId, err)
	}
	refBlockPrefix := binary.LittleEndian.Uint32(hbidB[4:8])

	headTime, err := time.Parse("2006-01-02T15:04:05", props.Time)
	if err != nil {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("parse head block time: %w", err)
	}
	expiration := headTime.Add(time.Duration(args.expiration) * time.Second).Format("2006-01-02T15:04:05")

	tx := hivego.HiveTransaction{
		RefBlockNum:    refBlockNum,
		RefBlockPrefix: refBlockPrefix,
		Expiration:     expiration,
		Operations:     ops,
	}

	serialized, err := hivego.SerializeTx(tx)
	if err != nil {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("serialize transaction: %w", err)
	}
	digest := hivego.HashTxForSig(serialized, hiveClient.ChainID)
	txid, err := tx.GenerateTrxId()
	if err != nil {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("compute transaction id: %w", err)
	}

	opsJs, err := opsToWire(ops)
	if err != nil {
		return signingBundle{}, hivego.HiveTransaction{}, fmt.Errorf("encode operations: %w", err)
	}

	bundle := signingBundle{
		Network:       args.network,
		ChainId:       hiveClient.ChainID,
		Account:       user,
		SigningDigest: hex.EncodeToString(digest),
		SerializedTx:  hex.EncodeToString(serialized),
		TxId:          txid,
		Transaction: unsignedTx{
			RefBlockNum:    refBlockNum,
			RefBlockPrefix: refBlockPrefix,
			Expiration:     expiration,
			Operations:     opsJs,
			Extensions:     []string{},
			Signatures:     []string{},
		},
	}
	return bundle, tx, nil
}

func prepareSigningBundle(
	hiveClient *hivego.HiveRpcNode,
	ops []hivego.HiveOperation,
	user string,
	args args,
) {
	bundle, _, err := buildSigningBundle(hiveClient, ops, user, args)
	if err != nil {
		fmt.Println("failed to build signing bundle:", err)
		os.Exit(1)
	}
	expiration := bundle.Transaction.Expiration

	out, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		fmt.Println("failed to encode signing bundle", err)
		os.Exit(1)
	}

	fmt.Println("\n===== UNSIGNED TRANSACTION (sign the active authority externally) =====")
	fmt.Println(string(out))
	fmt.Println("=======================================================================")
	fmt.Printf("\nAccount to sign with (active): %s\n", user)
	fmt.Printf("Expires at (UTC): %s\n", expiration)
	fmt.Println("Sign 'signing_digest' on the device (hash/blind signing), or feed 'transaction' + 'chain_id' to a Hive signing tool,")
	fmt.Printf("then broadcast with:\n  %s -network %s -broadcast-signed <file> -signature <hex>\n", os.Args[0], args.network)

	if args.out != "" {
		if err := os.WriteFile(args.out, out, 0600); err != nil {
			fmt.Println("failed to write signing bundle to file", err)
			os.Exit(1)
		}
		fmt.Println("wrote signing bundle to", args.out)
	}
}

func broadcastSignedBundle(
	sysConfig systemconfig.SystemConfig,
	hiveConfig streamer.HiveConfig,
	args args,
) {
	data, err := os.ReadFile(args.broadcastSigned)
	if err != nil {
		fmt.Println("failed to read signing bundle", err)
		os.Exit(1)
	}
	var bundle signingBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		fmt.Println("failed to parse signing bundle", err)
		os.Exit(1)
	}

	ops, err := wireToOps(bundle.Transaction.Operations)
	if err != nil {
		fmt.Println("failed to reconstruct operations from bundle", err)
		os.Exit(1)
	}

	tx := hivego.HiveTransaction{
		RefBlockNum:    bundle.Transaction.RefBlockNum,
		RefBlockPrefix: bundle.Transaction.RefBlockPrefix,
		Expiration:     bundle.Transaction.Expiration,
		Operations:     ops,
	}

	hiveClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURIs)
	hiveClient.ChainID = sysConfig.HiveChainId()

	// Guard against a tampered/mismatched bundle: the signature was produced
	// over the digest computed at prepare time, so a reconstruction that does
	// not reproduce that digest would broadcast an invalid signature.
	serialized, err := hivego.SerializeTx(tx)
	if err != nil {
		fmt.Println("failed to re-serialize transaction", err)
		os.Exit(1)
	}
	digest := hex.EncodeToString(hivego.HashTxForSig(serialized, hiveClient.ChainID))
	if bundle.SigningDigest != "" && digest != bundle.SigningDigest {
		fmt.Printf("refusing to broadcast: reconstructed signing digest %s does not match bundle's %s\n", digest, bundle.SigningDigest)
		fmt.Println("(the bundle may have been edited, or -network / -sysconfig differ from when it was prepared)")
		os.Exit(1)
	}

	tx.AddSig(args.signature)

	txid, err := hiveClient.BroadcastRaw(tx)
	if err != nil {
		fmt.Println("failed to broadcast signed transaction", err)
		os.Exit(1)
	}
	fmt.Println("tx id:", txid)
}

// opsToWire encodes operations into Hive's standard [[name, obj], ...] form.
func opsToWire(ops []hivego.HiveOperation) ([][2]json.RawMessage, error) {
	out := make([][2]json.RawMessage, 0, len(ops))
	for _, op := range ops {
		nameB, err := json.Marshal(op.OpName())
		if err != nil {
			return nil, err
		}
		objB, err := json.Marshal(op)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]json.RawMessage{nameB, objB})
	}
	return out, nil
}

// wireToOps rebuilds typed operations from the bundle's wire form. Only the
// operation types this tool emits (custom_json, transfer) are supported.
func wireToOps(wire [][2]json.RawMessage) ([]hivego.HiveOperation, error) {
	ops := make([]hivego.HiveOperation, 0, len(wire))
	for _, pair := range wire {
		var name string
		if err := json.Unmarshal(pair[0], &name); err != nil {
			return nil, err
		}
		switch name {
		case "custom_json":
			var op hivego.CustomJsonOperation
			if err := json.Unmarshal(pair[1], &op); err != nil {
				return nil, err
			}
			ops = append(ops, op)
		case "transfer":
			var op hivego.TransferOperation
			if err := json.Unmarshal(pair[1], &op); err != nil {
				return nil, err
			}
			ops = append(ops, op)
		default:
			return nil, fmt.Errorf("unsupported operation %q in bundle", name)
		}
	}
	return ops, nil
}
