package gql

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// Pentest finding F10: gqlgen's FixedComplexityLimit caps the
// total complexity score, but cheap leaf fields like __typename
// or localNodeInfo have a per-field complexity of 1, so an
// attacker can pack 1000+ aliases of those into a single request
// without ever tripping the complexity limit. Each alias is a
// separate execution.
//
// AliasLimit caps the total field-selection count per operation
// (top-level + nested) to bound that amplification independent
// of complexity. The default of DefaultAliasLimit covers
// every legitimate use we have observed against the production
// schema; raise via NewAliasLimit() if a future client needs
// genuinely larger aggregations.
//
// Pentest finding W-C1 (fragment depth DoS): the original
// countSelections walked fragment spreads without counting the
// spread itself and without memoization. A chain of N empty
// fragments (fragment F1 { ...F2 } ... fragment FN { __typename })
// yielded a count of 1 and bypassed the cap entirely, while still
// forcing gqlparser/gqlgen to walk all N fragments at execute
// time. A branching pattern (fragment FN { ...FN+1 ...FN+1 })
// also blew up countSelections itself to O(2^N). The fixes:
//
//   1. Cap the number of fragment definitions per document
//      (DefaultMaxFragmentDefinitions). Legitimate queries use a
//      handful of fragments; thousands are always abusive.
//   2. Count each fragment spread as one selection so chained
//      empty fragments contribute to the alias cap.
//   3. Memoize per-fragment-name counts so a fragment body is
//      walked at most once during counting, making the count
//      O(F + S) instead of O(2^F) for branching graphs.
//   4. Guard the memo against fragment cycles defensively even
//      though gqlparser's validator should reject them.

const (
	DefaultAliasLimit             = 100
	DefaultMaxFragmentDefinitions = 50
)

type aliasLimit struct {
	max int
}

// NewAliasLimit returns a gqlgen extension that rejects an
// operation when the total number of field selections (any
// nesting level, fragments resolved) exceeds max.
func NewAliasLimit(max int) graphql.HandlerExtension {
	return aliasLimit{max: max}
}

var _ interface {
	graphql.OperationContextMutator
	graphql.HandlerExtension
} = aliasLimit{}

func (aliasLimit) ExtensionName() string                     { return "AliasLimit" }
func (aliasLimit) Validate(_ graphql.ExecutableSchema) error { return nil }
func (a aliasLimit) MutateOperationContext(_ context.Context, opCtx *graphql.OperationContext) *gqlerror.Error {
	if n := len(opCtx.Doc.Fragments); n > DefaultMaxFragmentDefinitions {
		return gqlerror.Errorf(
			"operation has %d fragment definitions, exceeding the limit of %d",
			n, DefaultMaxFragmentDefinitions,
		)
	}
	op := opCtx.Doc.Operations.ForName(opCtx.OperationName)
	if op == nil {
		return nil
	}
	memo := make(map[string]int, len(opCtx.Doc.Fragments))
	count := countSelections(op.SelectionSet, opCtx.Doc.Fragments, memo)
	if count > a.max {
		return gqlerror.Errorf(
			"operation has %d field selections, exceeding the alias / field-selection limit of %d",
			count, a.max,
		)
	}
	return nil
}

// countSelections walks an operation's selection tree and returns
// the total number of selections encountered. Fragment spreads
// expand to the fragment's selection set; inline fragments inline
// their own selection set. Each spread itself counts as one
// selection so a chain of empty fragments still contributes to
// the cap. memo caches the per-fragment-body count so a fragment
// referenced from many places is walked only once.
func countSelections(set ast.SelectionSet, fragments ast.FragmentDefinitionList, memo map[string]int) int {
	n := 0
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			n++
			n += countSelections(s.SelectionSet, fragments, memo)
		case *ast.InlineFragment:
			n += countSelections(s.SelectionSet, fragments, memo)
		case *ast.FragmentSpread:
			n++
			if c, ok := memo[s.Name]; ok {
				n += c
				continue
			}
			def := fragments.ForName(s.Name)
			if def == nil {
				continue
			}
			// Defensive cycle guard: seed the memo with 0 before
			// recursing so a cyclic reference (which gqlparser
			// should already reject) cannot re-enter.
			memo[s.Name] = 0
			inner := countSelections(def.SelectionSet, fragments, memo)
			memo[s.Name] = inner
			n += inner
		}
	}
	return n
}
