package oraclee2e

import (
	"vsc-node/lib/datalayer"
	"vsc-node/lib/logger"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/e2e"
	"vsc-node/modules/oracle"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"
	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"
)

type Node struct {
	oracle  *oracle.Oracle
	plugins []aggregate.Plugin
}

func MakeNode(nodeName string) *Node {
	dbConf := db.NewDbConfig()
	db := db.New(dbConf)
	vscDb := vsc.New(db, nodeName)
	witnessesDb := witnesses.New(vscDb)
	electionDb := elections.New(vscDb)
	contractDb := contracts.New(vscDb)
	contractState := contracts.NewContractState(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	hiveBlocks := &e2e.MockHiveDbs{}
	vscBlocks := vscBlocks.New(vscDb)
	actionsDb := ledgerDb.NewActionsDb(vscDb)
	rcDb := rcDb.New(vscDb)
	nonceDb := nonces.New(vscDb)
	wasm := wasm_parent_ipc.New()
	sysConfig := common.SystemConfig{Network: "mocknet"}
	identityConfig := common.NewIdentityConfig(
		"oracle-test_" + nodeName + "/config",
	)
	p2p := p2pInterface.New(witnessesDb, identityConfig, sysConfig, 0)
	logger := logger.PrefixedLogger{
		Prefix: "oracle-test_" + nodeName,
	}
	dataLayer := datalayer.New(p2p, nodeName)
	se := stateEngine.New(
		logger, dataLayer, witnessesDb, electionDb, contractDb, contractState,
		txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks,
		actionsDb, rcDb, nonceDb, wasm,
	)
	vstream := vstream.New(se)

	oracle := oracle.New(
		p2p, identityConfig, electionDb, witnessesDb, vstream, se,
	)

	plugins := []aggregate.Plugin{
		dbConf, db, identityConfig, vscDb, e2e.NewDbNuker(vscDb), witnessesDb,
		p2p, dataLayer, electionDb, contractDb, hiveBlocks, vscBlocks, txDb,
		ledgerDbImpl, actionsDb, balanceDb, interestClaims, contractState, rcDb,
		nonceDb, vstream, wasm, se,
	}

	out := Node{oracle, plugins}
	return &out
}
