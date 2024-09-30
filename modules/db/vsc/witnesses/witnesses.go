package witnesses

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
)

type witnesses struct {
	*db.Collection
}

func New(d *vsc.VscDb) Witnesses {
	return &witnesses{db.NewCollection(d.DbInstance, "witnesses")}
}

// StoreNodeAnnouncement implements Witnesses.
func (w *witnesses) StoreNodeAnnouncement(nodeId string) error {
	_, err := w.InsertOne(context.Background(), Witness{NodeId: nodeId})
	return err
}
