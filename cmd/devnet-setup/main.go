package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/gateway"
	"vsc-node/modules/gql"
	"vsc-node/modules/hive/streamer"
	p2p "vsc-node/modules/p2p"
	state_engine "vsc-node/modules/state-processing"

	"github.com/vsc-eco/hivego"
)

func main() {
	args, err := ParseArgs()
	if err != nil {
		fmt.Println("Error parsing arguments:", err)
		os.Exit(1)
	}

	kp, err := hivego.KeyPairFromWif(args.wif)
	if err != nil {
		fmt.Println("invalid wif:", err)
		os.Exit(1)
	}
	pubKey := kp.GetPublicKeyString()
	defaultAuth := hivego.Auths{
		WeightThreshold: 1,
		KeyAuths:        [][2]any{{*pubKey, 1}},
		AccountAuths:    [][2]any{},
	}
	sysConf := systemconfig.FromNetwork(args.network)

	threshold := int(args.nodes * 2 / 3)
	bootnodes := []string{}
	p2pConfs := []p2p.P2PConfig{}
	gwAuths := [][2]any{}
	hiveOps := []hivego.HiveOperation{}

	// Init nodes
	for n := 1; n <= args.nodes; n++ {
		nodeDir := path.Join(args.dataDir, "data-"+strconv.Itoa(n))
		witnessName := args.witPrefix + strconv.Itoa(n)

		dbConf := db.NewDbConfig(nodeDir)
		p2pConf := p2p.NewConfig(nodeDir)
		gqlConf := gql.NewGqlConfig(nodeDir)
		hiveConf := streamer.NewHiveConfig(nodeDir)
		idConf := common.NewIdentityConfig(nodeDir)

		wits := witnesses.NewEmptyWitnesses()
		p2pServer := p2p.New(wits, p2pConf, idConf, sysConf, nil)

		plugins := aggregate.New([]aggregate.Plugin{dbConf, p2pConf, gqlConf, hiveConf, idConf, p2pServer})
		plugins.Init()

		hiveConf.SetHiveURIs(strings.Split(args.hiveUrl, ","))
		dbConf.SetDbURI(args.dbUrl)
		dbConf.SetDbName(args.dbPrefix + "-" + strconv.Itoa(n))
		p2pConf.SetOptions(p2p.P2POpts{
			Port:          args.p2pPort - 1 + n,
			ServerMode:    true,
			AllowPrivate:  true,
			Bootnodes:     []string{},
			AnnounceAddrs: []string{},
		})
		idConf.SetUsername(witnessName)
		idConf.SetActiveKey(args.wif)

		gwKey, _ := gateway.GatewayKeyFromBlsSeed(idConf.DefaultValue().BlsPrivKeySeed)

		bootnodes = append(bootnodes, p2pServer.GetPeerAddr().String()+"/p2p/"+p2pServer.GetPeerId())
		p2pConfs = append(p2pConfs, p2pConf)
		gwAuths = append(gwAuths, [2]any{*gwKey.GetPublicKeyString(), 1})
		hiveOps = append(hiveOps, hivego.AccountCreateOperation{
			Fee:            "0.000 TESTS",
			Creator:        args.witCreator,
			NewAccountName: witnessName,
			Owner:          defaultAuth,
			Active:         defaultAuth,
			Posting:        defaultAuth,
			MemoKey:        *pubKey,
			JsonMetadata:   "",
		})
	}

	// Set bootnodes
	for _, p := range p2pConfs {
		p.SetBootnodes(bootnodes)
	}

	// Create gateway and dao accounts
	hiveOps = append(hiveOps, hivego.AccountCreateOperation{
		Fee:            "0.000 TESTS",
		Creator:        args.witCreator,
		NewAccountName: sysConf.GatewayWallet(),
		Owner: hivego.Auths{
			WeightThreshold: threshold,
			KeyAuths:        gwAuths,
			AccountAuths:    [][2]any{},
		},
		Active: hivego.Auths{
			WeightThreshold: threshold,
			KeyAuths:        gwAuths,
			AccountAuths:    [][2]any{},
		},
		Posting: hivego.Auths{
			WeightThreshold: threshold,
			KeyAuths:        gwAuths,
			AccountAuths:    [][2]any{},
		},
		MemoKey:      *pubKey,
		JsonMetadata: "",
	}, hivego.AccountCreateOperation{
		Fee:            "0.000 TESTS",
		Creator:        args.witCreator,
		NewAccountName: params.DAO_WALLET,
		Owner:          defaultAuth,
		Active:         defaultAuth,
		Posting:        defaultAuth,
		MemoKey:        *pubKey,
		JsonMetadata:   "",
	}, hivego.AccountCreateOperation{
		Fee:            "0.000 TESTS",
		Creator:        args.witCreator,
		NewAccountName: "vsc.network",
		Owner:          defaultAuth,
		Active:         defaultAuth,
		Posting:        defaultAuth,
		MemoKey:        *pubKey,
		JsonMetadata:   "",
	})

	hiveClient := hivego.NewHiveRpc([]string{args.hiveUrl})
	hiveClient.ChainID = sysConf.HiveChainId()
	txId, err := hiveClient.Broadcast(hiveOps, &args.wif)
	if err != nil {
		fmt.Println("failed to broadcast account creation tx", err)
		os.Exit(1)
	}
	fmt.Println(txId)
	time.Sleep(3 * time.Second)

	// Deposit and stake
	hiveOps = []hivego.HiveOperation{}
	for n := 1; n <= args.nodes; n++ {
		witnessName := args.witPrefix + strconv.Itoa(n)

		stakeOp := state_engine.TxConsensusStake{
			From:   "hive:" + witnessName,
			To:     "hive:" + witnessName,
			Amount: args.stakeAmt,
			Asset:  string(ledger_db.AssetHive),
			NetId:  sysConf.NetId(),
		}
		stakeOpJson, _ := json.Marshal(stakeOp)

		hiveOps = append(hiveOps, hivego.TransferOperation{
			From:   args.witCreator,
			To:     sysConf.GatewayWallet(),
			Amount: args.stakeAmt + " TESTS",
			Memo:   "to=" + witnessName,
		}, hivego.CustomJsonOperation{
			Id:                   "vsc.consensus_stake",
			RequiredAuths:        []string{witnessName},
			RequiredPostingAuths: []string{},
			Json:                 string(stakeOpJson),
		})
	}

	txId, err = hiveClient.Broadcast(hiveOps, &args.wif)
	if err != nil {
		fmt.Println("failed to broadcast consensus stake tx", err)
		os.Exit(1)
	}
	fmt.Println(txId)

	os.Exit(0)
}
