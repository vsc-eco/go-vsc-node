package data_availability_test

import (
	"fmt"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	data_availability_client "vsc-node/modules/data-availability/client"
	data_availability_server "vsc-node/modules/data-availability/server"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/multiformats/go-multiaddr"
)

type Node struct {
	*aggregate.Aggregate
	client *data_availability_client.DataAvailability
	db     *vsc.VscDb
}

func (n Node) Client() bool {
	return n.client != nil
}

func (n *Node) RequestProof(data []byte) (stateEngine.StorageProof, error) {
	return n.client.RequestProof(data)
}

func (n *Node) NukeDb() error {
	return n.db.Nuke()
}

type MakeNodeInput struct {
	Username string
	Client   bool
}

func MakeNode(input MakeNodeInput) *Node {
	input.Username = "e2e-" + input.Username
	dbConf := db.NewDbConfig()
	db := db.New(dbConf)
	vscDb := vsc.New(db, input.Username)
	witnessesDb := witnesses.New(vscDb)

	// logger := logger.PrefixedLogger{
	// 	Prefix: input.Username,
	// }

	identityConfig := common.NewIdentityConfig("data-" + input.Username + "/config")

	identityConfig.Init()
	identityConfig.SetUsername(input.Username)

	p2p := p2pInterface.New(witnessesDb, identityConfig, 0)

	datalayer := DataLayer.New(p2p, input.Username)

	/*
			for _, addr := range node.P2P.Host.Addrs() {
			peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.P2P.Host.ID().String())
		}
	*/
	// id := p2p.Host.Network().Peers()[0]
	// p2p.Host.Network().Connectedness(id).String()
	p2p.Init()
	addrs := utils.Map(
		p2p.PeerInfo().GetPeerAddrs(),
		func(addr multiaddr.Multiaddr) string {
			b, _ := addr.MarshalText()
			fmt.Println(string(b))
			return string(b) + "/p2p/" + p2p.PeerInfo().GetPeerId()
		},
	)
	libp2p.BOOTSTRAP = append(libp2p.BOOTSTRAP, addrs...)

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
	}
}
