package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"vsc-node/modules/db/vsc/contracts"
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

func (b *Bot) callContract(
	ctx context.Context,
	contractInput json.RawMessage,
	action string,
) (string, error) {
	username := b.IdentityConfig.Get().HiveUsername
	hiveRpcClient := hivego.NewHiveRpc(b.HiveConfig.Get().HiveURIs)
	hiveRpcClient.ChainID = b.SystemConfig.HiveChainId()

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: b.IdentityConfig.HiveActiveKeyPair,
		},
	}

	txObj := stateEngine.TxVscCallContract{
		NetId:      b.SystemConfig.NetId(),
		Caller:     fmt.Sprintf("hive:%s", username),
		ContractId: b.BotConfig.ContractId(),
		Action:     action,
		Payload:    contractInput,
		RcLimit:    10000,
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
		return "", err
	}

	b.L.Info("creating tx", "json", txJson)

	op := hiveCreator.CustomJson([]string{username}, []string{}, "vsc.call", string(txJson))
	tx := hiveCreator.MakeTransaction([]hivego.HiveOperation{op})

	hiveCreator.PopulateSigningProps(&tx, nil)
	sig, err := hiveCreator.Sign(tx)
	if err != nil {
		return "", fmt.Errorf("error creating signature: %w", err)
	}

	tx.AddSig(sig)
	txId, err := hiveCreator.Broadcast(tx)
	if err != nil {
		return "", fmt.Errorf("error broadcasting tx: %w", err)
	}

	b.L.Info("tx broadcast", "id", txId)
	return txId, nil
}
