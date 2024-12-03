import (
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
)

type ledger struct {
	Balances *db.Collection
	History  *db.Collection
}

func New(d *vsc.VscDb) Ledger {
	return &contracts{db.NewCollection(d.DbInstance, "contracts")}
}
