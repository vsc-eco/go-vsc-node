package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
) error {
	username := b.IdentityConfig.Get().HiveUsername
	hiveRpcClient := hivego.NewHiveRpc(b.HiveConfig.Get().HiveURIs)

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: b.IdentityConfig.HiveActiveKeyPair,
		},
	}

	txObj := stateEngine.TxVscCallContract{
		NetId:      b.NetId,
		Caller:     fmt.Sprintf("hive:%s", username),
		ContractId: b.ContractId,
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
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		return nil
	}

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
