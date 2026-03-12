package gql

import (
	gqlgen "vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/gql/model"
)

const defaultLimit = 50

// limitCost returns base + (limit * childComplexity) for list queries.
// Uses the default limit (50) when no limit is provided.
func limitCost(base int, childComplexity int, limit *int) int {
	l := defaultLimit
	if limit != nil {
		l = *limit
	}
	return base + l*childComplexity
}

// NewComplexityRoot returns complexity cost functions for all GraphQL queries and fields.
// Fields without explicit costs default to 1 (gqlgen default).
func NewComplexityRoot() gqlgen.ComplexityRoot {
	c := gqlgen.ComplexityRoot{}

	// === Query-level costs ===

	// Simple lookups: cost 5
	c.Query.GetAccountBalance = func(childComplexity int, account string, height *model.Uint64) int {
		return 5 + childComplexity
	}
	c.Query.GetAccountRc = func(childComplexity int, account string, height *model.Uint64) int {
		return 5 + childComplexity
	}
	c.Query.GetAccountNonce = func(childComplexity int, account string) int {
		return 5 + childComplexity
	}
	c.Query.GetWitness = func(childComplexity int, account string, height *model.Uint64) int {
		return 5 + childComplexity
	}
	c.Query.WitnessStake = func(childComplexity int, account string) int {
		return 5 + childComplexity
	}
	c.Query.LocalNodeInfo = func(childComplexity int) int {
		return 5 + childComplexity
	}
	c.Query.GetElection = func(childComplexity int, epoch model.Uint64) int {
		return 5 + childComplexity
	}
	c.Query.ElectionByBlockHeight = func(childComplexity int, blockHeight *model.Uint64) int {
		return 5 + childComplexity
	}
	c.Query.GetTssKey = func(childComplexity int, keyID string) int {
		return 5 + childComplexity
	}
	c.Query.GetStateByKeys = func(childComplexity int, contractID string, keys []string) int {
		return 5 + childComplexity
	}
	c.Query.GetDagByCid = func(childComplexity int, cidString string) int {
		return 5 + childComplexity
	}

	// Mutation-like: cost 10
	c.Query.SubmitTransactionV1 = func(childComplexity int, tx string, sig string) int {
		return 10 + childComplexity
	}
	c.Query.SimulateContractCalls = func(childComplexity int, input gqlgen.SimulateContractCallsInput) int {
		return 10 + len(input.Calls)*childComplexity
	}

	// List/search queries: base cost 5, scaled by limit * childComplexity
	c.Query.FindTransaction = func(childComplexity int, filterOptions *gqlgen.TransactionFilter) int {
		var limit *int
		if filterOptions != nil {
			limit = filterOptions.Limit
		}
		return limitCost(5, childComplexity, limit)
	}
	c.Query.FindContractOutput = func(childComplexity int, filterOptions *gqlgen.ContractOutputFilter) int {
		var limit *int
		if filterOptions != nil {
			limit = filterOptions.Limit
		}
		return limitCost(5, childComplexity, limit)
	}
	c.Query.FindLedgerTXs = func(childComplexity int, filterOptions *gqlgen.LedgerTxFilter) int {
		var limit *int
		if filterOptions != nil {
			limit = filterOptions.Limit
		}
		return limitCost(5, childComplexity, limit)
	}
	c.Query.FindLedgerActions = func(childComplexity int, filterOptions *gqlgen.LedgerActionsFilter) int {
		var limit *int
		if filterOptions != nil {
			limit = filterOptions.Limit
		}
		return limitCost(5, childComplexity, limit)
	}
	c.Query.FindContract = func(childComplexity int, filterOptions *gqlgen.FindContractFilter) int {
		var limit *int
		if filterOptions != nil {
			limit = filterOptions.Limit
		}
		return limitCost(5, childComplexity, limit)
	}
	// Fixed-size list queries (no limit parameter): flat cost 50
	c.Query.WitnessNodes = func(childComplexity int, height model.Uint64) int {
		return 50 + childComplexity
	}
	c.Query.WitnessSchedule = func(childComplexity int, height model.Uint64) int {
		return 50 + childComplexity
	}
	c.Query.GetTssRequests = func(childComplexity int, keyID string, msgHex []string) int {
		return 50 + childComplexity
	}

	// === Nested list fields: cost 5 each ===

	c.TransactionRecord.Ops = func(childComplexity int) int {
		return 5 + childComplexity
	}
	c.TransactionRecord.Ledger = func(childComplexity int) int {
		return 5 + childComplexity
	}
	c.TransactionRecord.LedgerActions = func(childComplexity int) int {
		return 5 + childComplexity
	}
	c.TransactionRecord.Output = func(childComplexity int) int {
		return 5 + childComplexity
	}
	c.ElectionResult.Members = func(childComplexity int) int {
		return 5 + childComplexity
	}
	c.ContractOutput.Results = func(childComplexity int) int {
		return 5 + childComplexity
	}

	return c
}
