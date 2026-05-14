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
	"vsc-node/modules/db/vsc"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/e2e"
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
	if args.sysconfigPath != "" {
		if err := sysConf.LoadOverrides(args.sysconfigPath); err != nil {
			fmt.Println("Error loading sysconfig overrides:", err)
			os.Exit(1)
		}
	}

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

		// Set DB config BEFORE creating the DB plugin and calling Init(),
		// so the DB connects to the correct MongoDB endpoint (not localhost).
		dbConf.SetDbURI(args.dbUrl)
		dbConf.SetDbName(args.dbPrefix + "-" + strconv.Itoa(n))

		wits := witnesses.NewEmptyWitnesses()
		p2pServer := p2p.New(wits, p2pConf, idConf, sysConf, nil)

		plugins := []aggregate.Plugin{dbConf, p2pConf, gqlConf, hiveConf, idConf, p2pServer}

		if args.dropDb {
			d := db.New(dbConf)
			magiDb := vsc.New(d, dbConf)
			dbNuker := e2e.NewDbNuker(magiDb)
			plugins = append(plugins, d, magiDb, dbNuker)
		}

		a := aggregate.New(plugins)
		if err := a.Init(); err != nil {
			fmt.Printf("Error initializing node %d: %v\n", n, err)
			os.Exit(1)
		}

		host := strings.Replace(args.p2pHost, "?", strconv.Itoa(n), 1)
		port := args.p2pPort - 1 + n

		// Always need bootnodes to point at every magi node so the deployer can dial in.
		bootnodes = append(bootnodes, host+"/tcp/"+strconv.Itoa(port)+"/p2p/"+p2pServer.GetPeerId())

		// In -deployer-only mode, the magi nodes are already configured and running.
		// Skip rewriting their on-disk config and skip the account-create op.
		if args.deployerOnly {
			continue
		}

		hiveConf.SetHiveURIs(strings.Split(args.hiveUrl, ","))
		p2pConf.SetOptions(p2p.P2POpts{
			Port:          port,
			ServerMode:    true,
			AllowPrivate:  true,
			Bootnodes:     []string{},
			AnnounceAddrs: []string{},
		})
		idConf.SetUsername(witnessName)
		idConf.SetActiveKey(args.wif)

		gwKey, _ := gateway.GatewayKeyFromBlsSeed(idConf.Get().BlsPrivKeySeed)

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

	// Init contract-deployer identity. Lives alongside the magi node data dirs
	// in a `contract-deploy-1` subdir, dials the magi nodes as its bootnodes,
	// and uses the witCreator's WIF (matching how the magi accounts are
	// authenticated).
	deployerDir := path.Join(args.dataDir, "contract-deploy-1")
	depP2pConf := p2p.NewConfig(deployerDir)
	depHiveConf := streamer.NewHiveConfig(deployerDir)
	depIdConf := common.NewIdentityConfig(deployerDir)
	depWits := witnesses.NewEmptyWitnesses()
	depP2pServer := p2p.New(depWits, depP2pConf, depIdConf, sysConf, nil)

	depA := aggregate.New([]aggregate.Plugin{depP2pConf, depHiveConf, depIdConf, depP2pServer})
	if err := depA.Init(); err != nil {
		fmt.Printf("Error initializing contract-deployer: %v\n", err)
		os.Exit(1)
	}

	depHiveConf.SetHiveURIs(strings.Split(args.hiveUrl, ","))
	depP2pConf.SetOptions(p2p.P2POpts{
		Port:                   args.p2pPort + args.nodes,
		ServerMode:             false,
		AllowPrivate:           true,
		PubsubBufferSize:       512,
		PubsubConcurrencyLimit: 256,
		Bootnodes:              bootnodes,
		AnnounceAddrs:          []string{},
	})
	depIdConf.SetUsername(args.deployerName)
	depIdConf.SetActiveKey(args.wif)

	// Init feed-publisher config dir. The feed-publisher reads hiveConfig.json
	// and identityConfig.json from this directory; feedPublisherConfig.json is
	// written with defaults on first startup by the binary itself.
	fpDir := path.Join(args.dataDir, "feed-publisher")
	fpHiveConf := streamer.NewHiveConfig(fpDir)
	fpIdConf := common.NewIdentityConfig(fpDir)
	fpA := aggregate.New([]aggregate.Plugin{fpHiveConf, fpIdConf})
	if err := fpA.Init(); err != nil {
		fmt.Printf("Error initializing feed-publisher config: %v\n", err)
		os.Exit(1)
	}
	fpHiveConf.SetHiveURIs(strings.Split(args.hiveUrl, ","))
	fpIdConf.SetUsername(args.witCreator)
	fpIdConf.SetActiveKey(args.wif)

	hiveOps = append(hiveOps, hivego.AccountCreateOperation{
		Fee:            "0.000 TESTS",
		Creator:        args.witCreator,
		NewAccountName: args.deployerName,
		Owner:          defaultAuth,
		Active:         defaultAuth,
		Posting:        defaultAuth,
		MemoKey:        *pubKey,
		JsonMetadata:   "",
	})

	// Set bootnodes
	for _, p := range p2pConfs {
		p.SetBootnodes(bootnodes)
	}

	// Create gateway and dao accounts (skip when -deployer-only: they already exist).
	daoL1Acc := strings.Replace(params.DAO_WALLET, "hive:", "", 1)
	if !args.deployerOnly {
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
			NewAccountName: daoL1Acc,
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
	}

	hiveClient := hivego.NewHiveRpc([]string{args.hiveUrl})
	hiveClient.ChainID = sysConf.HiveChainId()
	txId, err := hiveClient.Broadcast(hiveOps, &args.wif)
	if err != nil {
		fmt.Println("failed to broadcast account creation tx", err)
		os.Exit(1)
	}
	fmt.Println(txId)
	time.Sleep(3 * time.Second)

	// Deposit and stake. Witness staking + gateway/dao HP grants run only on a
	// full setup; -deployer-only sends just the deployer's HP + TBD funding.
	hiveOps = []hivego.HiveOperation{}
	if !args.deployerOnly {
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
			}, hivego.TransferToVesting{
				From:   args.witCreator,
				To:     witnessName,
				Amount: "10.000 TESTS",
			})
		}
		hiveOps = append(hiveOps, hivego.TransferToVesting{
			From:   args.witCreator,
			To:     sysConf.GatewayWallet(),
			Amount: "10.000 TESTS",
		}, hivego.TransferToVesting{
			From:   args.witCreator,
			To:     daoL1Acc,
			Amount: "10.000 TESTS",
		}, hivego.TransferToVesting{
			From:   args.witCreator,
			To:     "vsc.network",
			Amount: "10.000 TESTS",
		})
	}
	hiveOps = append(hiveOps, hivego.TransferToVesting{
		From:   args.witCreator,
		To:     args.deployerName,
		Amount: args.deployerHpAmt + " TESTS",
	}, hivego.TransferOperation{
		From:   args.witCreator,
		To:     args.deployerName,
		Amount: args.deployerHbdAmt + " TBD",
		Memo:   "contract-deployer fees",
	})

	txId, err = hiveClient.Broadcast(hiveOps, &args.wif)
	if err != nil {
		fmt.Println("failed to broadcast consensus stake tx", err)
		os.Exit(1)
	}
	fmt.Println(txId)

	// Publish synthetic HIVE/HBD price feeds from the witness creator account
	// (initminer on devnet). The oracle's FeedTracker trusts feeds only from
	// accounts that have produced L1 blocks; on a HAF devnet initminer is the
	// sole Hive witness and satisfies that requirement immediately. Without
	// these feeds GeometryComputer.Compute returns HivePriceOK=false and swaps
	// fail with "pendulum snapshot unavailable". Feeds expire after ~100 blocks
	// (~5 min); re-run devnet-setup -deployer-only to refresh them.
	if !args.deployerOnly {
		hiveOps = []hivego.HiveOperation{
			feedPublishOperation{
				Publisher: args.witCreator,
				Base:      "0.250 TBD",
				Quote:     "1.000 TESTS",
			},
		}
		txId, err = hiveClient.Broadcast(hiveOps, &args.wif)
		if err != nil {
			fmt.Println("failed to broadcast feed_publish tx", err)
			os.Exit(1)
		}
		fmt.Println(txId)
	}

	os.Exit(0)
}
