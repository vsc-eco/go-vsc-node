package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	cbortypes "vsc-node/lib/cbor-types"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"
	p2pInterface "vsc-node/modules/p2p"

	"github.com/vsc-eco/hivego"
)

func main() {
	cbortypes.RegisterTypes()
	args, err := ParseArgs()
	if err != nil {
		fmt.Println("Error parsing arguments:", err)
		os.Exit(1)
	}
	dbConf := db.NewDbConfig(args.dataDir)
	p2pConf := p2pInterface.NewConfig(args.dataDir)
	hiveConf := streamer.NewHiveConfig(args.dataDir)

	fmt.Println("Network:", args.network)

	dbImpl := db.New(dbConf)
	vscDb := vsc.New(dbImpl, args.dbName)
	hiveBlocks, _ := hive_blocks.New(vscDb)
	witnessDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	sysConfig := systemconfig.FromNetwork(args.network)
	identityConfig := common.NewIdentityConfig(args.dataDir)

	p2p := p2pInterface.New(witnessDb, p2pConf, identityConfig, sysConfig, nil)
	da := datalayer.New(p2p, args.dataDir)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		//Configuration init
		dbConf,
		hiveConf,
		p2pConf,
		identityConfig,

		//DB plugin initialization
		dbImpl,
		vscDb,
		//DB collections
		hiveBlocks,
		electionDb,
		witnessDb,

		p2p,
		da, //Deps: [p2p]
	)

	a := aggregate.New(
		plugins,
	)
	initErr := a.Init()
	if initErr != nil {
		fmt.Println("failed to init", initErr)
		os.Exit(1)
	}
	a.Start()

	existing := electionDb.GetElection(0)
	if existing != nil {
		fmt.Println("the genesis election already exists")
		os.Exit(1)
	}

	hiveRpcClient := hivego.NewHiveRpc([]string{hiveConf.Get().HiveURI})
	hiveRpcClient.ChainID = sysConfig.HiveChainId()

	head, _ := hiveBlocks.GetLastProcessedBlock()
	wits, _ := witnessDb.GetWitnessesAtBlockHeight(head)
	members := []elections.ElectionMember{}
	weights := []uint64{}

	for _, w := range wits {
		k, e := w.ConsensusKey()
		if e == nil {
			members = append(members, elections.ElectionMember{Key: k.String(), Account: w.Account})
			weights = append(weights, 1)
		}
	}
	if len(members) == 0 {
		panic("No members found")
	}
	electionData := elections.ElectionData{
		ElectionCommonInfo: elections.ElectionCommonInfo{
			Epoch: 0,
			NetId: sysConfig.NetId(),
			Type:  "initial",
		},
		ElectionDataInfo: elections.ElectionDataInfo{
			Weights:         weights,
			Members:         members,
			ProtocolVersion: 0,
		},
	}
	cid, err := da.PutObject(electionData)
	if err != nil {
		panic(err)
	}

	electionHeader := map[string]interface{}{
		"epoch":  0,
		"data":   cid.String(),
		"net_id": sysConfig.NetId(),
	}
	bbyes, _ := json.Marshal(electionHeader)
	wif := identityConfig.Get().HiveActiveKey
	electionOp := hivego.CustomJsonOperation{
		RequiredAuths:        []string{identityConfig.Get().HiveUsername},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.election_result",
		Json:                 string(bbyes),
	}
	txid, err := hiveRpcClient.Broadcast([]hivego.HiveOperation{electionOp}, &wif)
	if err != nil {
		fmt.Println("failed to broadcast election tx", err)
		os.Exit(1)
	} else {
		fmt.Println("tx id:", txid)
	}

	seconds := 60
	fmt.Println("wait", seconds, "seconds for peers to retrieve dag...")

	time.Sleep(time.Duration(seconds) * time.Second)

	aErr := a.Stop()
	if aErr != nil {
		fmt.Println("error is", aErr)
		os.Exit(1)
	}
}
