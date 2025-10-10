package mapper

import (
	"encoding/json"
	"fmt"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/vsc-eco/hivego"
)

// need a contract id
// call a contract with 3 arguments a contract id, an input string, actions (function name)
// returning (json RawMessage, error)
func callContract(
	username, contractID string,
	contractInput json.RawMessage,
	action string,
) (json.RawMessage, error) {
	tx := stateEngine.TxVscCallContract{
		NetId:      "vsc-mainnet",
		Caller:     fmt.Sprintf("hive:%s", username), // hive:username
		ContractId: contractID,
		Action:     action,
		Payload:    contractInput,
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	}

	txData := tx.ToData()
	txJson, err := json.Marshal(&txData)
	if err != nil {
		return nil, err
	}

	deployOp := hivego.CustomJsonOperation{
		RequiredAuths:        []string{username},
		RequiredPostingAuths: []string{username},
		Id:                   contractID,
		Json:                 string(txJson),
	}

	deployOpJsonBytes, err := json.Marshal(&deployOp)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(deployOpJsonBytes), nil
}
