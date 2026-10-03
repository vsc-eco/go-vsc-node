package elections

import a "vsc-node/modules/aggregate"

type Elections interface {
	a.Plugin
	StoreElection(elecResult ElectionResult) error
	GetElection(epoch uint64) *ElectionResult
	// GetElectionStrict is GetElection but SURFACES the FindOne error rather than
	// swallowing it to nil, so a consensus-critical caller (#11 bond-lock) can
	// fail-STOP on a transient per-node infra error rather than fail-open (→ fork).
	// mongo.ErrNoDocuments = deterministic absence; any other error is transient.
	GetElectionStrict(epoch uint64) (*ElectionResult, error)
	GetPreviousElections(beforeEpoch uint64, limit int) []ElectionResult
	GetElectionByHeight(height uint64) (ElectionResult, error)
}
