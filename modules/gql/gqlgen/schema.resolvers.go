package gqlgen

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.
// Code generated by github.com/99designs/gqlgen version v0.17.55

import (
	"context"
	"fmt"
)

func (r *mutationResolver) IncrementNumber(ctx context.Context) (*TestResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) ContractStateDiff(ctx context.Context, id *string) (*ContractDiff, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) ContractState(ctx context.Context, id *string) (*ContractState, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) FindTransaction(ctx context.Context, filterOptions *FindTransactionFilter, decodedFilter *string) (*FindTransactionResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) FindContractOutput(ctx context.Context, filterOptions *FindContractOutputFilter, decodedFilter *string) (*FindContractOutputResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) FindLedgerTXs(ctx context.Context, filterOptions *LedgerTxFilter) (*LedgerResults, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) GetAccountBalance(ctx context.Context, account *string) (*GetBalanceResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) FindContract(ctx context.Context, id *string) (*FindContractResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) SubmitTransactionV1(ctx context.Context, tx string, sig string) (*TransactionSubmitResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) GetAccountNonce(ctx context.Context, keyGroup []*string) (*AccountNonceResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) LocalNodeInfo(ctx context.Context) (*LocalNodeInfo, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) WitnessNodes(ctx context.Context, height *int) ([]*WitnessNode, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) ActiveWitnessNodes(ctx context.Context) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) WitnessSchedule(ctx context.Context, height *int) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) NextWitnessSlot(ctx context.Context, self *bool) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) WitnessActiveScore(ctx context.Context, height *int) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) MockGenerateElection(ctx context.Context) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) AnchorProducer(ctx context.Context) (*AnchorProducer, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *queryResolver) GetCurrentNumber(ctx context.Context) (*TestResult, error) {
	panic(fmt.Errorf("not implemented"))
}

func (r *Resolver) Mutation() MutationResolver { return &mutationResolver{r} }

func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
