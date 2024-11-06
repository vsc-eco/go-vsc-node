package manager

import (
	"vsc-node/modules/protocol"
)

type manager struct {
	protocols []protocol.Protocol
	version   uint
}

var Manager = manager{
	protocols: []protocol.Protocol{
		protocol.V1,
		protocol.V2,
	},
}

func (m *manager) SetVersion(version uint) {
	m.version = version
}
