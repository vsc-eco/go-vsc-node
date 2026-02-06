package data_availability_test

import (
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	data_availability_client "vsc-node/modules/data-availability/client"
	data_availability_server "vsc-node/modules/data-availability/server"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
)

type Node struct {
	*aggregate.Aggregate
	client         *data_availability_client.DataAvailability
	db             *vsc.VscDb
	identityConfig common.IdentityConfig
	p2p            *p2pInterface.P2PServer
}

func (n Node) Client() bool {
	return n.client != nil
}

func (n *Node) RequestProof(data []byte) (stateEngine.StorageProof, error) {
	return n.client.RequestProof("http://localhost:7080/api/v1/graphql", data)
}

func (n *Node) NukeDb() error {
	return n.db.Nuke()
}

func (n *Node) ConsensusKey() string {
	did, err := n.identityConfig.BlsDID()
	if err != nil {
		panic(err)
	}

	return did.String()
}

type MakeNodeInput struct {
	Username string
	Client   bool
}

var nodeCount = 0

func MakeNode(input MakeNodeInput) *Node {
	input.Username = "e2e-" + input.Username
	dataDir := "data-" + input.Username
	dbConf := db.NewDbConfig(dataDir)
	identityConfig := common.NewIdentityConfig(dataDir)
	p2pConfig := p2pInterface.NewConfig(dataDir)
	aggregate.New([]aggregate.Plugin{dbConf, identityConfig, p2pConfig}).Init()
	dbConf.SetDbName("go-vsc-" + input.Username)
	identityConfig.SetUsername(input.Username)
	p2pConfig.SetOptions(p2pInterface.P2POpts{
		Port:         7001 + nodeCount,
		ServerMode:   !input.Client,
		AllowPrivate: true,
		Bootnodes:    []string{},
	})

	db := db.New(dbConf)
	vscDb := vsc.New(db, dbConf)
	witnessesDb := witnesses.New(vscDb)

	nodeCount++

	sysConfig := systemconfig.MocknetConfig()

	p2p := p2pInterface.New(witnessesDb, p2pConfig, identityConfig, sysConfig, nil)

	datalayer := DataLayer.New(p2p, dataDir)

	// key, err := identityConfig.Libp2pPrivateKey()
	// if err != nil {
	// 	panic(err)
	// }
	// peerId, err := peer.IDFromPrivateKey(key)
	// if err != nil {
	// 	panic(err)
	// }
	// libp2p.BOOTSTRAP = append(libp2p.BOOTSTRAP, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", port, peerId.String()))

	var da aggregate.Plugin
	if input.Client {
		da = data_availability_client.New(p2p, identityConfig, datalayer)
	} else {
		da = data_availability_server.New(p2p, identityConfig, datalayer)
	}

	plugins := []aggregate.Plugin{
		dbConf,
		db,
		identityConfig,
		vscDb,
		witnessesDb,
		p2p,
		datalayer,
		da,
	}

	client, _ := da.(*data_availability_client.DataAvailability)

	return &Node{
		aggregate.New(plugins),
		client,
		vscDb,
		identityConfig,
		p2p,
	}
}
