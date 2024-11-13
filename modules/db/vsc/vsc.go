package vsc

import (
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/db"
)

type VscDb struct {
	*db.DbInstance
}

var _ a.Plugin = &VscDb{}

func New(d db.Db) *VscDb {
	return &VscDb{db.NewDbInstance(d, "go-vsc")}
}
