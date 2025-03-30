package gqlgen

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.
// Code generated by github.com/99designs/gqlgen version v0.17.68

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/gql/model"
	transactionpool "vsc-node/modules/transaction-pool"
)

// AnchoredBlock is the resolver for the anchored_block field.
func (r *contractOutputResolver) AnchoredBlock(ctx context.Context, obj *contracts.ContractOutput) (*string, error) {
	panic(fmt.Errorf("not implemented: AnchoredBlock - anchored_block"))
}

// AnchoredHeight is the resolver for the anchored_height field.
func (r *contractOutputResolver) AnchoredHeight(ctx context.Context, obj *contracts.ContractOutput) (*int, error) {
	panic(fmt.Errorf("not implemented: AnchoredHeight - anchored_height"))
}

// AnchoredID is the resolver for the anchored_id field.
func (r *contractOutputResolver) AnchoredID(ctx context.Context, obj *contracts.ContractOutput) (*string, error) {
	panic(fmt.Errorf("not implemented: AnchoredID - anchored_id"))
}

// AnchoredIndex is the resolver for the anchored_index field.
func (r *contractOutputResolver) AnchoredIndex(ctx context.Context, obj *contracts.ContractOutput) (*int, error) {
	panic(fmt.Errorf("not implemented: AnchoredIndex - anchored_index"))
}

// Gas is the resolver for the gas field.
func (r *contractOutputResolver) Gas(ctx context.Context, obj *contracts.ContractOutput) (*Gas, error) {
	panic(fmt.Errorf("not implemented: Gas - gas"))
}

// Results is the resolver for the results field.
func (r *contractOutputResolver) Results(ctx context.Context, obj *contracts.ContractOutput) ([]*string, error) {
	panic(fmt.Errorf("not implemented: Results - results"))
}

// SideEffects is the resolver for the side_effects field.
func (r *contractOutputResolver) SideEffects(ctx context.Context, obj *contracts.ContractOutput) (*string, error) {
	panic(fmt.Errorf("not implemented: SideEffects - side_effects"))
}

// ID is the resolver for the id field.
func (r *contractStateResolver) ID(ctx context.Context, obj contracts.ContractState) (*string, error) {
	panic(fmt.Errorf("not implemented: ID - id"))
}

// State is the resolver for the state field.
func (r *contractStateResolver) State(ctx context.Context, obj contracts.ContractState, key *string) (*string, error) {
	panic(fmt.Errorf("not implemented: State - state"))
}

// StateQuery is the resolver for the stateQuery field.
func (r *contractStateResolver) StateQuery(ctx context.Context, obj contracts.ContractState, key *string, query *string) (*string, error) {
	panic(fmt.Errorf("not implemented: StateQuery - stateQuery"))
}

// StateKeys is the resolver for the stateKeys field.
func (r *contractStateResolver) StateKeys(ctx context.Context, obj contracts.ContractState, key *string) (*string, error) {
	panic(fmt.Errorf("not implemented: StateKeys - stateKeys"))
}

// StateMerkle is the resolver for the state_merkle field.
func (r *contractStateResolver) StateMerkle(ctx context.Context, obj contracts.ContractState) (*string, error) {
	panic(fmt.Errorf("not implemented: StateMerkle - state_merkle"))
}

