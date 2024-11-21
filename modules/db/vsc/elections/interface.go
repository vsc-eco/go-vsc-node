package elections

import a "vsc-node/modules/aggregate"

type Elections interface {
	a.Plugin
	StoreElection(elecResult ElectionResult)
	GetElection(epoch int) *ElectionResult
	GetElectionByHeight(height int) *ElectionResult
}
