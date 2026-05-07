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

const DefaultAliasLimit = 100

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

func (aliasLimit) ExtensionName() string                          { return "AliasLimit" }
func (aliasLimit) Validate(_ graphql.ExecutableSchema) error      { return nil }
func (a aliasLimit) MutateOperationContext(_ context.Context, opCtx *graphql.OperationContext) *gqlerror.Error {
	op := opCtx.Doc.Operations.ForName(opCtx.OperationName)
	if op == nil {
		return nil
	}
	count := countSelections(op.SelectionSet, opCtx.Doc.Fragments)
	if count > a.max {
		return gqlerror.Errorf(
			"operation has %d field selections, exceeding the alias / field-selection limit of %d",
			count, a.max,
		)
	}
	return nil
}

// countSelections walks an operation's selection tree and returns
// the total number of fields encountered. Fragment spreads expand
// to the fragment's selection set; inline fragments inline their
// own selection set.
func countSelections(set ast.SelectionSet, fragments ast.FragmentDefinitionList) int {
	n := 0
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			n++
			n += countSelections(s.SelectionSet, fragments)
		case *ast.InlineFragment:
			n += countSelections(s.SelectionSet, fragments)
		case *ast.FragmentSpread:
			if def := fragments.ForName(s.Name); def != nil {
				n += countSelections(def.SelectionSet, fragments)
			}
		}
	}
	return n
}
