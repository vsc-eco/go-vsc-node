package gqlgen

import "vsc-node/modules/db/vsc/witnesses"

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Witnesses witnesses.Witnesses
}
