package elections

import a "vsc-node/modules/aggregate"

type Elections interface {
	a.Plugin
	StoreElection(elecResult ElectionResult) error
	GetElection(epoch uint64) *ElectionResult
	GetElectionByHeight(height uint64) (ElectionResult, error)
}
