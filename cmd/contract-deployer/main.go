package main

import (
	"encoding/json"
	"fmt"
	"os"
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
	identityConfig := common.NewIdentityConfig()
	hiveConfig := streamer.NewHiveConfig()
	sysConfig := systemconfig.FromNetwork(args.network)
	wits := witnesses.NewEmptyWitnesses()
	p2p := p2pInterface.New(wits, identityConfig, sysConfig, nil)
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

func deployNewContract(sysConfig systemconfig.SystemConfig, identityConfig common.IdentityConfig, hiveConfig streamer.HiveConfig, proof *stateEngine.StorageProof, args args) {
	user := identityConfig.Get().HiveUsername
	wif := identityConfig.Get().HiveActiveKey

	if len(user) > 0 && len(wif) > 0 {
		hiveClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURI)

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

		deployOp := hivego.CustomJsonOperation{
			RequiredAuths:        []string{user},
			RequiredPostingAuths: []string{},
			Id:                   "vsc.create_contract",
			Json:                 string(j),
		}
		feeOp := hivego.TransferOperation{
			From:   user,
			To:     sysConfig.GatewayWallet(),
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

func updateContract(sysConfig systemconfig.SystemConfig, identityConfig common.IdentityConfig, hiveConfig streamer.HiveConfig, proof *stateEngine.StorageProof, args args) {
	user := identityConfig.Get().HiveUsername
	wif := identityConfig.Get().HiveActiveKey

	if len(user) > 0 && len(wif) > 0 {
		hiveClient := hivego.NewHiveRpc(hiveConfig.Get().HiveURI)

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

		updateOp := hivego.CustomJsonOperation{
			RequiredAuths:        []string{user},
			RequiredPostingAuths: []string{},
			Id:                   "vsc.update_contract",
			Json:                 string(j),
		}
		feeOp := hivego.TransferOperation{
			From:   user,
			To:     sysConfig.GatewayWallet(),
			Amount: "10.000 HBD",
			Memo:   "",
		}
		ops := []hivego.HiveOperation{updateOp}
		if proof != nil {
			ops = append(ops, feeOp)
		}
		txid, err := hiveClient.Broadcast(ops, &wif)
		if err != nil {
			fmt.Println("failed to broadcast contract update tx", err)
		} else {
			fmt.Println("tx id:", txid)
		}
	} else {
		fmt.Println("could not update contract as username and/or key is not specified in identityConfig.json")
	}
}
