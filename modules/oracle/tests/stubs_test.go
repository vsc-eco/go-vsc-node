package tests

import (
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"
)

var (
	p2pSrv     *libp2p.P2PServer
	conf       common.IdentityConfig
	electionDb elections.Elections
	witnessDb  witnesses.Witnesses
)
