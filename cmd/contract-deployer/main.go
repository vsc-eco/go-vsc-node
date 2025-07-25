package main

import (
	"encoding/json"
	"fmt"
	"os"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	data_availability_client "vsc-node/modules/data-availability/client"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/vsc-eco/hivego"
)

type EmptyWitnesses struct {
	witnesses []witnesses.Witness
}

func NewEmptyWitnesses() *EmptyWitnesses {
	return &EmptyWitnesses{
		witnesses: make([]witnesses.Witness, 0),
	}
}

func (w *EmptyWitnesses) GetLastestWitnesses() ([]witnesses.Witness, error) {
	return w.witnesses, nil
}

func main() {
	args, err := ParseArgs()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	identityConfig := common.NewIdentityConfig()
	hiveConfig := streamer.NewHiveConfig()
	sysConfig := common.SystemConfig{
		Network: args.network,
	}
	wits := NewEmptyWitnesses()
	p2p := p2pInterface.New(wits, identityConfig, sysConfig)
	da := datalayer.New(p2p)
	client := data_availability_client.New(p2p, identityConfig, da)

	plugins := []aggregate.Plugin{
		identityConfig,
		hiveConfig,
		p2p,
		da,
		client,
	}
	a := aggregate.New(
		plugins,
	)

	WASM_CODE, err := os.ReadFile(args.wasmPath)
	if err != nil {
		fmt.Println("failed to read WASM file", err)
		os.Exit(1)
	}

	fmt.Println("WASM_CODE:", len(WASM_CODE), WASM_CODE[:10], "...")

	err = a.Init()
	if err != nil {
		fmt.Println("failed to init plugins", err)
		os.Exit(1)
	}
	a.Start()

	proof, proofError := client.RequestProof(WASM_CODE)
	if proofError != nil {
		fmt.Println("failed to request storage proof", proofError)
	} else {
		fmt.Println(proof)
		user := identityConfig.Get().HiveUsername
		wif := identityConfig.Get().HiveActiveKey

		if len(user) > 0 && len(wif) > 0 {
			hiveClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURI)

			tx := stateEngine.TxCreateContract{
				Version:      "0.1",
				NetId:        "vsc-mainnet",
				Name:         args.name,
				Description:  args.description,
				Owner:        user,
				Code:         proof.Hash,
				Runtime:      wasm_runtime.Go,
				StorageProof: proof,
			}

			txData := tx.ToData()
			j, _ := json.Marshal(txData)
			fmt.Println(string(j))

			txid, err := hiveClient.BroadcastJson([]string{user}, []string{}, "vsc.create_contract", string(j), &wif)
			if err != nil {
				fmt.Println("failed to broadcast contract creation tx", err)
			} else {
				fmt.Println("tx id:", txid)
				fmt.Println("contract id:", common.ContractId(txid, 0))
			}
		} else {
			fmt.Println("not publishing contract as username and/or key is not specified in identityConfig.json")
		}
	}

	err = a.Stop()
	if err != nil {
		fmt.Println("failed to stop plugins", err)
		os.Exit(1)
	}
}