// IncrementNumber is the resolver for the incrementNumber field.
func (r *mutationResolver) IncrementNumber(ctx context.Context) (*TestResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// ContractStateDiff is the resolver for the contractStateDiff field.
func (r *queryResolver) ContractStateDiff(ctx context.Context, id *string) (*ContractDiff, error) {
	panic(fmt.Errorf("not implemented"))
}

// ContractState is the resolver for the contractState field.
func (r *queryResolver) ContractState(ctx context.Context, id *string) (contracts.ContractState, error) {
	panic(fmt.Errorf("not implemented"))
}

// FindTransaction is the resolver for the findTransaction field.
func (r *queryResolver) FindTransaction(ctx context.Context, filterOptions *FindTransactionFilter, decodedFilter *string) (*FindTransactionResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// FindContractOutput is the resolver for the findContractOutput field.
func (r *queryResolver) FindContractOutput(ctx context.Context, filterOptions *FindContractOutputFilter, decodedFilter *string) (*FindContractOutputResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// FindLedgerTXs is the resolver for the findLedgerTXs field.
func (r *queryResolver) FindLedgerTXs(ctx context.Context, filterOptions *LedgerTxFilter) (*LedgerResults, error) {
	panic(fmt.Errorf("not implemented"))
}

// GetAccountBalance is the resolver for the getAccountBalance field.
func (r *queryResolver) GetAccountBalance(ctx context.Context, account *string) (*GetBalanceResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// FindContract is the resolver for the findContract field.
func (r *queryResolver) FindContract(ctx context.Context, id *string) (*FindContractResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// SubmitTransactionV1 is the resolver for the submitTransactionV1 field.
func (r *queryResolver) SubmitTransactionV1(ctx context.Context, tx string, sig string) (*TransactionSubmitResult, error) {
	Tx, err := base64.URLEncoding.DecodeString(tx)
	if err != nil {
		return nil, err
	}
	Sig, err := base64.URLEncoding.DecodeString(sig)
	if err != nil {
		return nil, err
	}
	cid, err := r.TxPool.IngestTx(transactionpool.SerializedVSCTransaction{
		Tx,
		Sig,
	})
	if err != nil {
		return nil, err
	}
	id := cid.String()
	return &TransactionSubmitResult{
		ID: &id,
	}, nil
}

// GetAccountNonce is the resolver for the getAccountNonce field.
func (r *queryResolver) GetAccountNonce(ctx context.Context, keyGroup []*string) (*AccountNonceResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// LocalNodeInfo is the resolver for the localNodeInfo field.
func (r *queryResolver) LocalNodeInfo(ctx context.Context) (*LocalNodeInfo, error) {
	panic(fmt.Errorf("not implemented"))
}

// WitnessNodes is the resolver for the witnessNodes field.
func (r *queryResolver) WitnessNodes(ctx context.Context, height model.Uint64) ([]witnesses.Witness, error) {
	return r.Witnesses.GetWitnessesAtBlockHeight(uint64(height))
}

// ActiveWitnessNodes is the resolver for the activeWitnessNodes field.
func (r *queryResolver) ActiveWitnessNodes(ctx context.Context) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

// WitnessSchedule is the resolver for the witnessSchedule field.
func (r *queryResolver) WitnessSchedule(ctx context.Context, height model.Uint64) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

// NextWitnessSlot is the resolver for the nextWitnessSlot field.
func (r *queryResolver) NextWitnessSlot(ctx context.Context, self *bool) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

// WitnessActiveScore is the resolver for the witnessActiveScore field.
func (r *queryResolver) WitnessActiveScore(ctx context.Context, height *int) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

// MockGenerateElection is the resolver for the mockGenerateElection field.
func (r *queryResolver) MockGenerateElection(ctx context.Context) (*string, error) {
	panic(fmt.Errorf("not implemented"))
}

// AnchorProducer is the resolver for the anchorProducer field.
func (r *queryResolver) AnchorProducer(ctx context.Context) (*AnchorProducer, error) {
	panic(fmt.Errorf("not implemented"))
}

// GetCurrentNumber is the resolver for the getCurrentNumber field.
func (r *queryResolver) GetCurrentNumber(ctx context.Context) (*TestResult, error) {
	panic(fmt.Errorf("not implemented"))
}

// WitnessStake is the resolver for the witnessStake field.
func (r *queryResolver) WitnessStake(ctx context.Context, account string) (model.Uint64, error) {
	res, err := r.Balances.GetBalanceRecord(account, uint64(math.MaxUint64))
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return model.Uint64(res.HIVE_CONSENSUS), nil
}

// IpfsPeerID is the resolver for the ipfs_peer_id field.
func (r *witnessResolver) IpfsPeerID(ctx context.Context, obj *witnesses.Witness) (*string, error) {
	panic(fmt.Errorf("not implemented: IpfsPeerID - ipfs_peer_id"))
}

// LastSigned is the resolver for the last_signed field.
func (r *witnessResolver) LastSigned(ctx context.Context, obj *witnesses.Witness) (*int, error) {
	panic(fmt.Errorf("not implemented: LastSigned - last_signed"))
}

// SigningKeys is the resolver for the signing_keys field.
func (r *witnessResolver) SigningKeys(ctx context.Context, obj *witnesses.Witness) (*HiveKeys, error) {
	panic(fmt.Errorf("not implemented: SigningKeys - signing_keys"))
}

// ContractOutput returns ContractOutputResolver implementation.
func (r *Resolver) ContractOutput() ContractOutputResolver { return &contractOutputResolver{r} }

// ContractState returns ContractStateResolver implementation.
func (r *Resolver) ContractState() ContractStateResolver { return &contractStateResolver{r} }

// Mutation returns MutationResolver implementation.
func (r *Resolver) Mutation() MutationResolver { return &mutationResolver{r} }

// Query returns QueryResolver implementation.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

// Witness returns WitnessResolver implementation.
func (r *Resolver) Witness() WitnessResolver { return &witnessResolver{r} }

type contractOutputResolver struct{ *Resolver }
type contractStateResolver struct{ *Resolver }
type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
type witnessResolver struct{ *Resolver }

// !!! WARNING !!!
// The code below was going to be deleted when updating resolvers. It has been copied here so you have
// one last chance to move it out of harms way if you want. There are two reasons this happens:
//  - When renaming or deleting a resolver the old code will be put in here. You can safely delete
//    it when you're done.
//  - You have helper methods in this file. Move them out to keep these resolver files clean.
/*
	func (r *queryResolver) Stake(ctx context.Context, account string) (model.Uint64, error) {
	panic(fmt.Errorf("not implemented: Stake - stake"))
}
*/
