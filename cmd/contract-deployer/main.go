package main

import (
	"encoding/json"
	"fmt"
	"os"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
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
	identityConfig := common.NewIdentityConfig()
	hiveConfig := streamer.NewHiveConfig()
	sysConfig := common_types.SystemConfig{
		Network: args.network,
	}
	wits := witnesses.NewEmptyWitnesses()
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

	initErr := a.Init()
	if initErr != nil {
		fmt.Println("failed to init plugins", err)
		os.Exit(1)
	}
	if args.isInit {
		fmt.Println("generated config files successfully")
		os.Exit(0)
	}

	WASM_CODE, err := os.ReadFile(args.wasmPath)
	if err != nil {
		fmt.Println("failed to read WASM file", err)
		os.Exit(1)
	}
	fmt.Println("WASM_CODE:", len(WASM_CODE), WASM_CODE[:10], "...")

	a.Start()

	proof, proofError := client.RequestProof(args.gqlUrl, WASM_CODE)
	if proofError != nil {
		fmt.Println("failed to request storage proof", proofError)
	} else {
		fmt.Println(proof)
		user := identityConfig.Get().HiveUsername
		wif := identityConfig.Get().HiveActiveKey
		owner := args.owner
		if owner == "" {
			owner = user
		}

		if len(user) > 0 && len(wif) > 0 {
			hiveClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURI)

			tx := stateEngine.TxCreateContract{
				Version:      "0.1",
				NetId:        "vsc-mainnet",
				Name:         args.name,
				Description:  args.description,
				Owner:        owner,
				Code:         proof.Hash,
				Runtime:      wasm_runtime.Go,
				StorageProof: proof,
			}

			txData := tx.ToData()
			j, err := json.Marshal(txData)
			if err != nil {
				fmt.Println("failed to marshal tx json data", err)
				os.Exit(1)
			}
			fmt.Println(string(j))

			deployOp := hivego.CustomJsonOperation{
				RequiredAuths:        []string{user},
				RequiredPostingAuths: []string{},
				Id:                   "vsc.create_contract",
				Json:                 string(j),
			}
			feeOp := hivego.TransferOperation{
				From:   user,
				To:     common.GATEWAY_WALLET,
				Amount: "10.000 HBD",
				Memo:   "",
			}

			txid, err := hiveClient.Broadcast([]hivego.HiveOperation{
				deployOp,
				feeOp,
			}, &wif)
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
