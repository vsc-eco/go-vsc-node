package elections

import a "vsc-node/modules/aggregate"

type Elections interface {
	a.Plugin
	StoreElection(elecResult ElectionResult)
	GetElection(epoch uint64) *ElectionResult
	GetElectionByHeight(height uint64) *ElectionResult
}
