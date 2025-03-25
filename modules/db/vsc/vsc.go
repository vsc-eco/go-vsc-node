package vsc

import (
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/db"
)

type VscDb struct {
	*db.DbInstance
}

var _ a.Plugin = &VscDb{}

func New(d db.Db, suffix ...string) *VscDb {
	var dbPath string
	if len(suffix) > 0 {
		dbPath = "go-vsc-" + suffix[0]
	} else {
		dbPath = "go-vsc"
	}

	return &VscDb{db.NewDbInstance(d, dbPath)}
}
