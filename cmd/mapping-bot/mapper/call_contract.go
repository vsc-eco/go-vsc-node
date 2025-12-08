package mapper

import (
	"encoding/json"
	"fmt"
	"log"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/hive/streamer"
	stateEngine "vsc-node/modules/state-processing"

	"vsc-node/lib/hive"

	"github.com/vsc-eco/hivego"
)

type txVscCallContractJSON struct {
	NetId      string             `json:"net_id"`
	Caller     string             `json:"caller"`
	ContractId string             `json:"contract_id"`
	Action     string             `json:"action"`
	Payload    json.RawMessage    `json:"payload"`
	RcLimit    uint               `json:"rc_limit"`
	Intents    []contracts.Intent `json:"intents"`
}

// need a contract id
// call a contract from L1 (hive account) with 4 arguments:
//   - hive username
//   - contract id
//   - raw json message input
//   - action (function name/contract entrypoint)
//
// returning (json RawMessage, error)
func callContract(
	username, contractID string,
	contractInput json.RawMessage,
	action string,
) error {

	identityConfig := common.NewIdentityConfig()
	identityConfig.Init()
	hiveConfig := streamer.NewHiveConfig()
	hiveConfig.Init()

	hiveRpcClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURI)

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: identityConfig.HiveActiveKeyPair,
		},
	}

	log.Println("identity config", identityConfig)

	txObj := stateEngine.TxVscCallContract{
		NetId:      "vsc-mainnet",
		Caller:     fmt.Sprintf("hive:%s", username), // hive:username
		ContractId: contractID,
		Action:     action,
		Payload:    contractInput,
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	}

	wrapper := txVscCallContractJSON{
		NetId:      txObj.NetId,
		Caller:     txObj.Caller,
		ContractId: txObj.ContractId,
		Action:     txObj.Action,
		Payload:    txObj.Payload,
		RcLimit:    txObj.RcLimit,
		Intents:    txObj.Intents,
	}

	txJson, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}

	fmt.Println("txjson", string(txJson))
	// return nil

	op := hiveCreator.CustomJson([]string{username}, []string{}, "vsc.call", string(txJson))
	tx := hiveCreator.MakeTransaction([]hivego.HiveOperation{op})

	fmt.Println("tx created")

	hiveCreator.PopulateSigningProps(&tx, nil)
	sig, err := hiveCreator.Sign(tx)
	if err != nil {
		return fmt.Errorf("error creating signature: %w", err)
	}

	tx.AddSig(sig)
	_, err = hiveCreator.Broadcast(tx)
	if err != nil {
		return fmt.Errorf("error broadcasting tx: %w", err)
	}

	fmt.Println("tx broadcast")
	return nil
}
