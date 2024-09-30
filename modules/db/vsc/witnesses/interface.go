package witnesses

import a "vsc-node/modules/aggregate"

type Witnesses interface {
	a.Plugin
	StoreNodeAnnouncement(nodeId string) error
}
