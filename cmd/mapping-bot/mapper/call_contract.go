package mapper

import (
	"encoding/json"
	"fmt"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/hive/streamer"
	stateEngine "vsc-node/modules/state-processing"

	"vsc-node/lib/hive"

	"github.com/vsc-eco/hivego"
)

// need a contract id
// call a contract with 3 arguments a contract id, an input string, actions (function name)
// returning (json RawMessage, error)
func callContract(
	username, contractID string,
	contractInput json.RawMessage,
	action string,
) error {

	hiveApiUrl := streamer.NewHiveConfig()
	hiveRpcClient := hivego.NewHiveRpc(hiveApiUrl.Get().HiveURI)
	identityConfig := common.NewIdentityConfig()

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: identityConfig.HiveActiveKeyPair,
		},
	}

	txObj := stateEngine.TxVscCallContract{
		NetId:      "vsc-mainnet",
		Caller:     fmt.Sprintf("hive:%s", username), // hive:username
		ContractId: contractID,
		Action:     action,
		Payload:    contractInput,
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	}

	txData := txObj.ToData()
	txJson, err := json.Marshal(&txData)
	if err != nil {
		return err
	}

	op := hiveCreator.CustomJson([]string{username}, nil, "vsc.call", string(txJson))
	tx := hiveCreator.MakeTransaction([]hivego.HiveOperation{op})

	hiveCreator.PopulateSigningProps(&tx, nil)
	sig, err := hiveCreator.Sign(tx)
	if err != nil {
		return err
	}

	tx.AddSig(sig)
	_, err = hiveCreator.Broadcast(tx)
	if err != nil {
		return err
	}
	return nil
}
